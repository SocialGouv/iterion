package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/audit"
	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
)

// The rank of the TARGET: no roster write reaches a member ranked above its
// author. The writes that name one member — the org roster's PATCH and
// DELETE, and a team grant's PATCH, DELETE and PUT — all take the same guard
// (requireRankOverMember), which reads both ranks from their membership ROWS.
//
// Every refusal below is asserted on the ROW the store holds afterwards, not
// on the answer alone: a guard that answered 403 and wrote anyway would pass
// a test that read the response.

const (
	teamOwnerID  = "teamowner"
	secondAdmin  = "orgadmin2"
	configEdName = "cfgeditor"
)

func seedOrgRow(t *testing.T, s *Server, userID, orgID string, role identity.OrgRole) {
	t.Helper()
	if err := s.authStore().UpsertOrgMembership(context.Background(), identity.OrgMembership{
		UserID: userID, OrgID: orgID, Role: role, JoinedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
}

func seedTeamGrant(t *testing.T, s *Server, userID, teamID string, role identity.Role) {
	t.Helper()
	if err := s.authStore().UpsertMembership(context.Background(), identity.Membership{
		UserID: userID, TeamID: teamID, Role: role, JoinedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
}

// orgRowRole reads a member's org role from the store — the only witness that
// a refused write left the row alone.
func orgRowRole(t *testing.T, s *Server, userID, orgID string) (identity.OrgRole, bool) {
	t.Helper()
	om, err := s.authStore().GetOrgMembership(context.Background(), userID, orgID)
	switch {
	case errors.Is(err, identity.ErrNotFound):
		return "", false
	case err != nil:
		t.Fatalf("read the org row of %s in %s: %v", userID, orgID, err)
	}
	return om.Role, true
}

func teamGrantRole(t *testing.T, s *Server, userID, teamID string) (identity.Role, bool) {
	t.Helper()
	mb, err := s.authStore().GetMembership(context.Background(), userID, teamID)
	switch {
	case errors.Is(err, identity.ErrNotFound):
		return "", false
	case err != nil:
		t.Fatalf("read the grant of %s in %s: %v", userID, teamID, err)
	}
	return mb.Role, true
}

func removeOrgMemberAs(s *Server, ctx context.Context, orgID, memberID string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	s.handleRemoveOrgMember(w, teamReq(ctx, "DELETE", "/api/orgs/"+orgID+"/members/"+memberID, "", orgID, memberID))
	return w
}

func patchTeamRoleAs(s *Server, ctx context.Context, teamID, memberID string, role identity.Role) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	s.handleUpdateMember(w, teamReq(ctx, "PATCH", "/api/teams/"+teamID+"/members/"+memberID,
		`{"role":"`+string(role)+`"}`, teamID, memberID))
	return w
}

func removeTeamMemberAs(s *Server, ctx context.Context, teamID, memberID string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	s.handleRemoveMember(w, teamReq(ctx, "DELETE", "/api/teams/"+teamID+"/members/"+memberID, "", teamID, memberID))
	return w
}

func putTeamMemberAs(s *Server, ctx context.Context, teamID, memberID string, role identity.Role) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	s.handlePutTeamMember(w, teamReq(ctx, "PUT", "/api/teams/"+teamID+"/members/"+memberID,
		`{"role":"`+string(role)+`"}`, teamID, memberID))
	return w
}

func wantBodyNames(t *testing.T, body string, clauses ...string) {
	t.Helper()
	for _, c := range clauses {
		if !strings.Contains(body, c) {
			t.Errorf("the refusal does not name %q: %s", c, body)
		}
	}
}

// The org ladder on the org roster's own writes: an org admin reaches no row
// of an org OWNER, in either direction, and the row stays exactly as it was.
// A removal is asserted on the owner's team grant too — the cascade's first
// pass revokes grants before the membership row is touched, so a guard that
// ran after it would leave the owner stripped of their access.
func TestMemberRank_anOrgAdminDoesNotReachAnOrgOwnersRow(t *testing.T) {
	for _, c := range []struct {
		name string
		act  func(*Server) *httptest.ResponseRecorder
		want []string
	}{
		{
			name: "demoting them",
			act: func(s *Server) *httptest.ResponseRecorder {
				return patchOrgRole(s, orgAdminCtx(), "o1", "orgowner", identity.OrgRoleMember)
			},
			want: []string{
				"does not reach a member whose org rank the writer's own does not cover",
				"the org role of orgowner in this org is owner",
				"your own is admin",
				"a member of this org who already holds owner writes it, or a platform admin",
			},
		},
		{
			name: "removing them",
			act: func(s *Server) *httptest.ResponseRecorder {
				return removeOrgMemberAs(s, orgAdminCtx(), "o1", "orgowner")
			},
			want: []string{
				"the org role of orgowner in this org is owner",
				"a member of this org who already holds owner writes it, or a platform admin",
			},
		},
		{
			name: "promoting them",
			act: func(s *Server) *httptest.ResponseRecorder {
				return patchOrgRole(s, orgAdminCtx(), "o1", "orgowner", identity.OrgRoleAdmin)
			},
			want: []string{"the org role of orgowner in this org is owner"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newOrgCredsTestServer(t)
			seedOrgOwner(t, s)
			seedTeamGrant(t, s, "orgowner", "t1", identity.RoleOwner)

			w := c.act(s)
			if w.Code != http.StatusForbidden {
				t.Fatalf("an org admin %s the org owner: code=%d body=%s, want 403", c.name, w.Code, w.Body.String())
			}
			wantBodyNames(t, w.Body.String(), c.want...)
			if role, ok := orgRowRole(t, s, "orgowner", "o1"); !ok || role != identity.OrgRoleOwner {
				t.Errorf("the refused write moved the owner's row: role=%q present=%v", role, ok)
			}
			if role, ok := teamGrantRole(t, s, "orgowner", "t1"); !ok || role != identity.RoleOwner {
				t.Errorf("the refused write reached the owner's team grant: role=%q present=%v", role, ok)
			}
		})
	}
}

// What the rank of the target does not take away, on the org roster: every
// rank at or below the writer's own, their own row, and a platform admin's
// reach over an owner (IsSuperAdmin is the escape hatch, and they hold no org
// row for a ladder to place them on).
func TestMemberRank_theOrgRosterWritesThatStillGoThrough(t *testing.T) {
	for _, c := range []struct {
		name   string
		seed   func(*testing.T, *Server) context.Context
		target string
		// role is the org role written; "" removes the member instead.
		role identity.OrgRole
	}{
		{"an org owner demotes an admin", seedOrgOwner, "orgadmin", identity.OrgRoleMember},
		{"an org owner removes an admin", seedOrgOwner, "orgadmin", ""},
		{"an org admin demotes another admin", func(t *testing.T, s *Server) context.Context {
			seedOrgRow(t, s, secondAdmin, "o1", identity.OrgRoleAdmin)
			return orgAdminCtx()
		}, secondAdmin, identity.OrgRoleMember},
		{"an org admin removes another admin", func(t *testing.T, s *Server) context.Context {
			seedOrgRow(t, s, secondAdmin, "o1", identity.OrgRoleAdmin)
			return orgAdminCtx()
		}, secondAdmin, ""},
		{"an org admin promotes a member", func(*testing.T, *Server) context.Context { return orgAdminCtx() }, racerID, identity.OrgRoleAdmin},
		{"an org admin removes a member", func(*testing.T, *Server) context.Context { return orgAdminCtx() }, racerID, ""},
		{"an org admin demotes their own row", func(*testing.T, *Server) context.Context { return orgAdminCtx() }, "orgadmin", identity.OrgRoleMember},
		{"an org admin removes their own row", func(*testing.T, *Server) context.Context { return orgAdminCtx() }, "orgadmin", ""},
		{"a platform admin demotes an owner", func(t *testing.T, s *Server) context.Context {
			seedOrgOwner(t, s)
			return superAdminCtx()
		}, "orgowner", identity.OrgRoleMember},
		{"a platform admin removes an owner", func(t *testing.T, s *Server) context.Context {
			seedOrgOwner(t, s)
			return superAdminCtx()
		}, "orgowner", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newOrgCredsTestServer(t)
			seedRacer(t, s)
			ctx := c.seed(t, s)

			if c.role == "" {
				w := removeOrgMemberAs(s, ctx, "o1", c.target)
				if w.Code != http.StatusNoContent {
					t.Fatalf("code=%d body=%s, want 204", w.Code, w.Body.String())
				}
				if role, ok := orgRowRole(t, s, c.target, "o1"); ok {
					t.Errorf("the removal left the row at %q", role)
				}
				return
			}
			w := patchOrgRole(s, ctx, "o1", c.target, c.role)
			if w.Code != http.StatusOK {
				t.Fatalf("code=%d body=%s, want 200", w.Code, w.Body.String())
			}
			if role, ok := orgRowRole(t, s, c.target, "o1"); !ok || role != c.role {
				t.Errorf("the row carries %q (present=%v), want %q", role, ok, c.role)
			}
		})
	}
}

// The org ladder binds a TEAM-scope write too: a team admin whose own org
// standing is `member` writes no grant of an org ADMIN, whichever of the
// three writes they make. The grant is the org admin's access to the team,
// and the rank that decides is the ORG one — the team ladder would place a
// team admin above a team member and let it through.
//
// The remedy such a refusal names is PERFORMED here. A team route is gated on
// canManageTeam, which inside an org only its admins and owners pass, so a
// plain member of the org holding the target's rank — whom the org roster's
// own refusals name — reaches no team of it: the body names an admin or owner
// of the organization instead, and one makes the same write.
func TestMemberRank_aTeamAdminDoesNotReachTheGrantOfAnOrgAdmin(t *testing.T) {
	for _, c := range []struct {
		name  string
		act   func(*Server, context.Context) *httptest.ResponseRecorder
		after func(*testing.T, *Server)
	}{
		{"demoting it", func(s *Server, ctx context.Context) *httptest.ResponseRecorder {
			return patchTeamRoleAs(s, ctx, "t1", "orgadmin", identity.RoleViewer)
		}, func(t *testing.T, s *Server) {
			if role, ok := teamGrantRole(t, s, "orgadmin", "t1"); !ok || role != identity.RoleViewer {
				t.Errorf("the remedy the refusal names did not write the grant: role=%q present=%v", role, ok)
			}
		}},
		{"removing it", func(s *Server, ctx context.Context) *httptest.ResponseRecorder {
			return removeTeamMemberAs(s, ctx, "t1", "orgadmin")
		}, func(t *testing.T, s *Server) {
			if role, ok := teamGrantRole(t, s, "orgadmin", "t1"); ok {
				t.Errorf("the remedy the refusal names left the grant at %q", role)
			}
		}},
		{"placing it", func(s *Server, ctx context.Context) *httptest.ResponseRecorder {
			return putTeamMemberAs(s, ctx, "t1", "orgadmin", identity.RoleViewer)
		}, func(t *testing.T, s *Server) {
			if role, ok := teamGrantRole(t, s, "orgadmin", "t1"); !ok || role != identity.RoleViewer {
				t.Errorf("the remedy the refusal names did not write the grant: role=%q present=%v", role, ok)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newOrgCredsTestServer(t)
			seedRacerAsTeamAdmin(t, s)
			seedAccount(t, s, "orgadmin", "orgadmin@example.com")
			seedTeamGrant(t, s, "orgadmin", "t1", identity.RoleMember)

			w := c.act(s, racerCtx())
			if w.Code != http.StatusForbidden {
				t.Fatalf("a team admin %s of an org admin: code=%d body=%s, want 403", c.name, w.Code, w.Body.String())
			}
			wantBodyNames(t, w.Body.String(),
				"does not reach a member whose org rank the writer's own does not cover",
				"the org role of orgadmin in this org is admin",
				"your own is member",
				"an admin or owner of this organization writes it, or a platform admin")
			wantBodyLacks(t, w.Body.String(), "a member of this org who already holds admin")
			if role, ok := teamGrantRole(t, s, "orgadmin", "t1"); !ok || role != identity.RoleMember {
				t.Errorf("the refused write moved the grant: role=%q present=%v", role, ok)
			}

			seedAccount(t, s, secondAdmin, "orgadmin2@example.com")
			seedOrgRow(t, s, secondAdmin, "o1", identity.OrgRoleAdmin)
			remedy := c.act(s, memberCtx(secondAdmin))
			if remedy.Code != http.StatusOK && remedy.Code != http.StatusNoContent {
				t.Fatalf("an admin of the organization %s the same grant: code=%d body=%s, want 200 or 204",
					c.name, remedy.Code, remedy.Body.String())
			}
			c.after(t, s)
		})
	}
}

// The team ladder, where the writer's authority over the team is their own
// GRANT: a team admin reaches no grant of a team OWNER, even when the org
// ladder has nothing to say (both are plain members of the org).
func TestMemberRank_aTeamAdminDoesNotReachATeamOwnersGrant(t *testing.T) {
	for _, c := range []struct {
		name string
		act  func(*Server) *httptest.ResponseRecorder
	}{
		{"demoting it", func(s *Server) *httptest.ResponseRecorder {
			return patchTeamRoleAs(s, racerCtx(), "t1", teamOwnerID, identity.RoleViewer)
		}},
		{"removing it", func(s *Server) *httptest.ResponseRecorder {
			return removeTeamMemberAs(s, racerCtx(), "t1", teamOwnerID)
		}},
		{"placing it", func(s *Server) *httptest.ResponseRecorder {
			return putTeamMemberAs(s, racerCtx(), "t1", teamOwnerID, identity.RoleViewer)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newOrgCredsTestServer(t)
			seedRacerAsTeamAdmin(t, s)
			seedAccount(t, s, teamOwnerID, "teamowner@example.com")
			seedOrgRow(t, s, teamOwnerID, "o1", identity.OrgRoleMember)
			seedTeamGrant(t, s, teamOwnerID, "t1", identity.RoleOwner)

			w := c.act(s)
			if w.Code != http.StatusForbidden {
				t.Fatalf("a team admin %s of a team owner: code=%d body=%s, want 403", c.name, w.Code, w.Body.String())
			}
			wantBodyNames(t, w.Body.String(),
				"is not written over a member whose team rank the writer's own does not cover",
				"the role of teamowner in this team is owner",
				"your own is admin",
				"a member of this team who already holds owner writes it, an admin or owner of the team's organization, or a platform admin")
			if role, ok := teamGrantRole(t, s, teamOwnerID, "t1"); !ok || role != identity.RoleOwner {
				t.Errorf("the refused write moved the grant: role=%q present=%v", role, ok)
			}
		})
	}
}

// racerCtx is the team admin the team-scope witnesses write as: a t1 admin
// who is a plain member of o1 (seedRacerAsTeamAdmin).
func racerCtx() context.Context {
	return memberCtx(racerID)
}

// What the two team-ladder rules — the rank of the target, and the rank
// WRITTEN — do not take away on a team grant: an org admin manages every team
// of their org and answers to the ORG ladder alone, so a team OWNER's grant is
// theirs to write and so is handing one out; a team admin keeps the ranks at
// or below their own, writing their own rank included, the capability off the
// ladder (config_editor, ADR-078, which no rank measures), their own grant,
// and a member with no grant at all; a team owner hands out an owner; and a
// platform admin keeps everything.
func TestMemberRank_theTeamGrantWritesThatStillGoThrough(t *testing.T) {
	for _, c := range []struct {
		name   string
		seed   func(*testing.T, *Server) context.Context
		target string
		// role is the team role written; "" removes the grant instead.
		role identity.Role
	}{
		{"an org admin demotes a team owner", seedTeamOwner, teamOwnerID, identity.RoleViewer},
		{"an org admin removes a team owner", seedTeamOwner, teamOwnerID, ""},
		{"a platform admin demotes a team owner", func(t *testing.T, s *Server) context.Context {
			seedTeamOwner(t, s)
			return superAdminCtx()
		}, teamOwnerID, identity.RoleViewer},
		{"a team admin demotes a team member", func(t *testing.T, s *Server) context.Context {
			seedRacerAsTeamAdmin(t, s)
			seedAccount(t, s, "plain", "plain@example.com")
			seedOrgRow(t, s, "plain", "o1", identity.OrgRoleMember)
			seedTeamGrant(t, s, "plain", "t1", identity.RoleMember)
			return racerCtx()
		}, "plain", identity.RoleViewer},
		{"a team admin removes a team member", func(t *testing.T, s *Server) context.Context {
			seedRacerAsTeamAdmin(t, s)
			seedAccount(t, s, "plain", "plain@example.com")
			seedOrgRow(t, s, "plain", "o1", identity.OrgRoleMember)
			seedTeamGrant(t, s, "plain", "t1", identity.RoleMember)
			return racerCtx()
		}, "plain", ""},
		{"a team admin demotes another team admin", func(t *testing.T, s *Server) context.Context {
			seedRacerAsTeamAdmin(t, s)
			seedAccount(t, s, "peer", "peer@example.com")
			seedOrgRow(t, s, "peer", "o1", identity.OrgRoleMember)
			seedTeamGrant(t, s, "peer", "t1", identity.RoleAdmin)
			return racerCtx()
		}, "peer", identity.RoleMember},
		{"a team admin writes over a config editor", func(t *testing.T, s *Server) context.Context {
			seedRacerAsTeamAdmin(t, s)
			seedAccount(t, s, configEdName, "cfgeditor@example.com")
			seedOrgRow(t, s, configEdName, "o1", identity.OrgRoleMember)
			seedTeamGrant(t, s, configEdName, "t1", identity.RoleConfigEditor)
			return racerCtx()
		}, configEdName, identity.RoleViewer},
		{"a team admin removes a config editor", func(t *testing.T, s *Server) context.Context {
			seedRacerAsTeamAdmin(t, s)
			seedAccount(t, s, configEdName, "cfgeditor@example.com")
			seedOrgRow(t, s, configEdName, "o1", identity.OrgRoleMember)
			seedTeamGrant(t, s, configEdName, "t1", identity.RoleConfigEditor)
			return racerCtx()
		}, configEdName, ""},
		{"a team admin writes their own grant", func(t *testing.T, s *Server) context.Context {
			seedRacerAsTeamAdmin(t, s)
			return racerCtx()
		}, racerID, identity.RoleMember},
		{"a team admin writes their own rank on a member", func(t *testing.T, s *Server) context.Context {
			seedRacerAsTeamAdmin(t, s)
			seedAccount(t, s, "plain", "plain@example.com")
			seedOrgRow(t, s, "plain", "o1", identity.OrgRoleMember)
			seedTeamGrant(t, s, "plain", "t1", identity.RoleMember)
			return racerCtx()
		}, "plain", identity.RoleAdmin},
		{"a team admin writes the capability off the ladder", func(t *testing.T, s *Server) context.Context {
			seedRacerAsTeamAdmin(t, s)
			seedAccount(t, s, "plain", "plain@example.com")
			seedOrgRow(t, s, "plain", "o1", identity.OrgRoleMember)
			seedTeamGrant(t, s, "plain", "t1", identity.RoleMember)
			return racerCtx()
		}, "plain", identity.RoleConfigEditor},
		{"a team owner writes an owner", func(t *testing.T, s *Server) context.Context {
			seedAccount(t, s, teamOwnerID, "teamowner@example.com")
			seedOrgRow(t, s, teamOwnerID, "o1", identity.OrgRoleMember)
			seedTeamGrant(t, s, teamOwnerID, "t1", identity.RoleOwner)
			seedRacer(t, s)
			seedTeamGrant(t, s, racerID, "t1", identity.RoleMember)
			return memberCtx(teamOwnerID)
		}, racerID, identity.RoleOwner},
		{"an org admin writes an owner", func(t *testing.T, s *Server) context.Context {
			seedRacer(t, s)
			seedTeamGrant(t, s, racerID, "t1", identity.RoleMember)
			return orgAdminCtx()
		}, racerID, identity.RoleOwner},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newOrgCredsTestServer(t)
			ctx := c.seed(t, s)

			if c.role == "" {
				w := removeTeamMemberAs(s, ctx, "t1", c.target)
				if w.Code != http.StatusNoContent {
					t.Fatalf("code=%d body=%s, want 204", w.Code, w.Body.String())
				}
				if role, ok := teamGrantRole(t, s, c.target, "t1"); ok {
					t.Errorf("the removal left the grant at %q", role)
				}
				return
			}
			w := patchTeamRoleAs(s, ctx, "t1", c.target, c.role)
			if w.Code != http.StatusOK {
				t.Fatalf("code=%d body=%s, want 200", w.Code, w.Body.String())
			}
			if role, ok := teamGrantRole(t, s, c.target, "t1"); !ok || role != c.role {
				t.Errorf("the grant carries %q (present=%v), want %q", role, ok, c.role)
			}
		})
	}
}

// seedTeamOwner seeds a t1 OWNER who is a plain member of o1 — the grant an
// org admin still writes — and answers the org admin's context.
func seedTeamOwner(t *testing.T, s *Server) context.Context {
	t.Helper()
	seedAccount(t, s, teamOwnerID, "teamowner@example.com")
	seedOrgRow(t, s, teamOwnerID, "o1", identity.OrgRoleMember)
	seedTeamGrant(t, s, teamOwnerID, "t1", identity.RoleOwner)
	return orgAdminCtx()
}

// A placement that creates a grant where there was none has no team rank to
// clear, and the team admin who makes it keeps it — the ladder measures the
// grant the target HOLDS, and none ranks below every rung.
func TestMemberRank_aTeamAdminStillPlacesAMemberWithNoGrant(t *testing.T) {
	s := newOrgCredsTestServer(t)
	seedRacerAsTeamAdmin(t, s)
	seedAccount(t, s, "newcomer", "newcomer@example.com")
	seedOrgRow(t, s, "newcomer", "o1", identity.OrgRoleMember)

	w := putTeamMemberAs(s, racerCtx(), "t1", "newcomer", identity.RoleMember)
	if w.Code != http.StatusOK {
		t.Fatalf("placing a member with no grant: code=%d body=%s, want 200", w.Code, w.Body.String())
	}
	if role, ok := teamGrantRole(t, s, "newcomer", "t1"); !ok || role != identity.RoleMember {
		t.Errorf("the placement wrote %q (present=%v), want member", role, ok)
	}
}

// A team with no parent org has no org ladder to read: the team ladder is the
// whole rule there, and it holds — a team admin reaches no team owner's grant
// in a legacy team either.
func TestMemberRank_aTeamWithNoOrgKeepsTheTeamLadder(t *testing.T) {
	s := newOrgCredsTestServer(t)
	ctx := context.Background()
	if _, err := s.authStore().CreateTeam(ctx, identity.Team{
		ID: "t-legacy", Name: "legacy", Slug: "legacy", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	seedTeamGrant(t, s, racerID, "t-legacy", identity.RoleAdmin)
	seedTeamGrant(t, s, teamOwnerID, "t-legacy", identity.RoleOwner)

	w := removeTeamMemberAs(s, racerCtx(), "t-legacy", teamOwnerID)
	if w.Code != http.StatusForbidden {
		t.Fatalf("a team admin removing the owner of an org-less team: code=%d body=%s, want 403", w.Code, w.Body.String())
	}
	// And the refusal offers no org remedy, there being no organization to
	// hold one: the members of the team and a platform admin are the whole
	// list.
	wantBodyNames(t, w.Body.String(), "the role of teamowner in this team is owner", "your own is admin",
		"a member of this team who already holds owner writes it, or a platform admin")
	wantBodyLacks(t, w.Body.String(), "organization")
	if role, ok := teamGrantRole(t, s, teamOwnerID, "t-legacy"); !ok || role != identity.RoleOwner {
		t.Errorf("the refused removal moved the grant: role=%q present=%v", role, ok)
	}
}

// targetRankErrorGate fails the read of ONE account's org row — the target's,
// which the guard reads after the writer's own — so a rank that cannot be
// read is exercised where it matters: a guard that let an unreadable rank
// through would write over whatever the row holds.
type targetRankErrorGate struct {
	identity.Store
	userID, orgID string
	err           error
}

func (g *targetRankErrorGate) GetOrgMembership(ctx context.Context, userID, orgID string) (identity.OrgMembership, error) {
	if userID == g.userID && orgID == g.orgID {
		return identity.OrgMembership{}, g.err
	}
	return g.Store.GetOrgMembership(ctx, userID, orgID)
}

// A rank that cannot be READ refuses the write with the store's own error,
// naming the read it could not make — never a write taken unranked.
func TestMemberRank_aRankThatCannotBeReadRefusesTheWrite(t *testing.T) {
	s := newOrgCredsTestServer(t)
	seedOrgOwner(t, s)
	seedTeamGrant(t, s, "orgowner", "t1", identity.RoleOwner)
	real := s.authStore()
	s.authSvc.SetStoreForTest(&targetRankErrorGate{
		Store: real, userID: "orgowner", orgID: "o1", err: errors.New("org row unavailable"),
	})
	defer s.authSvc.SetStoreForTest(real)

	w := removeOrgMemberAs(s, orgAdminCtx(), "o1", "orgowner")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("a removal whose target's rank could not be read: code=%d body=%s, want 500", w.Code, w.Body.String())
	}
	wantBodyNames(t, w.Body.String(), "resolve the org membership of orgowner", "org row unavailable")
	if om, err := real.GetOrgMembership(context.Background(), "orgowner", "o1"); err != nil || om.Role != identity.OrgRoleOwner {
		t.Errorf("the refused removal moved the row: %+v (%v)", om, err)
	}
	if mb, err := real.GetMembership(context.Background(), "orgowner", "t1"); err != nil || mb.Role != identity.RoleOwner {
		t.Errorf("the refused removal reached the grant: %+v (%v)", mb, err)
	}
}

// unrankableRowGate answers one account's org row with a role this build
// cannot place on its ladder — a rung added to identity.OrgRole by another
// build, or a hand-written row.
type unrankableRowGate struct {
	identity.Store
	userID, orgID string
	role          identity.OrgRole
}

func (g *unrankableRowGate) GetOrgMembership(ctx context.Context, userID, orgID string) (identity.OrgMembership, error) {
	om, err := g.Store.GetOrgMembership(ctx, userID, orgID)
	if err == nil && userID == g.userID && orgID == g.orgID {
		om.Role = g.role
	}
	return om, err
}

// A rank this build cannot PLACE is not demonstrably below the writer's own,
// so the write is refused rather than taken: AtLeast ranks an unknown role at
// zero, which no comparison covers. No standing covers it either, so the
// refusal names the one remedy that works, a platform admin — who still
// reaches the row. The role written differs from the one the row holds on
// both halves, and the store is read through the real store afterwards: a
// write that landed behind the 403, or one the platform admin did not make,
// is what the row says.
func TestMemberRank_aRankThisBuildCannotPlaceIsNotWrittenOver(t *testing.T) {
	s := newOrgCredsTestServer(t)
	ctx := context.Background()
	seedRacer(t, s)
	real := s.authStore()
	if om, err := real.GetOrgMembership(ctx, racerID, "o1"); err != nil || om.Role != identity.OrgRoleMember {
		t.Fatalf("the row this write would move is %+v (%v), want a plain member — the role written below must differ from it", om, err)
	}
	s.authSvc.SetStoreForTest(&unrankableRowGate{Store: real, userID: racerID, orgID: "o1", role: "archon"})
	defer s.authSvc.SetStoreForTest(real)

	w := patchOrgRole(s, orgAdminCtx(), "o1", racerID, identity.OrgRoleAdmin)
	if w.Code != http.StatusForbidden {
		t.Fatalf("writing over a rank this build cannot place: code=%d body=%s, want 403", w.Code, w.Body.String())
	}
	wantBodyNames(t, w.Body.String(), "the org role of racer in this org is archon", "your own is admin",
		"a platform admin writes it")
	wantBodyLacks(t, w.Body.String(), "a member of this org who already holds archon")
	if om, err := real.GetOrgMembership(ctx, racerID, "o1"); err != nil || om.Role != identity.OrgRoleMember {
		t.Errorf("the refused write moved the row: %+v (%v)", om, err)
	}

	super := patchOrgRole(s, superAdminCtx(), "o1", racerID, identity.OrgRoleAdmin)
	if super.Code != http.StatusOK {
		t.Fatalf("a platform admin over the same row: code=%d body=%s, want 200", super.Code, super.Body.String())
	}
	if om, err := real.GetOrgMembership(ctx, racerID, "o1"); err != nil || om.Role != identity.OrgRoleAdmin {
		t.Errorf("the platform admin's write did not land: %+v (%v)", om, err)
	}
}

// memberCtx answers one account's identity, for a witness whose author is
// named by the scenario rather than by a fixture — the remedy a refusal names
// is performed by one of these.
func memberCtx(userID string) context.Context {
	return auth.WithIdentity(context.Background(), auth.Identity{UserID: userID})
}

// wantBodyLacks keeps a refusal from naming what it must not: a remedy that
// does not work, or a rank its reader cannot read.
func wantBodyLacks(t *testing.T, body string, clauses ...string) {
	t.Helper()
	for _, c := range clauses {
		if strings.Contains(body, c) {
			t.Errorf("the refusal names %q: %s", c, body)
		}
	}
}

// The cap on the rank WRITTEN, which is what makes the team ladder a boundary
// rather than two requests of delay: a writer whose authority over the team
// is their own GRANT writes no role above it — on any row, their own
// included. So the self-promotion that would clear the team ladder is refused
// first, and the writes it would have been the premise of still are.
func TestMemberRank_aTeamAdminWritesNoRankAboveTheirOwnGrant(t *testing.T) {
	for _, c := range []struct {
		name    string
		promote func(*Server) *httptest.ResponseRecorder
	}{
		{"through the patch", func(s *Server) *httptest.ResponseRecorder {
			return patchTeamRoleAs(s, racerCtx(), "t1", racerID, identity.RoleOwner)
		}},
		{"through the put", func(s *Server) *httptest.ResponseRecorder {
			return putTeamMemberAs(s, racerCtx(), "t1", racerID, identity.RoleOwner)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newOrgCredsTestServer(t)
			seedRacerAsTeamAdmin(t, s)
			seedAccount(t, s, teamOwnerID, "teamowner@example.com")
			seedOrgRow(t, s, teamOwnerID, "o1", identity.OrgRoleMember)
			seedTeamGrant(t, s, teamOwnerID, "t1", identity.RoleOwner)

			w := c.promote(s)
			if w.Code != http.StatusForbidden {
				t.Fatalf("a team admin writing owner on their own grant: code=%d body=%s, want 403", w.Code, w.Body.String())
			}
			wantBodyNames(t, w.Body.String(),
				"a team role is not written above the writer's own grant",
				"writing the role owner in this team takes that grant",
				"your own is admin",
				"a member of this team who already holds owner writes it, an admin or owner of the team's organization, or a platform admin")
			if role, ok := teamGrantRole(t, s, racerID, "t1"); !ok || role != identity.RoleAdmin {
				t.Errorf("the refused write raised their own grant: role=%q present=%v", role, ok)
			}
			// The two writes that promotion was the premise of: the team
			// owner's grant is still theirs, demotion and removal alike.
			demote := patchTeamRoleAs(s, racerCtx(), "t1", teamOwnerID, identity.RoleViewer)
			if demote.Code != http.StatusForbidden {
				t.Fatalf("the demotion the promotion was the premise of: code=%d body=%s, want 403", demote.Code, demote.Body.String())
			}
			drop := removeTeamMemberAs(s, racerCtx(), "t1", teamOwnerID)
			if drop.Code != http.StatusForbidden {
				t.Fatalf("the removal the promotion was the premise of: code=%d body=%s, want 403", drop.Code, drop.Body.String())
			}
			if role, ok := teamGrantRole(t, s, teamOwnerID, "t1"); !ok || role != identity.RoleOwner {
				t.Errorf("a refused write moved the owner's grant: role=%q present=%v", role, ok)
			}
		})
	}
}

// The repair path the off-boarding's own error bodies send an operator back
// into: a removal re-issued for a member whose org row is already gone and
// whose team grants are not. A target with no row on the org ladder stands
// below every rung of it, so the re-issue answers success and finishes what
// the first pass left — a guard that compared the absent row would refuse it
// and leave the grants standing.
func TestMemberRank_theReIssuedRemovalOfAMemberWithNoOrgRowGoesThrough(t *testing.T) {
	s := newOrgCredsTestServer(t)
	ctx := context.Background()
	seedRacer(t, s)
	seedTeamGrant(t, s, racerID, "t1", identity.RoleMember)
	if err := s.authStore().DeleteOrgMembership(ctx, racerID, "o1"); err != nil {
		t.Fatal(err)
	}
	if role, ok := orgRowRole(t, s, racerID, "o1"); ok {
		t.Fatalf("the fixture still holds an org row (%q) — then it is not the half-off-boarded state this re-issue repairs", role)
	}

	w := removeOrgMemberAs(s, orgAdminCtx(), "o1", racerID)
	if w.Code != http.StatusNoContent {
		t.Fatalf("re-issuing the removal of a member whose org row is already gone: code=%d body=%s, want 204",
			w.Code, w.Body.String())
	}
	if role, ok := teamGrantRole(t, s, racerID, "t1"); ok {
		t.Errorf("the re-issued removal left the grant at %q — then it did not finish the off-boarding", role)
	}
}

// An author who holds no org row at all — the orphan grant
// GET /api/admin/users/{id} reports as drift — stands below every rung of the
// org ladder, so the ladder refuses them every target that HAS a row. Their
// refusal names no rank of the target's: the org roster that rank is read
// from answers that author 403 (canViewOrg). The remedy it names instead is
// performed here.
func TestMemberRank_anAuthorWithNoOrgRowIsRefusedWithoutNamingTheTargetsRank(t *testing.T) {
	s := newOrgCredsTestServer(t)
	seedRacer(t, s)
	seedTeamGrant(t, s, racerID, "t1", identity.RoleMember)
	if role, ok := orgRowRole(t, s, "teamadmin", "o1"); ok {
		t.Fatalf("the author holds an org row (%q) — then this is not the row-less author the ladder places below every rung", role)
	}

	w := patchTeamRoleAs(s, teamAdminCtx(), "t1", racerID, identity.RoleViewer)
	if w.Code != http.StatusForbidden {
		t.Fatalf("an author with no org row writing over a member who has one: code=%d body=%s, want 403", w.Code, w.Body.String())
	}
	wantBodyNames(t, w.Body.String(),
		"the org role of racer in this org stands above your own, which is none",
		"an owner of this organization writes it, or a platform admin")
	wantBodyLacks(t, w.Body.String(), "the org role of racer in this org is")
	if role, ok := teamGrantRole(t, s, racerID, "t1"); !ok || role != identity.RoleMember {
		t.Errorf("the refused write moved the grant: role=%q present=%v", role, ok)
	}
	// The remedy, performed: an owner of the organization makes that write.
	ownerCtx := seedOrgOwner(t, s)
	again := patchTeamRoleAs(s, ownerCtx, "t1", racerID, identity.RoleViewer)
	if again.Code != http.StatusOK {
		t.Fatalf("the remedy the refusal names: code=%d body=%s, want 200", again.Code, again.Body.String())
	}
	if role, ok := teamGrantRole(t, s, racerID, "t1"); !ok || role != identity.RoleViewer {
		t.Errorf("the remedy the refusal names did not write the grant: role=%q present=%v", role, ok)
	}
}

// A team removal of a grant that is not THERE writes over no rank: it takes
// nothing away, so no ladder is read and the route keeps its own answer — the
// idempotent 204 of a removal already done — on the very input where the org
// ladder would otherwise refuse this author. The answer is measured beside
// it, through a platform admin, whom the guard exempts. The role change keeps
// its 404 instead: it reads the grant it would write back, before any rank.
func TestMemberRank_aTeamWriteOverAMemberWithNoGrantKeepsItsAnswer(t *testing.T) {
	s := newOrgCredsTestServer(t)
	seedRacerAsTeamAdmin(t, s)
	seedAccount(t, s, "orgadmin", "orgadmin@example.com")
	if role, ok := teamGrantRole(t, s, "orgadmin", "t1"); ok {
		t.Fatalf("the target holds a grant (%q) — then this is not the no-grant input", role)
	}
	if role, ok := orgRowRole(t, s, "orgadmin", "o1"); !ok || role != identity.OrgRoleAdmin {
		t.Fatalf("the target is not an org admin (%q, present=%v) — then the org ladder has nothing to say here", role, ok)
	}

	base := removeTeamMemberAs(s, superAdminCtx(), "t1", "orgadmin")
	if base.Code != http.StatusNoContent {
		t.Fatalf("the answer this route gives on a grant that is not there: code=%d body=%s, want 204", base.Code, base.Body.String())
	}
	drop := removeTeamMemberAs(s, racerCtx(), "t1", "orgadmin")
	if drop.Code != base.Code {
		t.Fatalf("a team admin removing a grant that is not there: code=%d body=%s, want %d — the answer the route gives without a rank to compare",
			drop.Code, drop.Body.String(), base.Code)
	}
	patch := patchTeamRoleAs(s, racerCtx(), "t1", "orgadmin", identity.RoleViewer)
	if patch.Code != http.StatusNotFound {
		t.Fatalf("a role change on a grant that is not there: code=%d body=%s, want 404", patch.Code, patch.Body.String())
	}
	if role, ok := teamGrantRole(t, s, "orgadmin", "t1"); ok {
		t.Errorf("a write over a member with no grant created one at %q", role)
	}
}

// unrankableGrantGate answers one account's grant in one team with a role
// this build cannot place on the team ladder — a rung added to identity.Role
// by another build, or a hand-written row.
type unrankableGrantGate struct {
	identity.Store
	userID, teamID string
	role           identity.Role
}

func (g *unrankableGrantGate) GetMembership(ctx context.Context, userID, teamID string) (identity.Membership, error) {
	mb, err := g.Store.GetMembership(ctx, userID, teamID)
	if err == nil && userID == g.userID && teamID == g.teamID {
		mb.Role = g.role
	}
	return mb, err
}

// The team ladder's half of the same rule, with the remedies that work there:
// a grant this build cannot place is covered by no grant of the team, so the
// refusal names the org's admins and owners and a platform admin — and the
// platform admin reaches the row, which the store is read for on both halves.
func TestMemberRank_aTeamRankThisBuildCannotPlaceIsNotWrittenOver(t *testing.T) {
	s := newOrgCredsTestServer(t)
	ctx := context.Background()
	seedRacerAsTeamAdmin(t, s)
	seedAccount(t, s, teamOwnerID, "teamowner@example.com")
	seedOrgRow(t, s, teamOwnerID, "o1", identity.OrgRoleMember)
	seedTeamGrant(t, s, teamOwnerID, "t1", identity.RoleOwner)
	real := s.authStore()
	s.authSvc.SetStoreForTest(&unrankableGrantGate{Store: real, userID: teamOwnerID, teamID: "t1", role: "archon"})
	defer s.authSvc.SetStoreForTest(real)

	w := removeTeamMemberAs(s, racerCtx(), "t1", teamOwnerID)
	if w.Code != http.StatusForbidden {
		t.Fatalf("removing a grant this build cannot place: code=%d body=%s, want 403", w.Code, w.Body.String())
	}
	wantBodyNames(t, w.Body.String(),
		"the role of teamowner in this team is archon",
		"your own is admin",
		"an admin or owner of the team's organization, or a platform admin")
	wantBodyLacks(t, w.Body.String(), "a member of this team who already holds archon")
	if mb, err := real.GetMembership(ctx, teamOwnerID, "t1"); err != nil || mb.Role != identity.RoleOwner {
		t.Errorf("the refused removal moved the grant: %+v (%v)", mb, err)
	}

	super := removeTeamMemberAs(s, superAdminCtx(), "t1", teamOwnerID)
	if super.Code != http.StatusNoContent {
		t.Fatalf("a platform admin over the same grant: code=%d body=%s, want 204", super.Code, super.Body.String())
	}
	if mb, err := real.GetMembership(ctx, teamOwnerID, "t1"); !errors.Is(err, identity.ErrNotFound) {
		t.Errorf("the platform admin's removal did not land: %+v (%v)", mb, err)
	}
}

// And inside a team that no longer EXISTS: the dangling grant a deleted team
// leaves is still a grant, so the rank that decides who clears it is the team
// one — the only ladder there, the parent org going with the team — and the
// refusal offers no org remedy either.
func TestMemberRank_aDeletedTeamKeepsTheTeamLadder(t *testing.T) {
	s := newOrgCredsTestServer(t)
	ctx := context.Background()
	seedRacerAsTeamAdmin(t, s)
	seedAccount(t, s, teamOwnerID, "teamowner@example.com")
	seedOrgRow(t, s, teamOwnerID, "o1", identity.OrgRoleMember)
	seedTeamGrant(t, s, teamOwnerID, "t1", identity.RoleOwner)
	if err := s.authStore().DeleteTeam(ctx, "t1"); err != nil {
		t.Fatal(err)
	}

	w := removeTeamMemberAs(s, racerCtx(), "t1", teamOwnerID)
	if w.Code != http.StatusForbidden {
		t.Fatalf("a team admin clearing the dangling grant of a team OWNER: code=%d body=%s, want 403",
			w.Code, w.Body.String())
	}
	wantBodyNames(t, w.Body.String(), "the role of teamowner in this team is owner", "your own is admin",
		"a member of this team who already holds owner writes it, or a platform admin")
	wantBodyLacks(t, w.Body.String(), "organization")
	if role, ok := teamGrantRole(t, s, teamOwnerID, "t1"); !ok || role != identity.RoleOwner {
		t.Errorf("the refused removal moved the dangling grant: role=%q present=%v", role, ok)
	}
}

// The mint's TEAM cap (the one that closes the two-request self-promotion a
// team with no parent org leaves open): accepting an invitation writes the
// grant it names, so the role minted is measured against the minter's own
// grant wherever their authority over the team IS that grant. The refusal is
// asserted on the invitation ROWS the store holds afterwards — a mint that
// answered 403 and inserted anyway would hand out the rank on the next accept
// — and the accept is then performed, so the claim is about the two requests
// rather than about the first.
func TestTeamInvitations_aTeamRoleIsNotMintedAboveTheCreatorsGrant(t *testing.T) {
	s := newOrgCredsTestServer(t)
	ctx := context.Background()
	seedRacer(t, s)
	seedTeam(t, s, "solo", "solo")
	seedTeamGrant(t, s, racerID, "solo", identity.RoleAdmin)
	seedAccount(t, s, teamOwnerID, "teamowner@example.com")
	seedTeamGrant(t, s, teamOwnerID, "solo", identity.RoleOwner)

	w := mintTeamInvitationAs(s, racerCtx(), "solo", "racer@example.com", identity.RoleOwner)
	if w.Code != http.StatusForbidden {
		t.Fatalf("a team admin minting owner for their own address in an org-less team: code=%d body=%s, want 403",
			w.Code, w.Body.String())
	}
	wantBodyNames(t, w.Body.String(),
		"a team invitation cannot carry a team role above its creator's grant",
		"accepting an invitation at role owner writes that grant in this team",
		"your own is admin",
		"a member of this team who already holds owner writes it, or a platform admin")
	// The remedy a team with no organization offers is the team's own
	// members and a platform admin: there is no org to hold one.
	wantBodyLacks(t, w.Body.String(), "organization")
	invs, err := s.authStore().ListInvitationsByTeam(ctx, "solo")
	if err != nil {
		t.Fatal(err)
	}
	if len(invs) != 0 {
		t.Fatalf("the refused mint wrote an invitation anyway: %+v", invs)
	}
	// Request two, which the mint is what denies: there is no token, and the
	// grant the accept would have raised is untouched.
	if role, ok := teamGrantRole(t, s, racerID, "solo"); !ok || role != identity.RoleAdmin {
		t.Errorf("the refused mint moved the minter's own grant: role=%q present=%v", role, ok)
	}
	if role, ok := teamGrantRole(t, s, teamOwnerID, "solo"); !ok || role != identity.RoleOwner {
		t.Errorf("the refused mint reached the owner's grant: role=%q present=%v", role, ok)
	}
}

// What the mint's team cap does NOT refuse, each cell measured rather than
// claimed: the cap is on the rank, not on the act.
func TestTeamInvitations_theMintsThatStillGoThrough(t *testing.T) {
	for _, c := range []struct {
		name   string
		teamID string
		seed   func(*testing.T, *Server) context.Context
		role   identity.Role
	}{
		// Equal rank passes, as it does on the roster's own writes.
		{"a team admin mints admin", "solo", seedRacerInSoloAsAdmin, identity.RoleAdmin},
		// config_editor mirrors up to NOTHING and sits on no team ladder
		// (ADR-078), so neither cap measures it.
		{"a team admin mints config_editor", "solo", seedRacerInSoloAsAdmin, identity.RoleConfigEditor},
		// The owner of an org-less team is the top of its only ladder.
		{"a team owner mints owner", "solo", func(t *testing.T, s *Server) context.Context {
			t.Helper()
			seedRacer(t, s)
			seedTeam(t, s, "solo", "solo")
			seedTeamGrant(t, s, racerID, "solo", identity.RoleOwner)
			return racerCtx()
		}, identity.RoleOwner},
		// An admin of the parent org manages every team in it, so no grant
		// of theirs bounds what they mint inside it — here they hold none.
		{"an org admin mints owner in a team of their org", "t1", func(t *testing.T, s *Server) context.Context {
			t.Helper()
			return orgAdminCtx()
		}, identity.RoleOwner},
		// IsSuperAdmin is the escape hatch, and a platform admin holds no
		// membership row for a cap to read.
		{"a platform admin mints owner", "solo", func(t *testing.T, s *Server) context.Context {
			t.Helper()
			seedTeam(t, s, "solo", "solo")
			return superAdminCtx()
		}, identity.RoleOwner},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newOrgCredsTestServer(t)
			ctx := c.seed(t, s)
			w := mintTeamInvitationAs(s, ctx, c.teamID, "newcomer@example.com", c.role)
			if w.Code != http.StatusOK {
				t.Fatalf("minting %s in %s: code=%d body=%s, want 200", c.role, c.teamID, w.Code, w.Body.String())
			}
			invs, err := s.authStore().ListInvitationsByTeam(context.Background(), c.teamID)
			if err != nil {
				t.Fatal(err)
			}
			if len(invs) != 1 || invs[0].Role != c.role {
				t.Fatalf("the mint left %d invitations (%+v), want one at %s", len(invs), invs, c.role)
			}
		})
	}
}

func seedRacerInSoloAsAdmin(t *testing.T, s *Server) context.Context {
	t.Helper()
	seedRacer(t, s)
	seedTeam(t, s, "solo", "solo")
	seedTeamGrant(t, s, racerID, "solo", identity.RoleAdmin)
	return racerCtx()
}

func mintTeamInvitationAs(s *Server, ctx context.Context, teamID, email string, role identity.Role) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	s.handleCreateInvitation(w, teamReq(ctx, "POST", "/api/teams/"+teamID+"/invitations",
		`{"email":"`+email+`","role":"`+string(role)+`"}`, teamID, ""))
	return w
}

// grantAfterTheMintGate answers the minter's own grant differently once the
// invitation row has LANDED — the write that lowers it, interleaved with a
// mint whose cap read the grant before it. The flip is keyed on the
// invitation INSERT rather than on a read count: the cap and the confirmation
// read the same row, so a gate keyed on an ordinal would answer whichever of
// them the build happens to call first.
type grantAfterTheMintGate struct {
	identity.Store
	userID, teamID string

	// after is the grant the confirmation reads: a role that no longer
	// covers what was minted, or none at all when gone is set.
	after identity.Role
	gone  bool
	err   error

	mu     sync.Mutex
	minted bool
	reads  int
}

func (g *grantAfterTheMintGate) CreateInvitation(ctx context.Context, inv identity.Invitation) error {
	if err := g.Store.CreateInvitation(ctx, inv); err != nil {
		return err
	}
	if inv.TeamID == g.teamID {
		g.mu.Lock()
		g.minted = true
		g.mu.Unlock()
	}
	return nil
}

func (g *grantAfterTheMintGate) GetMembership(ctx context.Context, userID, teamID string) (identity.Membership, error) {
	if userID != g.userID || teamID != g.teamID {
		return g.Store.GetMembership(ctx, userID, teamID)
	}
	g.mu.Lock()
	minted := g.minted
	if minted {
		g.reads++
	}
	g.mu.Unlock()
	switch {
	case !minted:
		return g.Store.GetMembership(ctx, userID, teamID)
	case g.err != nil:
		return identity.Membership{}, g.err
	case g.gone:
		return identity.Membership{}, identity.ErrNotFound
	}
	return identity.Membership{UserID: userID, TeamID: teamID, Role: g.after, JoinedAt: time.Now()}, nil
}

func (g *grantAfterTheMintGate) readsAfterTheMint() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.reads
}

// The team cap's confirm half: the cap is read BEFORE the row lands, and the
// write that lowers the minter's own grant reaches only the rows its own
// listing sees, so a mint that straddles such a write is confirmed once the
// row is there — and the invitation goes back when the grant no longer covers
// it. Without it the mint hands out a rank its creator has lost.
func TestTeamInvitations_aGrantThatDropsAfterTheMintTakesTheInvitationBack(t *testing.T) {
	for _, c := range []struct {
		name  string
		after identity.Role
		gone  bool
		names []string
	}{
		{"lowered", identity.RoleMember, false, []string{
			"a team invitation cannot carry a team role above its creator's grant",
			"your own is member by the time it was minted",
			"the invitation this request minted was revoked",
		}},
		{"revoked", "", true, []string{
			"your own is none by the time it was minted",
			"the invitation this request minted was revoked",
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newOrgCredsTestServer(t)
			ctx := context.Background()
			real := s.authStore()
			seedRacer(t, s)
			seedTeam(t, s, "solo", "solo")
			seedTeamGrant(t, s, racerID, "solo", identity.RoleOwner)
			gate := &grantAfterTheMintGate{Store: real, userID: racerID, teamID: "solo", after: c.after, gone: c.gone}
			s.authSvc.SetStoreForTest(gate)

			w := mintTeamInvitationAs(s, racerCtx(), "solo", "newcomer@example.com", identity.RoleOwner)
			if w.Code != http.StatusForbidden {
				t.Fatalf("a mint whose creator's grant dropped behind it: code=%d body=%s, want 403", w.Code, w.Body.String())
			}
			wantBodyNames(t, w.Body.String(), c.names...)
			if got := gate.readsAfterTheMint(); got != 1 {
				t.Errorf("the minter's grant was read %d times after the row landed, want 1 — the confirmation's own", got)
			}
			s.authSvc.SetStoreForTest(real)
			invs, err := real.ListInvitationsByTeam(ctx, "solo")
			if err != nil {
				t.Fatal(err)
			}
			if len(invs) != 0 {
				t.Fatalf("the invitation the confirmation refused still stands: %+v", invs)
			}
		})
	}
}

// A grant that cannot be READ after the mint is not one that still covers the
// role: the invitation goes back and the 500 names that read, so an operator
// is sent to the row that could not be read rather than to the standing,
// which was read fine.
func TestTeamInvitations_aTeamCapThatCannotBeConfirmedTakesTheInvitationBack(t *testing.T) {
	s := newOrgCredsTestServer(t)
	ctx := context.Background()
	real := s.authStore()
	seedRacer(t, s)
	seedTeam(t, s, "solo", "solo")
	seedTeamGrant(t, s, racerID, "solo", identity.RoleOwner)
	gate := &grantAfterTheMintGate{Store: real, userID: racerID, teamID: "solo", err: errors.New("pool saturated")}
	s.authSvc.SetStoreForTest(gate)

	w := mintTeamInvitationAs(s, racerCtx(), "solo", "newcomer@example.com", identity.RoleOwner)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("a mint whose creator's grant could not be re-read: code=%d body=%s, want 500", w.Code, w.Body.String())
	}
	wantBodyNames(t, w.Body.String(),
		"confirming your own grant in the team this invitation joins after it was minted failed",
		"pool saturated",
		"a cap that cannot be confirmed is not one",
		"the invitation this request minted was revoked")
	s.authSvc.SetStoreForTest(real)
	invs, err := real.ListInvitationsByTeam(ctx, "solo")
	if err != nil {
		t.Fatal(err)
	}
	if len(invs) != 0 {
		t.Fatalf("the invitation whose cap could not be confirmed still stands: %+v", invs)
	}
}

// The ONE read a removal takes of the target's grant answers both questions
// it is asked — is there a grant at all, and what rank does it hold — so a
// grant that cannot be READ refuses the removal with the store's error. An
// unreadable row read as "absent" would skip both ladders and take the grant
// of a member ranked above the author.
func TestMemberRank_aRemovalWhoseTargetGrantCannotBeReadRefuses(t *testing.T) {
	s := newOrgCredsTestServer(t)
	seedRacerAsTeamAdmin(t, s)
	seedAccount(t, s, teamOwnerID, "teamowner@example.com")
	seedOrgRow(t, s, teamOwnerID, "o1", identity.OrgRoleMember)
	seedTeamGrant(t, s, teamOwnerID, "t1", identity.RoleOwner)
	real := s.authStore()
	s.authSvc.SetStoreForTest(&grantReadErrorGate{
		Store: real, userID: teamOwnerID, teamID: "t1", err: errors.New("pool saturated"),
	})

	w := removeTeamMemberAs(s, racerCtx(), "t1", teamOwnerID)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("a removal whose target grant could not be read: code=%d body=%s, want 500", w.Code, w.Body.String())
	}
	wantBodyNames(t, w.Body.String(), "resolve the grant of teamowner in team t1", "pool saturated")
	s.authSvc.SetStoreForTest(real)
	if role, ok := teamGrantRole(t, s, teamOwnerID, "t1"); !ok || role != identity.RoleOwner {
		t.Errorf("the refused removal took the grant anyway: role=%q present=%v", role, ok)
	}
}

// grantReadErrorGate fails the read of ONE account's grant in ONE team and
// answers every other read normally.
type grantReadErrorGate struct {
	identity.Store
	userID, teamID string
	err            error
}

func (g *grantReadErrorGate) GetMembership(ctx context.Context, userID, teamID string) (identity.Membership, error) {
	if userID == g.userID && teamID == g.teamID {
		return identity.Membership{}, g.err
	}
	return g.Store.GetMembership(ctx, userID, teamID)
}

// The cap on the rank WRITTEN binds a placement that CREATES the row too: a
// target with no grant clears the team ladder (none ranks below every rung),
// which is exactly the cell where only the cap stands between a team admin
// and handing out owner. In an org team and in a team with no parent org
// alike, since the cap's premise — the author's authority over the team is
// their own grant — holds in both.
func TestMemberRank_aTeamAdminPlacesNoNewGrantAboveTheirOwn(t *testing.T) {
	for _, teamID := range []string{"t1", "solo"} {
		t.Run(teamID, func(t *testing.T) {
			s := newOrgCredsTestServer(t)
			seedRacerAsTeamAdmin(t, s)
			seedAccount(t, s, "newcomer", "newcomer@example.com")
			seedOrgRow(t, s, "newcomer", "o1", identity.OrgRoleMember)
			if teamID == "solo" {
				seedTeam(t, s, "solo", "solo")
				seedTeamGrant(t, s, racerID, "solo", identity.RoleAdmin)
			}

			w := putTeamMemberAs(s, racerCtx(), teamID, "newcomer", identity.RoleOwner)
			if w.Code != http.StatusForbidden {
				t.Fatalf("a team admin placing a NEW grant at owner: code=%d body=%s, want 403", w.Code, w.Body.String())
			}
			wantBodyNames(t, w.Body.String(),
				"a team role is not written above the writer's own grant",
				"writing the role owner in this team takes that grant",
				"your own is admin")
			if role, ok := teamGrantRole(t, s, "newcomer", teamID); ok {
				t.Fatalf("the refused placement created the grant anyway: role=%q", role)
			}
			// The rank the author DOES cover still lands, so the refusal
			// above is the cap rather than the route.
			ok := putTeamMemberAs(s, racerCtx(), teamID, "newcomer", identity.RoleAdmin)
			if ok.Code != http.StatusOK {
				t.Fatalf("placing at the author's own rank: code=%d body=%s, want 200", ok.Code, ok.Body.String())
			}
			if role, present := teamGrantRole(t, s, "newcomer", teamID); !present || role != identity.RoleAdmin {
				t.Errorf("the placement wrote %q (present=%v), want admin", role, present)
			}
		})
	}
}

// The org ladder on a team route when the target is an org OWNER: the remedy
// has to name an OWNER of the org, because an admin of it — whom every other
// cell of this refusal names — is refused the same write by this very
// ladder. The remedy is then performed, both halves: the admin the body does
// not name is refused, and the owner it names makes the write.
func TestMemberRank_aTeamWriteOverAnOrgOwnerNamesAnOwnerAsTheRemedy(t *testing.T) {
	const orgOwnerUser = "orgowner"
	s := newOrgCredsTestServer(t)
	seedRacerAsTeamAdmin(t, s)
	ownerCtx := seedOrgOwner(t, s)
	seedAccount(t, s, orgOwnerUser, "orgowner@example.com")
	seedTeamGrant(t, s, orgOwnerUser, "t1", identity.RoleMember)

	w := patchTeamRoleAs(s, racerCtx(), "t1", orgOwnerUser, identity.RoleViewer)
	if w.Code != http.StatusForbidden {
		t.Fatalf("a team admin writing over the grant of an org owner: code=%d body=%s, want 403", w.Code, w.Body.String())
	}
	wantBodyNames(t, w.Body.String(),
		"the org role of orgowner in this org is owner", "your own is member",
		"an owner of this organization writes it, or a platform admin")
	// Not the admin clause the other cells of this refusal carry: an org
	// admin cannot make this write either.
	wantBodyLacks(t, w.Body.String(), "an admin or owner of this organization")

	admin := patchTeamRoleAs(s, orgAdminCtx(), "t1", orgOwnerUser, identity.RoleViewer)
	if admin.Code != http.StatusForbidden {
		t.Fatalf("the org ADMIN the refusal does not name: code=%d body=%s, want 403 — then naming them would work",
			admin.Code, admin.Body.String())
	}
	if role, ok := teamGrantRole(t, s, orgOwnerUser, "t1"); !ok || role != identity.RoleMember {
		t.Fatalf("a refused write moved the grant: role=%q present=%v", role, ok)
	}
	owner := patchTeamRoleAs(s, ownerCtx, "t1", orgOwnerUser, identity.RoleViewer)
	if owner.Code != http.StatusOK {
		t.Fatalf("the remedy the refusal names, performed: code=%d body=%s, want 200", owner.Code, owner.Body.String())
	}
	if role, ok := teamGrantRole(t, s, orgOwnerUser, "t1"); !ok || role != identity.RoleViewer {
		t.Errorf("the remedy's write did not land: role=%q present=%v", role, ok)
	}
}

// The CAP's refusal on a team with no parent org offers no org remedy either:
// there is no organization to hold one, and canManageTeam admits no member of
// one. The target rule's half of this is witnessed by
// aTeamWithNoOrgKeepsTheTeamLadder; this is the cap's.
func TestMemberRank_theCapOnAnOrglessTeamOffersNoOrgRemedy(t *testing.T) {
	s := newOrgCredsTestServer(t)
	seedRacer(t, s)
	seedTeam(t, s, "solo", "solo")
	seedTeamGrant(t, s, racerID, "solo", identity.RoleAdmin)

	w := patchTeamRoleAs(s, racerCtx(), "solo", racerID, identity.RoleOwner)
	if w.Code != http.StatusForbidden {
		t.Fatalf("a team admin writing owner on their own grant in an org-less team: code=%d body=%s, want 403",
			w.Code, w.Body.String())
	}
	wantBodyNames(t, w.Body.String(),
		"a team role is not written above the writer's own grant",
		"a member of this team who already holds owner writes it, or a platform admin")
	wantBodyLacks(t, w.Body.String(), "organization")
	if role, ok := teamGrantRole(t, s, racerID, "solo"); !ok || role != identity.RoleAdmin {
		t.Errorf("the refused write raised their own grant: role=%q present=%v", role, ok)
	}
}

// The remedy list of a refusal that has NO remedy but a platform admin: a
// team with no parent org — no org admins to name — whose target holds a rank
// this build cannot place, which no member of the team covers either. The
// sentence has to read as one, which is what the empty-list branch of
// joinRemedies is for.
func TestMemberRank_anOrglessTeamRefusalNamesAPlatformAdminAlone(t *testing.T) {
	s := newOrgCredsTestServer(t)
	seedRacer(t, s)
	seedTeam(t, s, "solo", "solo")
	seedTeamGrant(t, s, racerID, "solo", identity.RoleAdmin)
	seedAccount(t, s, "weird", "weird@example.com")
	seedTeamGrant(t, s, "weird", "solo", identity.RoleMember)
	real := s.authStore()
	s.authSvc.SetStoreForTest(&unrankableGrantGate{
		Store: real, userID: "weird", teamID: "solo", role: identity.Role("supervisor"),
	})

	w := patchTeamRoleAs(s, racerCtx(), "solo", "weird", identity.RoleViewer)
	if w.Code != http.StatusForbidden {
		t.Fatalf("writing over a team rank this build cannot place: code=%d body=%s, want 403", w.Code, w.Body.String())
	}
	wantBodyNames(t, w.Body.String(),
		"the role of weird in this team is supervisor", "and your own is admin — a platform admin")
	// One remedy reads as one: no empty clause before the "or", and no org
	// remedy on a team that has no org.
	wantBodyLacks(t, w.Body.String(), ", or a platform admin", "organization",
		"a member of this team who already holds supervisor")
	s.authSvc.SetStoreForTest(real)
	if role, ok := teamGrantRole(t, s, "weird", "solo"); !ok || role != identity.RoleMember {
		t.Errorf("the refused write moved the grant: role=%q present=%v", role, ok)
	}
}

// grantReadCountGate counts the reads of ONE account's grant in ONE team
// taken BEFORE the delete of that grant — the window a removal decides in.
// The count is keyed on that delete rather than on an ordinal, so the reads
// the sweep of a revoked grant makes afterwards are not counted as the
// decision's.
type grantReadCountGate struct {
	identity.Store
	userID, teamID string

	mu      sync.Mutex
	reads   int
	deleted bool
}

func (g *grantReadCountGate) GetMembership(ctx context.Context, userID, teamID string) (identity.Membership, error) {
	if userID == g.userID && teamID == g.teamID {
		g.mu.Lock()
		if !g.deleted {
			g.reads++
		}
		g.mu.Unlock()
	}
	return g.Store.GetMembership(ctx, userID, teamID)
}

func (g *grantReadCountGate) DeleteMembership(ctx context.Context, userID, teamID string) error {
	err := g.Store.DeleteMembership(ctx, userID, teamID)
	if userID == g.userID && teamID == g.teamID {
		g.mu.Lock()
		g.deleted = true
		g.mu.Unlock()
	}
	return err
}

func (g *grantReadCountGate) readsBeforeTheDelete() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.reads
}

// ONE read of the target's grant decides a removal: whether there is a grant
// to take at all — none, and no ladder is read — and the rank the team ladder
// measures come from the SAME read, so the two cannot disagree and the
// decision does not rest on a read taken earlier than the guard's own.
func TestMemberRank_aRemovalDecidesOnOneReadOfTheTargetsGrant(t *testing.T) {
	for _, c := range []struct {
		name   string
		target identity.Role
		want   int
	}{
		{"a grant the author's own rank covers", identity.RoleMember, http.StatusNoContent},
		{"a grant above the author's own", identity.RoleOwner, http.StatusForbidden},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newOrgCredsTestServer(t)
			seedRacerAsTeamAdmin(t, s)
			seedAccount(t, s, teamOwnerID, "teamowner@example.com")
			seedOrgRow(t, s, teamOwnerID, "o1", identity.OrgRoleMember)
			seedTeamGrant(t, s, teamOwnerID, "t1", c.target)
			real := s.authStore()
			gate := &grantReadCountGate{Store: real, userID: teamOwnerID, teamID: "t1"}
			s.authSvc.SetStoreForTest(gate)

			w := removeTeamMemberAs(s, racerCtx(), "t1", teamOwnerID)
			if w.Code != c.want {
				t.Fatalf("removing a %s grant: code=%d body=%s, want %d", c.target, w.Code, w.Body.String(), c.want)
			}
			if got := gate.readsBeforeTheDelete(); got != 1 {
				t.Errorf("the target's grant was read %d times before the removal decided, want 1 — the guard's own", got)
			}
			s.authSvc.SetStoreForTest(real)
		})
	}
}

// The GRANT the mint's team cap measures is read before the row lands, and a
// read that fails is a cap that cannot be applied: the mint is refused with
// the store's error rather than measured against a grant nobody read.
func TestTeamInvitations_aCreatorsGrantThatCannotBeReadRefusesTheMint(t *testing.T) {
	s := newOrgCredsTestServer(t)
	ctx := context.Background()
	real := s.authStore()
	seedRacer(t, s)
	seedTeam(t, s, "solo", "solo")
	seedTeamGrant(t, s, racerID, "solo", identity.RoleAdmin)
	s.authSvc.SetStoreForTest(&grantReadErrorAfterGateGate{
		Store: real, userID: racerID, teamID: "solo", err: errors.New("pool saturated"),
	})

	w := mintTeamInvitationAs(s, racerCtx(), "solo", "newcomer@example.com", identity.RoleAdmin)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("a mint whose creator's grant could not be read: code=%d body=%s, want 500", w.Code, w.Body.String())
	}
	wantBodyNames(t, w.Body.String(), "resolve your own grant in team solo", "pool saturated")
	wantBodyLacks(t, w.Body.String(), "your own is none")
	s.authSvc.SetStoreForTest(real)
	invs, err := real.ListInvitationsByTeam(ctx, "solo")
	if err != nil {
		t.Fatal(err)
	}
	if len(invs) != 0 {
		t.Fatalf("the refused mint wrote an invitation anyway: %+v", invs)
	}
}

// grantReadErrorAfterGateGate serves the team-management gate its read and
// fails the ones after it — the cap's own. Keyed on the gate's read rather
// than on an ordinal of every read in the request, so what it refuses is the
// read the cap takes.
type grantReadErrorAfterGateGate struct {
	identity.Store
	userID, teamID string
	err            error

	mu    sync.Mutex
	reads int
}

func (g *grantReadErrorAfterGateGate) GetMembership(ctx context.Context, userID, teamID string) (identity.Membership, error) {
	if userID != g.userID || teamID != g.teamID {
		return g.Store.GetMembership(ctx, userID, teamID)
	}
	g.mu.Lock()
	g.reads++
	n := g.reads
	g.mu.Unlock()
	if n > 1 {
		return identity.Membership{}, g.err
	}
	return g.Store.GetMembership(ctx, userID, teamID)
}

// mintGrantCancelGate lowers the minter's own GRANT in the team as their
// invitation row lands and hangs the request up with it, then refuses every
// store call whose context is already done — what a driver does, and what
// MemoryStore, which discards ctx, cannot show. It is the team twin of
// mintCancelGate, which does the same to an org standing.
type mintGrantCancelGate struct {
	identity.Store
	userID, teamID string
	after          identity.Role
	cancel         func()

	mu   sync.Mutex
	done bool
}

func (g *mintGrantCancelGate) CreateInvitation(ctx context.Context, inv identity.Invitation) error {
	if err := g.Store.CreateInvitation(ctx, inv); err != nil {
		return err
	}
	if inv.TeamID != g.teamID {
		return nil
	}
	g.mu.Lock()
	first := !g.done
	g.done = true
	g.mu.Unlock()
	if !first {
		return nil
	}
	if err := g.UpsertMembership(ctx, identity.Membership{
		UserID: g.userID, TeamID: g.teamID, Role: g.after, JoinedAt: time.Now(),
	}); err != nil {
		return err
	}
	g.cancel()
	return nil
}

func (g *mintGrantCancelGate) GetMembership(ctx context.Context, userID, teamID string) (identity.Membership, error) {
	if err := ctx.Err(); err != nil {
		return identity.Membership{}, fmt.Errorf("identity: get membership: %w", err)
	}
	return g.Store.GetMembership(ctx, userID, teamID)
}

func (g *mintGrantCancelGate) DeleteInvitation(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("identity: delete invitation: %w", err)
	}
	return g.Store.DeleteInvitation(ctx, id)
}

// The grant has already dropped when the confirmation runs, so a client that
// hangs up in between must take neither of its two steps with it: the re-read
// and the revoke each run on a context detached from the request. A revoke
// that died with the client would leave standing exactly the invitation the
// confirmation exists to remove, and a re-read that died with it would answer
// 500 on a grant it could have read. The org half of the same pair is
// TestTeamInvitations_aClientGoneAtTheMintStillLosesTheUnconfirmedInvitation.
func TestTeamInvitations_aClientGoneAtTheMintStillLosesTheInvitationTheGrantNoLongerCovers(t *testing.T) {
	s := newOrgCredsTestServer(t)
	bg := context.Background()
	real := s.authStore()
	seedRacer(t, s)
	seedTeam(t, s, "solo", "solo")
	seedTeamGrant(t, s, racerID, "solo", identity.RoleOwner)
	s.auditStore = &ctxAwareAudit{Store: audit.NewMemoryStore()}
	ctx, cancel := context.WithCancel(racerCtx())
	defer cancel()
	s.authSvc.SetStoreForTest(&mintGrantCancelGate{
		Store: real, userID: racerID, teamID: "solo", after: identity.RoleMember, cancel: cancel,
	})

	w := mintTeamInvitationAs(s, ctx, "solo", "newcomer@example.com", identity.RoleOwner)
	s.authSvc.SetStoreForTest(real)

	if mb, err := real.GetMembership(bg, racerID, "solo"); err != nil || mb.Role != identity.RoleMember {
		t.Fatalf("the grant did not drop: %+v (%v) — then this is not the window the test describes", mb, err)
	}
	invs, err := real.ListInvitationsByTeam(bg, "solo")
	if err != nil {
		t.Fatal(err)
	}
	if len(invs) != 0 {
		t.Errorf("the invitation whose cap could not be confirmed outlived the request (%+v) — the revoke died with the client (the mint answered %d: %s)",
			invs, w.Code, w.Body.String())
	}
	if w.Code != http.StatusForbidden {
		t.Errorf("the mint answered %d: %s — want 403 in the cap's own words: a grant that is READABLE is still read once the client is gone",
			w.Code, w.Body.String())
	}
	wantBodyNames(t, w.Body.String(),
		"a team invitation cannot carry a team role above its creator's grant",
		"your own is member by the time it was minted")
	rows := waitForTenantAuditRows(t, s.auditStore, "solo", "invitation.deleted", 1)
	if got := rows[0].Meta["reason"]; got != "creator_team_rank_dropped" {
		t.Errorf("the audit line of the revoke names %v, want creator_team_rank_dropped", got)
	}
}

// orgMintStandingGate lowers the minter's own ORG standing to member from the
// moment the org mint route reads the team its body names — the one window
// that route's GRANT cap binds in, since canManageOrg admits admin and owner
// alone and the cap reads that standing a second time, with no transaction
// across the two. The grant the confirmation then re-reads is answered by the
// gate this wraps, keyed on the invitation INSERT.
type orgMintStandingGate struct {
	*grantAfterTheMintGate
	orgID string

	mu       sync.Mutex
	teamRead bool
}

func (g *orgMintStandingGate) GetTeam(ctx context.Context, teamID string) (identity.Team, error) {
	tm, err := g.Store.GetTeam(ctx, teamID)
	if err == nil && teamID == g.teamID {
		g.mu.Lock()
		g.teamRead = true
		g.mu.Unlock()
	}
	return tm, err
}

func (g *orgMintStandingGate) GetOrgMembership(ctx context.Context, userID, orgID string) (identity.OrgMembership, error) {
	om, err := g.Store.GetOrgMembership(ctx, userID, orgID)
	if err != nil || userID != g.userID || orgID != g.orgID {
		return om, err
	}
	g.mu.Lock()
	lowered := g.teamRead
	g.mu.Unlock()
	if lowered {
		om.Role = identity.OrgRoleMember
	}
	return om, nil
}

// The team cap's confirm half on the ORG mint route: POST
// /api/orgs/{id}/invitations makes the one call both minting routes make, so
// it owes both confirmations there too. Its GRANT cap measures a grant only
// where the standing canManageOrg admitted is lower by the time the cap reads
// it, and the confirmation re-reads what that cap measured — the row goes
// back when the grant no longer covers the role minted. With the confirm half
// dropped on this route alone, the org mint hands out a rank its creator has
// lost on the very input the team route refuses.
func TestOrgInvitations_aGrantThatDropsAfterTheMintTakesTheInvitationBack(t *testing.T) {
	s := newOrgCredsTestServer(t)
	ctx := context.Background()
	real := s.authStore()
	seedRacerAsTeamAdmin(t, s)
	seedOrgRow(t, s, racerID, "o1", identity.OrgRoleAdmin)
	gate := &orgMintStandingGate{
		grantAfterTheMintGate: &grantAfterTheMintGate{
			Store: real, userID: racerID, teamID: "t1", after: identity.RoleViewer,
		},
		orgID: "o1",
	}
	s.authSvc.SetStoreForTest(gate)

	w := httptest.NewRecorder()
	s.handleCreateOrgInvitation(w, orgReq(racerCtx(), "POST", "/api/orgs/o1/invitations",
		`{"email":"newcomer@example.com","role":"member","team_id":"t1"}`, "o1"))
	s.authSvc.SetStoreForTest(real)

	if w.Code != http.StatusForbidden {
		t.Fatalf("an org mint whose creator's grant dropped behind it: code=%d body=%s, want 403",
			w.Code, w.Body.String())
	}
	wantBodyNames(t, w.Body.String(),
		"a team invitation cannot carry a team role above its creator's grant",
		"your own is viewer by the time it was minted",
		"the invitation this request minted was revoked")
	if got := gate.readsAfterTheMint(); got != 1 {
		t.Errorf("the minter's grant was read %d times after the row landed, want 1 — the confirmation's own", got)
	}
	invs, err := real.ListInvitationsByTeam(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(invs) != 0 {
		t.Fatalf("the invitation the confirmation refused still stands: %+v", invs)
	}
}

// The boundary the grant confirmation once STATED is closed by the writes
// themselves: every write that lowers a team authority re-measures the cap
// that would bind its target's minting NOW (teamRankTakesInvitation — the
// mint's own predicate, requireRankToInvite, with the authority the write
// left) and takes the rows it refuses. The same scenario the old witness
// asserted the row standing through both lowerings now answers with the row
// taken by the write that lowered the authority that carried it — the second
// leg is the shape the old P6 mutant claimed as a defect and this change
// ships. The first leg is the guard against over-reach: a standing that fell
// while the GRANT still carries the team takes nothing the cap that binds now
// itself admits.
func TestTeamInvitations_theStatedBoundaryIsClosed(t *testing.T) {
	s := newOrgCredsTestServer(t)
	ctx := context.Background()
	st := s.authStore()
	s.auditStore = &ctxAwareAudit{Store: audit.NewMemoryStore()}
	seedRacer(t, s)
	seedOrgRow(t, s, racerID, "o1", identity.OrgRoleAdmin)
	seedTeamGrant(t, s, racerID, "t1", identity.RoleAdmin)
	seedAccount(t, s, "boss", "boss@example.com")
	seedOrgRow(t, s, "boss", "o1", identity.OrgRoleOwner)

	_, tok := mintInvitationAs(t, s, racerCtx(), "t1", "racer@example.com", identity.RoleMember)

	demote := patchOrgRole(s, memberCtx("boss"), "o1", racerID, identity.OrgRoleMember)
	if demote.Code != http.StatusOK {
		t.Fatalf("the owner's demotion of the minter: code=%d body=%s, want 200",
			demote.Code, demote.Body.String())
	}
	if standing, _ := orgRowRole(t, s, racerID, "o1"); standing != identity.OrgRoleMember {
		t.Fatalf("the standing is %q after the demotion — then this is not the state the rule is about", standing)
	}
	invs, err := st.ListInvitationsByTeam(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(invs) != 1 || invs[0].Role != identity.RoleMember {
		t.Fatalf("the standing that fell took a row the cap that binds now admits (the grant still covers the role minted): %+v", invs)
	}

	unrank := patchTeamRoleAs(s, memberCtx("boss"), "t1", racerID, identity.RoleViewer)
	if unrank.Code != http.StatusOK {
		t.Fatalf("the owner's demotion of the minter's grant: code=%d body=%s, want 200",
			unrank.Code, unrank.Body.String())
	}
	if grant, _ := teamGrantRole(t, s, racerID, "t1"); grant != identity.RoleViewer {
		t.Fatalf("the grant is %q after its demotion — then this is not the state the rule is about", grant)
	}
	rows := waitForTenantAuditRows(t, s.auditStore, "t1", "invitation.deleted", 1)
	if got := rows[0].Meta["reason"]; got != "creator_team_rank_lowered" {
		t.Errorf("the audit line of the revoke names %v, want creator_team_rank_lowered", got)
	}
	if invs, err := st.ListInvitationsByTeam(ctx, "t1"); err != nil {
		t.Fatal(err)
	} else if len(invs) != 0 {
		t.Fatalf("the grant demotion left the row the cap that binds now refuses: %+v", invs)
	}
	// The taken row accepts no more: the token was revoked with the row.
	if _, err := s.authSvc.AcceptInvitationForExistingUser(ctx, racerID, tok); err == nil {
		t.Error("the accept of a taken invitation succeeded")
	}
}

// The first leg of the closure, on the gap the old boundary left OPEN: an org
// admin whose team grant is LOW mints a team `member` invitation for their own
// address (the mint measured their standing alone, admin covering the mirror),
// and an owner then lowers that standing below admin. The cap that would bind
// the minting now measures the grant too — standing member < admin — and the
// viewer grant refuses the role minted, so the write that lowered the standing
// takes the row with it, under its own audit cause. A build that leaves the
// row standing is the boundary this witness is named for.
func TestTeamInvitations_aLoweredOrgStandingTakesTheRowItsGrantNoLongerCovers(t *testing.T) {
	s := newOrgCredsTestServer(t)
	ctx := context.Background()
	st := s.authStore()
	s.auditStore = &ctxAwareAudit{Store: audit.NewMemoryStore()}
	seedRacer(t, s)
	seedOrgRow(t, s, racerID, "o1", identity.OrgRoleAdmin)
	seedTeamGrant(t, s, racerID, "t1", identity.RoleViewer)
	seedAccount(t, s, "boss", "boss@example.com")
	seedOrgRow(t, s, "boss", "o1", identity.OrgRoleOwner)

	_, tok := mintInvitationAs(t, s, racerCtx(), "t1", "racer@example.com", identity.RoleMember)

	demote := patchOrgRole(s, memberCtx("boss"), "o1", racerID, identity.OrgRoleMember)
	if demote.Code != http.StatusOK {
		t.Fatalf("the owner's demotion of the minter: code=%d body=%s, want 200",
			demote.Code, demote.Body.String())
	}
	if invs, err := st.ListInvitationsByTeam(ctx, "t1"); err != nil {
		t.Fatal(err)
	} else if len(invs) != 0 {
		t.Fatalf("the lowered standing left the row its grant no longer covers: %+v", invs)
	}
	rows := waitForTenantAuditRows(t, s.auditStore, "t1", "invitation.deleted", 1)
	if got := rows[0].Meta["reason"]; got != "org_member_demoted" {
		t.Errorf("the audit line of the revoke names %v, want org_member_demoted", got)
	}
	if _, err := s.authSvc.AcceptInvitationForExistingUser(ctx, racerID, tok); err == nil {
		t.Error("the accept of a taken invitation succeeded")
	}
}

// A team grant demotion ALONE, on a creator whose authority over the team is
// that grant: the row above the new grant goes, the row at it stands, and the
// audit line names the lowering write. A raise takes nothing — and the same
// lowering driven by a PLATFORM ADMIN bites the same way: the rule is about
// the authority the target is left with, whoever writes it.
func TestTeamInvitations_aGrantDemotionTakesTheRowsAboveTheNewGrant(t *testing.T) {
	s := newOrgCredsTestServer(t)
	ctx := context.Background()
	st := s.authStore()
	s.auditStore = &ctxAwareAudit{Store: audit.NewMemoryStore()}
	seedRacer(t, s) // org member of o1: their authority over the team IS the grant
	seedOrgRow(t, s, racerID, "o1", identity.OrgRoleMember)
	seedTeamGrant(t, s, racerID, "t1", identity.RoleOwner)
	seedAccount(t, s, "boss", "boss@example.com")
	seedOrgRow(t, s, "boss", "o1", identity.OrgRoleOwner)

	_, _ = mintInvitationAs(t, s, racerCtx(), "t1", "newcomer@example.com", identity.RoleMember)
	_, _ = mintInvitationAs(t, s, racerCtx(), "t1", "second@example.com", identity.RoleViewer)

	lower := putTeamMemberAs(s, memberCtx("boss"), "t1", racerID, identity.RoleViewer)
	if lower.Code != http.StatusOK {
		t.Fatalf("the owner's re-placement of the minter at a lower role: code=%d body=%s, want 200",
			lower.Code, lower.Body.String())
	}
	invs, err := st.ListInvitationsByTeam(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(invs) != 1 || invs[0].Role != identity.RoleViewer {
		t.Fatalf("the grant lowered to viewer left %+v, want only the viewer row it covers", invs)
	}
	rows := waitForTenantAuditRows(t, s.auditStore, "t1", "invitation.deleted", 1)
	if got := rows[0].Meta["reason"]; got != "creator_team_rank_lowered" {
		t.Errorf("the audit line of the revoke names %v, want creator_team_rank_lowered", got)
	}

	// The raise back reconciles nothing: the same predicate measures the cap
	// the raised grant admits, and the at-cap row stands.
	promote := patchTeamRoleAs(s, memberCtx("boss"), "t1", racerID, identity.RoleOwner)
	if promote.Code != http.StatusOK {
		t.Fatalf("the owner's raise of the minter back: code=%d body=%s, want 200",
			promote.Code, promote.Body.String())
	}
	if invs, err := st.ListInvitationsByTeam(ctx, "t1"); err != nil {
		t.Fatal(err)
	} else if len(invs) != 1 || invs[0].Role != identity.RoleViewer {
		t.Fatalf("the raise took a row the raised grant admits: %+v", invs)
	}

	// A platform admin drives the same lowering: the row at the new cap
	// stands — nothing above it is left to take — and the trail holds no new
	// row, which is the counterfactual the writer's identity is named for.
	lower = patchTeamRoleAs(s, superAdminCtx(), "t1", racerID, identity.RoleViewer)
	if lower.Code != http.StatusOK {
		t.Fatalf("the platform admin's demotion of the minter: code=%d body=%s, want 200",
			lower.Code, lower.Body.String())
	}
	if invs, err := st.ListInvitationsByTeam(ctx, "t1"); err != nil {
		t.Fatal(err)
	} else if len(invs) != 1 || invs[0].Role != identity.RoleViewer {
		t.Fatalf("the platform admin's lowering took the row the cap it wrote admits: %+v", invs)
	}
	waitForTenantAuditRows(t, s.auditStore, "t1", "invitation.deleted", 1)
}

// The removal of the grant takes the rows the standing does not cover — with
// no grant, the cap that would bind the creator's minting NOW refuses every
// role their standing does not — and hands a config_editor row OVER, as every
// lowering does: no cap has ever measured it, no cap measures it now.
func TestTeamInvitations_aGrantRemovalTakesTheRowsTheStandingDoesNotCover(t *testing.T) {
	s := newOrgCredsTestServer(t)
	ctx := context.Background()
	st := s.authStore()
	s.auditStore = &ctxAwareAudit{Store: audit.NewMemoryStore()}
	seedRacer(t, s) // org member: their authority over the team IS the grant
	seedOrgRow(t, s, racerID, "o1", identity.OrgRoleMember)
	seedTeamGrant(t, s, racerID, "t1", identity.RoleOwner)
	seedAccount(t, s, "boss", "boss@example.com")
	seedOrgRow(t, s, "boss", "o1", identity.OrgRoleOwner)

	_, _ = mintInvitationAs(t, s, racerCtx(), "t1", "newcomer@example.com", identity.RoleMember)
	_, _ = mintInvitationAs(t, s, racerCtx(), "t1", "second@example.com", identity.RoleViewer)
	_, _ = mintInvitationAs(t, s, racerCtx(), "t1", "racer@example.com", identity.RoleConfigEditor)

	remove := removeTeamMemberAs(s, memberCtx("boss"), "t1", racerID)
	if remove.Code != http.StatusNoContent {
		t.Fatalf("the owner's removal of the minter's grant: code=%d body=%s, want 204",
			remove.Code, remove.Body.String())
	}
	invs, err := st.ListInvitationsByTeam(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	// The member row and the viewer row go (no grant covers them); the
	// config_editor row stands — the caps never measured it.
	if len(invs) != 1 || invs[0].Role != identity.RoleConfigEditor {
		t.Fatalf("the removal left %+v, want only the config_editor row no cap measures", invs)
	}
	rows := waitForTenantAuditRows(t, s.auditStore, "t1", "invitation.deleted", 2)
	for _, row := range rows {
		if got := row.Meta["reason"]; got != "creator_team_rank_lowered" {
			t.Errorf("the audit line of the revoke names %v, want creator_team_rank_lowered", got)
		}
	}
}

// The grant compensation rides the same sweep: when the org membership row is
// definitively GONE, the compensation takes the grant back, sweeps the rows
// kept on it — and takes the rows its creator MINTED above what now covers
// them, the same cap measured from the grant being gone. The kept-on-grant
// sweep takes only what is addressed to the creator themselves, so the row
// here is addressed to somebody else, and the 409 the compensation answers
// names the boundary that went.
func TestTeamInvitations_theGrantCompensationTakesTheRowsMintedAboveWhatCoversThem(t *testing.T) {
	s := newOrgCredsTestServer(t)
	st := s.authStore()
	s.auditStore = &ctxAwareAudit{Store: audit.NewMemoryStore()}
	seedRacer(t, s)
	seedOrgRow(t, s, racerID, "o1", identity.OrgRoleMember)
	seedTeamGrant(t, s, racerID, "t1", identity.RoleOwner)
	seedAccount(t, s, "boss", "boss@example.com")
	seedOrgRow(t, s, "boss", "o1", identity.OrgRoleOwner)

	// A row the kept-on-grant sweep does not select: addressed to somebody
	// else, minted by the member whose grant the compensation is about to
	// take. The cap that would bind their minting once the grant is gone
	// refuses it, and the compensation's own sweep takes it.
	_, _ = mintInvitationAs(t, s, racerCtx(), "t1", "newcomer@example.com", identity.RoleMember)

	real := s.authStore()
	gate := &orgRowGoneGate{Store: real, userID: racerID, orgID: "o1"}
	s.authSvc.SetStoreForTest(gate)
	defer s.authSvc.SetStoreForTest(real)

	w := httptest.NewRecorder()
	s.confirmOrgMembershipForGrant(w, teamReq(orgAdminCtx(), "PUT", "/api/teams/t1/members/"+racerID, "", "t1", racerID),
		"o1", "t1", racerID, "placement", grantWriteAcked)
	if w.Code != http.StatusConflict {
		t.Fatalf("the compensation over a gone boundary: code=%d body=%s, want 409", w.Code, w.Body.String())
	}
	if invs, err := st.ListInvitationsByTeam(context.Background(), "t1"); err != nil {
		t.Fatal(err)
	} else if len(invs) != 0 {
		t.Fatalf("the compensation left the row the cap with no grant refuses: %+v", invs)
	}
	rows := waitForTenantAuditRows(t, s.auditStore, "t1", "invitation.deleted", 1)
	if got := rows[0].Meta["reason"]; got != "creator_team_rank_lowered" {
		t.Errorf("the audit line of the revoke names %v, want creator_team_rank_lowered", got)
	}
}

// orgRowGoneGate answers identity.ErrNotFound for ONE member's org row — the
// definitively-gone boundary the grant compensation's 409 exit acts on. Every
// other read passes through.
type orgRowGoneGate struct {
	identity.Store
	userID, orgID string
}

func (g *orgRowGoneGate) GetOrgMembership(ctx context.Context, userID, orgID string) (identity.OrgMembership, error) {
	if userID == g.userID && orgID == g.orgID {
		return identity.OrgMembership{}, identity.ErrNotFound
	}
	return g.Store.GetOrgMembership(ctx, userID, orgID)
}

// The KEEP side of the team writes' sweep, where the standing CARRIES the
// team: a creator holding an org row of admin or above keeps the rows they
// minted when only their TEAM grant is demoted. K.2 measures no grant for
// such a creator — the standing that carried their mint still carries the
// team — and that arm is load-bearing: a build that drops the standing half
// of the selection takes exactly these rows.
func TestTeamInvitations_aStandingThatCarriesTheTeamKeepsTheRowsItsGrantDemotionWouldTake(t *testing.T) {
	s := newOrgCredsTestServer(t)
	ctx := context.Background()
	st := s.authStore()
	s.auditStore = &ctxAwareAudit{Store: audit.NewMemoryStore()}
	seedAccount(t, s, "carrier", "carrier@example.com")
	seedOrgRow(t, s, "carrier", "o1", identity.OrgRoleAdmin)
	seedTeamGrant(t, s, "carrier", "t1", identity.RoleAdmin)
	seedAccount(t, s, "boss", "boss@example.com")
	seedOrgRow(t, s, "boss", "o1", identity.OrgRoleOwner)

	// The carrier's mint rested on their org standing alone (admin covering
	// the owner row's admin mirror): no grant of theirs was measured, and the
	// row is legal whatever their grant falls to.
	invID, _ := mintInvitationAs(t, s, memberCtx("carrier"), "t1", "newcomer@example.com", identity.RoleOwner)

	lower := patchTeamRoleAs(s, memberCtx("boss"), "t1", "carrier", identity.RoleMember)
	if lower.Code != http.StatusOK {
		t.Fatalf("the owner's demotion of the carrier's grant: code=%d body=%s, want 200",
			lower.Code, lower.Body.String())
	}
	if grant, _ := teamGrantRole(t, s, "carrier", "t1"); grant != identity.RoleMember {
		t.Fatalf("the carrier's grant is %q after the demotion — then this is not the state the keep side is about", grant)
	}
	if _, err := st.GetInvitation(ctx, invID); err != nil {
		t.Fatalf("the standing that carries the team lost the row its holder minted (%v) — a grant demotion took what no cap of theirs refuses", err)
	}
	if got := tenantAuditRows(t, s.auditStore, "t1", "invitation.deleted"); len(got) != 0 {
		t.Errorf("the demotion recorded %d revocations for a creator whose standing still carries the team: %+v", len(got), got)
	}
}

// The logged-in accept is a lowering write when it writes a LOWER team role
// over the acceptee's existing grant: pkg/auth reads the membership before
// the upsert and, on a lowering, calls the reconcile the server registers
// (SetTeamRankReconcile → takeTeamRankInvitations), so the rows the acceptee
// minted above the role it writes go — the self-addressed one included, with
// the grant fallen they are re-entry tickets — under the one cause every
// lowering write shares, attributed to the subsystem: the accept served no
// request of its own. A raise and an at-rank accept reconcile nothing, and a
// register with an invite token creates the grant it writes, reconciling
// nothing.
// The lowering detection's off-ladder leg: a LADDER grant lowered to
// config_editor is a lowering the accept re-measures, though the invitation's
// own role carries no rung. (The mirror leg — a creator holding only
// config_editor accepting a ladder invitation — has no observable of its own:
// the demotion that left them on config_editor already took every row the
// caps measured, so the exemption's acceptance and its refusal answer alike.)
func TestTeamInvitations_theAcceptLoweringMeasuresBothConfigEditorLegs(t *testing.T) {
	s := newOrgCredsTestServer(t)
	s.wireTeamGrantSweep()
	s.wireTeamRankReconcile()
	ctx := context.Background()
	st := s.authStore()
	s.auditStore = &ctxAwareAudit{Store: audit.NewMemoryStore()}
	seedRacer(t, s) // org member of o1: their authority over the team IS the grant
	seedTeamGrant(t, s, racerID, "t1", identity.RoleAdmin)

	// A member row to their own address, minted while their admin grant
	// covered the mint's caps.
	_, _ = mintInvitationAs(t, s, racerCtx(), "t1", "racer@example.com", identity.RoleMember)
	// The org admin hands them a config_editor capability instead: the accept
	// writes it over the admin grant — the rank falls OFF the ladder, and the
	// reconcile measures their rows against a grant no rung covers.
	_, cfgTok := mintInvitationAs(t, s, orgAdminCtx(), "t1", "racer@example.com", identity.RoleConfigEditor)

	mb, err := s.authSvc.AcceptInvitationForExistingUser(ctx, racerID, cfgTok)
	if err != nil {
		t.Fatalf("the config_editor accept: %v", err)
	}
	if mb.Role != identity.RoleConfigEditor {
		t.Fatalf("the accept wrote %s, want config_editor", mb.Role)
	}
	if grant, _ := teamGrantRole(t, s, racerID, "t1"); grant != identity.RoleConfigEditor {
		t.Fatalf("the grant after the accept is %q, want config_editor", grant)
	}
	invs, err := st.ListInvitationsByTeam(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	for _, inv := range invs {
		if inv.InvitedBy == racerID {
			t.Errorf("RE-ENTRY TICKET: the accept left standing %+v, minted above the config_editor grant it just wrote", inv)
		}
	}
	rows := waitForTenantAuditRows(t, s.auditStore, "t1", "invitation.deleted", 1)
	for _, row := range rows {
		if got := row.Meta["reason"]; got != "creator_team_rank_lowered" {
			t.Errorf("the audit line of the revoke names %v, want creator_team_rank_lowered", got)
		}
	}
}

func TestTeamInvitations_anAcceptThatLowersTheGrantTakesTheRowsMintedAboveIt(t *testing.T) {
	s := newOrgCredsTestServer(t)
	// This fixture attaches the auth service after New, so the seams the
	// constructor registers in production (wireTeamGrantSweep,
	// wireTeamRankReconcile) are registered here, the way New does it.
	s.wireTeamGrantSweep()
	s.wireTeamRankReconcile()
	ctx := context.Background()
	st := s.authStore()
	s.auditStore = &ctxAwareAudit{Store: audit.NewMemoryStore()}
	seedRacer(t, s) // org member of o1: their authority over the team IS the grant
	seedTeamGrant(t, s, racerID, "t1", identity.RoleOwner)
	seedAccount(t, s, "second", "second@example.com")
	seedOrgRow(t, s, "second", "o1", identity.OrgRoleMember)
	seedTeamGrant(t, s, "second", "t1", identity.RoleAdmin)
	seedAccount(t, s, "third", "third@example.com")
	seedOrgRow(t, s, "third", "o1", identity.OrgRoleMember)
	seedTeamGrant(t, s, "third", "t1", identity.RoleOwner)

	// The racer's own mints, legal while they held owner: a member row for
	// their own address, and one for a colleague.
	_, _ = mintInvitationAs(t, s, racerCtx(), "t1", "racer@example.com", identity.RoleMember)
	_, _ = mintInvitationAs(t, s, racerCtx(), "t1", "newcomer@example.com", identity.RoleMember)
	// The rows second and third minted at their own rank, which a raise and
	// an at-rank accept must leave alone.
	_, _ = mintInvitationAs(t, s, memberCtx("second"), "t1", "raise@example.com", identity.RoleMember)
	_, _ = mintInvitationAs(t, s, memberCtx("third"), "t1", "atrank@example.com", identity.RoleMember)

	// The org admin mints the invitations the three accepts answer: a viewer
	// for the racer — a lowering over their owner grant — an owner for
	// second (a raise over their admin grant) and an owner for third, whose
	// grant is already owner (at-rank).
	_, lowerTok := mintInvitationAs(t, s, orgAdminCtx(), "t1", "racer@example.com", identity.RoleViewer)
	_, raiseTok := mintInvitationAs(t, s, orgAdminCtx(), "t1", "second@example.com", identity.RoleOwner)
	_, atTok := mintInvitationAs(t, s, orgAdminCtx(), "t1", "third@example.com", identity.RoleOwner)

	mb, err := s.authSvc.AcceptInvitationForExistingUser(ctx, racerID, lowerTok)
	if err != nil {
		t.Fatalf("the lowering accept: %v", err)
	}
	if mb.Role != identity.RoleViewer {
		t.Fatalf("the accept wrote %s, want viewer — then this is not the lowering the test describes", mb.Role)
	}
	if grant, _ := teamGrantRole(t, s, racerID, "t1"); grant != identity.RoleViewer {
		t.Fatalf("the grant after the accept is %q, want viewer", grant)
	}
	invs, err := st.ListInvitationsByTeam(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	for _, inv := range invs {
		if inv.InvitedBy == racerID {
			t.Errorf("RE-ENTRY TICKET: the accept left standing %+v, minted above the viewer grant it just wrote", inv)
		}
	}
	rows := waitForTenantAuditRows(t, s.auditStore, "t1", "invitation.deleted", 2)
	for _, row := range rows {
		if got := row.Meta["reason"]; got != "creator_team_rank_lowered" {
			t.Errorf("the audit line of the revoke names %v, want creator_team_rank_lowered", got)
		}
		if row.ActorKind != "system" || row.ActorID != authGrantSweepActor {
			t.Errorf("the audit line's actor is %s/%s, want system/%s", row.ActorKind, row.ActorID, authGrantSweepActor)
		}
	}

	// The raise: second accepts owner over their admin grant. The same cap
	// measures what the raised grant admits and takes nothing — second's own
	// member row stands, and the trail holds no new line.
	if _, err := s.authSvc.AcceptInvitationForExistingUser(ctx, "second", raiseTok); err != nil {
		t.Fatalf("the raise accept: %v", err)
	}
	if grant, _ := teamGrantRole(t, s, "second", "t1"); grant != identity.RoleOwner {
		t.Fatalf("the raise wrote %q, want owner", grant)
	}
	if got := tenantAuditRows(t, s.auditStore, "t1", "invitation.deleted"); len(got) != 2 {
		t.Errorf("the raise recorded %d revocations, want none beyond the lowering's 2: %+v", len(got), got)
	}
	// At-rank: third accepts owner over their owner grant. Nothing is
	// lowered, so nothing is re-measured — their own member row stands.
	if _, err := s.authSvc.AcceptInvitationForExistingUser(ctx, "third", atTok); err != nil {
		t.Fatalf("the at-rank accept: %v", err)
	}
	if got := tenantAuditRows(t, s.auditStore, "t1", "invitation.deleted"); len(got) != 2 {
		t.Errorf("the at-rank accept recorded %d revocations, want none beyond the lowering's 2: %+v", len(got), got)
	}
	for _, addr := range []string{"raise@example.com", "atrank@example.com"} {
		live := false
		for _, inv := range func() []identity.Invitation {
			invs, err := st.ListInvitationsByTeam(ctx, "t1")
			if err != nil {
				t.Fatal(err)
			}
			return invs
		}() {
			if inv.Email == addr {
				live = true
			}
		}
		if !live {
			t.Errorf("the row for %s is gone — an accept that lowered nothing took it", addr)
		}
	}

	// A register with an invite token CREATES the grant it writes — there is
	// nothing to lower — and the personal team it provisions stands
	// untouched: no reconcile, no revocation.
	_, regTok := mintInvitationAs(t, s, orgAdminCtx(), "t1", "fresh@example.com", identity.RoleMember)
	res, err := s.authSvc.Register(ctx, "fresh@example.com", "correcthorse", "Fresh", regTok, "ua", "ip")
	if err != nil {
		t.Fatalf("the register creation: %v", err)
	}
	if _, err := st.GetTeam(ctx, res.User.DefaultTeamID); err != nil {
		t.Fatalf("the personal team the registration provisions did not survive the accept path's new read: %v", err)
	}
	if got := tenantAuditRows(t, s.auditStore, "t1", "invitation.deleted"); len(got) != 2 {
		t.Errorf("the register creation recorded %d revocations, want none beyond the lowering's 2: %+v", len(got), got)
	}
}

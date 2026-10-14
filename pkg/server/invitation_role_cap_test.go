package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
)

// Minting a team invitation grants ORG standing, not only team access:
// accepting one writes the team grant AND, for a role on the org ladder, an
// org membership at identity.OrgRoleForTeamRole (pkg/auth
// grantOrgMembershipForTeam). Gated by the team-management check alone, that
// is a way up — a team admin who is a plain org member mints an admin/owner
// invitation for their own address and comes back an org ADMIN, with
// canManageOrg over the org's own admins; the off-boarded holder of an orphan
// admin grant passes the same check on that grant alone and re-enters the org
// the removal took them out of.
//
// So the cap is the minter's own standing, and the refusal is measured on
// what it denies: no invitation row, no org membership, and the org's own
// admin still an org admin.
func TestTeamInvitations_aMintCannotCarryAnOrgRoleAboveItsCreators(t *testing.T) {
	for _, seed := range []struct {
		name string
		seed func(*testing.T, *Server)
		have string
	}{
		{"an ordinary team admin who is a plain org member", func(t *testing.T, s *Server) {
			t.Helper()
			seedRacer(t, s)
			if err := s.authStore().UpsertMembership(context.Background(), identity.Membership{
				UserID: racerID, TeamID: "t1", Role: identity.RoleAdmin, JoinedAt: time.Now(),
			}); err != nil {
				t.Fatal(err)
			}
		}, "member"},
		{"the off-boarded holder of an orphan admin grant", seedOrphanAdminGrant, "none"},
	} {
		for _, role := range []identity.Role{identity.RoleOwner, identity.RoleAdmin} {
			t.Run(seed.name+" mints "+string(role), func(t *testing.T) {
				s := newOrgCredsTestServer(t)
				seed.seed(t, s)
				ctx := context.Background()
				real := s.authStore()
				if !s.canManageTeam(ctx, auth.Identity{UserID: racerID}, "t1") {
					t.Fatal("the minter does not manage the team — then this is not the mint the test measures")
				}
				if s.canManageOrg(ctx, auth.Identity{UserID: racerID}, "o1") {
					t.Fatal("the minter already manages the org — then this is not the escalation the test measures")
				}

				stood, stoodErr := real.GetOrgMembership(ctx, racerID, "o1")

				selfCtx := auth.WithIdentity(ctx, auth.Identity{UserID: racerID})
				mint := httptest.NewRecorder()
				s.handleCreateInvitation(mint, teamReq(selfCtx, "POST", "/api/teams/t1/invitations",
					`{"email":"racer@example.com","role":"`+string(role)+`"}`, "t1", ""))
				if mint.Code != http.StatusForbidden {
					t.Fatalf("minting a %s invitation for one's own address: code=%d body=%s, want 403",
						role, mint.Code, mint.Body.String())
				}
				body := mint.Body.String()
				for _, want := range []string{
					"cannot carry an org role above its creator's",
					"an invitation at role " + string(role),
					"an org admin membership",
					"your own org role in this team's organization is " + seed.have,
				} {
					if !strings.Contains(body, want) {
						t.Errorf("the refusal does not name %q: %s", want, body)
					}
				}
				// The row, not the body: a refusal that answered 403 and
				// minted anyway leaves an accept away from the org.
				invs, err := real.ListInvitationsByTeam(ctx, "t1")
				if err != nil {
					t.Fatal(err)
				}
				if len(invs) != 0 {
					t.Fatalf("the refused mint wrote an invitation anyway: %+v", invs)
				}
				// And the standing it was refused for did not move: a cap
				// that minted and let the accept through would raise it to
				// org admin.
				now, nowErr := real.GetOrgMembership(ctx, racerID, "o1")
				if (stoodErr == nil) != (nowErr == nil) || stood.Role != now.Role {
					t.Errorf("the refused mint moved the minter's org standing: %+v (%v) -> %+v (%v)",
						stood, stoodErr, now, nowErr)
				}
				if s.canManageOrg(ctx, auth.Identity{UserID: racerID}, "o1") {
					t.Errorf("the refused minter manages the org: %+v", now)
				}
				// And the capability the accepted invitation would have
				// yielded is still out of reach: the org's own admin keeps
				// their membership.
				rm := httptest.NewRecorder()
				s.handleRemoveOrgMember(rm, teamReq(selfCtx, "DELETE", "/api/orgs/o1/members/orgadmin", "", "o1", "orgadmin"))
				if rm.Code != http.StatusForbidden {
					t.Errorf("the refused minter removing the org's own admin: code=%d body=%s, want 403",
						rm.Code, rm.Body.String())
				}
				if _, err := real.GetOrgMembership(ctx, "orgadmin", "o1"); err != nil {
					t.Errorf("the org admin's own membership is gone: %v", err)
				}
			})
		}
	}
}

// The rung that closes an account's way BACK IN: an account with no org
// membership at all — the off-boarded holder of an orphan admin grant, who
// passes canManageTeam on that grant alone — reaches no org role, so every
// role on the ladder is above their standing, `member` and `viewer` included.
// Minting one to their own address and accepting it is the two-request door
// that re-admits them to the org the removal took them out of, which is why
// the exemption list of the cap holds the EMPTY mirror and not the lowest
// rung of the ladder.
func TestTeamInvitations_anOrphanGrantHolderMintsNoWayBackIntoTheOrg(t *testing.T) {
	for _, role := range []identity.Role{identity.RoleMember, identity.RoleViewer} {
		t.Run("mints "+string(role), func(t *testing.T) {
			s := newOrgCredsTestServer(t)
			seedOrphanAdminGrant(t, s)
			ctx := context.Background()
			real := s.authStore()
			selfCtx := auth.WithIdentity(ctx, auth.Identity{UserID: racerID})
			if !s.canManageTeam(ctx, auth.Identity{UserID: racerID}, "t1") {
				t.Fatal("the minter does not manage the team — then this is not the mint the test measures")
			}

			mint := httptest.NewRecorder()
			s.handleCreateInvitation(mint, teamReq(selfCtx, "POST", "/api/teams/t1/invitations",
				`{"email":"racer@example.com","role":"`+string(role)+`"}`, "t1", ""))
			if mint.Code == http.StatusOK {
				// Let through: carry out the second request, so the failure
				// names the state the mint bought rather than a status code.
				var minted struct {
					Token string `json:"token"`
				}
				if err := json.Unmarshal(mint.Body.Bytes(), &minted); err != nil {
					t.Fatal(err)
				}
				_, acceptErr := s.authSvc.AcceptInvitationForExistingUser(ctx, racerID, minted.Token)
				om, omErr := real.GetOrgMembership(ctx, racerID, "o1")
				roster := httptest.NewRecorder()
				s.handleListOrgMembers(roster, orgReq(selfCtx, "GET", "/api/orgs/o1/members", "", "o1"))
				t.Fatalf("the orphan-grant holder minted a %s invitation to their own address and accepted it (err=%v): org membership %+v (err=%v), org roster code=%d — re-admitted to the org",
					role, acceptErr, om, omErr, roster.Code)
			}
			if mint.Code != http.StatusForbidden {
				t.Fatalf("minting a %s invitation with no org standing at all: code=%d body=%s, want 403",
					role, mint.Code, mint.Body.String())
			}
			// The refusal names what it refused, for a role whose mirror is
			// the bottom of the ladder as for the top.
			for _, want := range []string{
				"an invitation at role " + string(role),
				"an org member membership",
				"your own org role in this team's organization is none",
			} {
				if body := mint.Body.String(); !strings.Contains(body, want) {
					t.Errorf("the refusal does not name %q: %s", want, body)
				}
			}
			if om, err := real.GetOrgMembership(ctx, racerID, "o1"); err == nil {
				t.Fatalf("the refused mint left an org membership: %+v", om)
			}
		})
	}
}

// The cap is the ORG MIRROR of the role, not the role: a capability outside
// the org ladder (config_editor, rank 0 — ADR-078 decision 4) is mirrored up
// to no org membership at all, so it carries no standing to exceed and stays
// mintable by a team admin who holds none. Gating on
// OrgRole.AtLeast(OrgRoleForTeamRole(role)) alone would refuse every
// config_editor invitation, org owners included, since AtLeast("") is false
// by construction.
func TestTeamInvitations_aRoleThatMirrorsUpToNothingIsNotCapped(t *testing.T) {
	for _, c := range []struct {
		name string
		seed func(*testing.T, *Server)
		role identity.Role
	}{
		{"a plain org member mints config_editor", seedRacerAsTeamAdmin, identity.RoleConfigEditor},
		{"an account with no org membership mints config_editor", seedOrphanAdminGrant, identity.RoleConfigEditor},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newOrgCredsTestServer(t)
			c.seed(t, s)
			selfCtx := auth.WithIdentity(context.Background(), auth.Identity{UserID: racerID})
			mint := httptest.NewRecorder()
			s.handleCreateInvitation(mint, teamReq(selfCtx, "POST", "/api/teams/t1/invitations",
				`{"email":"newcomer@example.com","role":"`+string(c.role)+`"}`, "t1", ""))
			if mint.Code != http.StatusOK {
				t.Fatalf("minting a %s invitation: code=%d body=%s, want 200",
					c.role, mint.Code, mint.Body.String())
			}
		})
	}
}

// A separate property from the one above, on roles that DO have a mirror:
// the comparison is OrgRole.AtLeast, which includes equality, so a minter
// mints the mirror they exactly reach. A plain org member reaches org
// `member`, which is what member and viewer mirror up to (ADR-078:
// everything below admin). Gating on "strictly above" would refuse the
// ordinary team admin inviting an ordinary teammate.
func TestTeamInvitations_aMirrorTheMinterExactlyReachesStillMints(t *testing.T) {
	for _, role := range []identity.Role{identity.RoleMember, identity.RoleViewer} {
		t.Run("a plain org member mints "+string(role), func(t *testing.T) {
			s := newOrgCredsTestServer(t)
			seedRacerAsTeamAdmin(t, s)
			if want := identity.OrgRoleForTeamRole(role); want != identity.OrgRoleMember {
				t.Fatalf("%s mirrors up to %q, not the org role the minter holds — then this is not the equality rung", role, want)
			}
			selfCtx := auth.WithIdentity(context.Background(), auth.Identity{UserID: racerID})
			mint := httptest.NewRecorder()
			s.handleCreateInvitation(mint, teamReq(selfCtx, "POST", "/api/teams/t1/invitations",
				`{"email":"newcomer@example.com","role":"`+string(role)+`"}`, "t1", ""))
			if mint.Code != http.StatusOK {
				t.Fatalf("minting a %s invitation: code=%d body=%s, want 200",
					role, mint.Code, mint.Body.String())
			}
		})
	}
}

// seedRacerAsTeamAdmin makes the racer a t1 admin who is a plain member of
// the team's org: the standing a team admin ordinarily has.
func seedRacerAsTeamAdmin(t *testing.T, s *Server) {
	t.Helper()
	seedRacer(t, s)
	if err := s.authStore().UpsertMembership(context.Background(), identity.Membership{
		UserID: racerID, TeamID: "t1", Role: identity.RoleAdmin, JoinedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
}

// What the cap must not take away: an org admin or owner mints the roles
// whose mirror they already reach, a platform admin mints anything (they hold
// no org membership to read — IsSuperAdmin is the escape hatch), and a team
// with no parent org has no boundary to read at all.
func TestTeamInvitations_theStandingThatStillMints(t *testing.T) {
	s := newOrgCredsTestServer(t)
	ctx := context.Background()
	if err := s.authStore().UpsertOrgMembership(ctx, identity.OrgMembership{
		UserID: "orgowner", OrgID: "o1", Role: identity.OrgRoleOwner, JoinedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		who  string
		ctx  context.Context
		role identity.Role
	}{
		{"an org admin", orgAdminCtx(), identity.RoleOwner},
		{"an org admin", orgAdminCtx(), identity.RoleAdmin},
		{"an org admin", orgAdminCtx(), identity.RoleMember},
		{"an org owner", auth.WithIdentity(ctx, auth.Identity{UserID: "orgowner"}), identity.RoleOwner},
		{"a platform admin", superAdminCtx(), identity.RoleOwner},
	} {
		t.Run(c.who+" mints "+string(c.role), func(t *testing.T) {
			mint := httptest.NewRecorder()
			s.handleCreateInvitation(mint, teamReq(c.ctx, "POST", "/api/teams/t1/invitations",
				`{"email":"newcomer@example.com","role":"`+string(c.role)+`"}`, "t1", ""))
			if mint.Code != http.StatusOK {
				t.Fatalf("code=%d body=%s, want 200", mint.Code, mint.Body.String())
			}
		})
	}
	t.Run("a team with no parent org", func(t *testing.T) {
		seedTeam(t, s, "solo", "solo")
		if err := s.authStore().UpsertMembership(ctx, identity.Membership{
			UserID: "hermit", TeamID: "solo", Role: identity.RoleOwner, JoinedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		mint := httptest.NewRecorder()
		s.handleCreateInvitation(mint, teamReq(auth.WithIdentity(ctx, auth.Identity{UserID: "hermit"}),
			"POST", "/api/teams/solo/invitations", `{"email":"friend@example.com","role":"owner"}`, "solo", ""))
		if mint.Code != http.StatusOK {
			t.Fatalf("an orgless team owner minting owner: code=%d body=%s, want 200", mint.Code, mint.Body.String())
		}
	})
}

// orgStandingErrorGate fails the read of ONE account's org membership, the
// way a saturated pool or a stepdown does.
type orgStandingErrorGate struct {
	identity.Store
	userID, orgID string
}

func (g orgStandingErrorGate) GetOrgMembership(ctx context.Context, userID, orgID string) (identity.OrgMembership, error) {
	if userID == g.userID && orgID == g.orgID {
		return identity.OrgMembership{}, errors.New("identity: get org membership: connection reset by peer")
	}
	return g.Store.GetOrgMembership(ctx, userID, orgID)
}

// A standing that cannot be READ is not a standing: the mint is refused with
// the store's own error rather than passed through on a membership nobody
// could see.
func TestTeamInvitations_aStandingThatCannotBeReadRefusesTheMint(t *testing.T) {
	s := newOrgCredsTestServer(t)
	seedRacerAsTeamAdmin(t, s)
	ctx := context.Background()
	real := s.authStore()
	s.authSvc.SetStoreForTest(orgStandingErrorGate{Store: real, userID: racerID, orgID: "o1"})

	selfCtx := auth.WithIdentity(ctx, auth.Identity{UserID: racerID})
	mint := httptest.NewRecorder()
	s.handleCreateInvitation(mint, teamReq(selfCtx, "POST", "/api/teams/t1/invitations",
		`{"email":"racer@example.com","role":"owner"}`, "t1", ""))
	if mint.Code != http.StatusInternalServerError {
		t.Fatalf("minting while the minter's standing cannot be read: code=%d body=%s, want 500",
			mint.Code, mint.Body.String())
	}
	if body := mint.Body.String(); !strings.Contains(body, "connection reset by peer") {
		t.Errorf("the 500 does not carry the store's own error: %s", body)
	}
	s.authSvc.SetStoreForTest(real)
	invs, err := real.ListInvitationsByTeam(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(invs) != 0 {
		t.Fatalf("the refused mint wrote an invitation anyway: %+v", invs)
	}
}

// The org invitation route takes the team the invitation joins from the
// REQUEST, where canManageOrg authorized the org in the path and nothing
// about another one: a team of another org is refused the way its two
// siblings refuse it (the caps PATCH and the invitation DELETE), since
// accepting such an invitation writes a grant — and, for a ladder role, an
// org membership — inside a tenant the caller does not manage.
func TestOrgInvitations_aTeamOfAnotherOrgIsNotInvitedInto(t *testing.T) {
	s := newOrgCredsTestServer(t)
	ctx := context.Background()
	real := s.authStore()
	for _, role := range []identity.Role{identity.RoleOwner, identity.RoleConfigEditor} {
		t.Run("o1 admin mints "+string(role)+" into o2's team", func(t *testing.T) {
			w := httptest.NewRecorder()
			s.handleCreateOrgInvitation(w, orgReq(orgAdminCtx(), "POST", "/api/orgs/o1/invitations",
				`{"email":"outsider@example.com","role":"`+string(role)+`","team_id":"t9"}`, "o1"))
			if w.Code != http.StatusNotFound {
				t.Fatalf("minting into a team of another org: code=%d body=%s, want 404", w.Code, w.Body.String())
			}
			if body := w.Body.String(); !strings.Contains(body, "team not in org") {
				t.Errorf("the refusal does not say what it refused: %s", body)
			}
			invs, err := real.ListInvitationsByTeam(ctx, "t9")
			if err != nil {
				t.Fatal(err)
			}
			if len(invs) != 0 {
				t.Fatalf("the refused mint wrote an invitation into the other org's team: %+v", invs)
			}
		})
	}
	t.Run("a team of the org in the path", func(t *testing.T) {
		w := httptest.NewRecorder()
		s.handleCreateOrgInvitation(w, orgReq(orgAdminCtx(), "POST", "/api/orgs/o1/invitations",
			`{"email":"newcomer@example.com","role":"owner","team_id":"t2"}`, "o1"))
		if w.Code != http.StatusOK {
			t.Fatalf("minting into a team of this org: code=%d body=%s, want 200", w.Code, w.Body.String())
		}
		var minted struct {
			TeamID string `json:"team_id"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &minted); err != nil {
			t.Fatal(err)
		}
		if minted.TeamID != "t2" {
			t.Errorf("the invitation joins team %q, want t2", minted.TeamID)
		}
	})
	t.Run("no team named at all", func(t *testing.T) {
		w := httptest.NewRecorder()
		s.handleCreateOrgInvitation(w, orgReq(orgAdminCtx(), "POST", "/api/orgs/o1/invitations",
			`{"email":"newcomer2@example.com","role":"admin"}`, "o1"))
		if w.Code != http.StatusOK {
			t.Fatalf("minting into the org's first team: code=%d body=%s, want 200", w.Code, w.Body.String())
		}
	})
	// A team id that exists NOWHERE answers what a team of another org
	// answers, in the same words its two siblings use for both causes
	// (handleUpdateOrgTeamCaps, handleDeleteOrgInvitation): an org admin
	// who can tell them apart can probe which team ids exist in the tenants
	// they do not manage.
	t.Run("a team that exists nowhere", func(t *testing.T) {
		w := httptest.NewRecorder()
		s.handleCreateOrgInvitation(w, orgReq(orgAdminCtx(), "POST", "/api/orgs/o1/invitations",
			`{"email":"newcomer3@example.com","role":"owner","team_id":"does-not-exist"}`, "o1"))
		if w.Code != http.StatusNotFound {
			t.Fatalf("minting into a team that exists nowhere: code=%d body=%s, want 404", w.Code, w.Body.String())
		}
		if body := w.Body.String(); !strings.Contains(body, "team not in org") {
			t.Errorf("the refusal does not answer what a team of another org answers: %s", body)
		}
	})
	// And a team that cannot be READ is neither of those: the cap cannot be
	// applied, so the mint is refused with the store's own error rather than
	// collapsed into the 404 of a team that is not there.
	t.Run("a team whose read fails", func(t *testing.T) {
		sick := newOrgCredsTestServer(t)
		realStore := sick.authStore()
		sick.authSvc.SetStoreForTest(teamReadErrorGate{Store: realStore, teamID: "t2"})
		w := httptest.NewRecorder()
		sick.handleCreateOrgInvitation(w, orgReq(orgAdminCtx(), "POST", "/api/orgs/o1/invitations",
			`{"email":"newcomer4@example.com","role":"owner","team_id":"t2"}`, "o1"))
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("minting while the team cannot be read: code=%d body=%s, want 500", w.Code, w.Body.String())
		}
		for _, want := range []string{"read the team", "connection reset by peer"} {
			if body := w.Body.String(); !strings.Contains(body, want) {
				t.Errorf("the 500 does not say %q: %s", want, body)
			}
		}
		sick.authSvc.SetStoreForTest(realStore)
		invs, err := realStore.ListInvitationsByTeam(ctx, "t2")
		if err != nil {
			t.Fatal(err)
		}
		if len(invs) != 0 {
			t.Fatalf("the refused mint wrote an invitation anyway: %+v", invs)
		}
	})
}

// listTeamsErrorGate fails ListTeamsByOrg for ONE org, the way a saturated
// pool or a stepdown does.
type listTeamsErrorGate struct {
	identity.Store
	orgID string
}

func (g listTeamsErrorGate) ListTeamsByOrg(ctx context.Context, orgID string) ([]identity.Team, error) {
	if orgID == g.orgID {
		return nil, errors.New("identity: list teams by org: connection reset by peer")
	}
	return g.Store.ListTeamsByOrg(ctx, orgID)
}

// listInvitationsErrorGate fails the pending-invitation read of ONE team.
type listInvitationsErrorGate struct {
	identity.Store
	teamID string
}

func (g listInvitationsErrorGate) ListInvitationsByTeam(ctx context.Context, teamID string) ([]identity.Invitation, error) {
	if teamID == g.teamID {
		return nil, errors.New("identity: list invitations by team: connection reset by peer")
	}
	return g.Store.ListInvitationsByTeam(ctx, teamID)
}

// The org's primary team is resolved by a READ, and a read that failed is not
// an org without a team: every org route that keys an org-level resource on
// that team (firstTeamInOrg — the mint's default team, the SSO providers and
// domains) answers the store's error instead of the client error of an org
// that has no team. The genuinely empty org keeps that client error.
func TestOrgRoutes_orgTeamsThatCannotBeListedAreNotAnOrgWithoutATeam(t *testing.T) {
	ctx := context.Background()
	t.Run("the mint's default team", func(t *testing.T) {
		s := newOrgCredsTestServer(t)
		real := s.authStore()
		s.authSvc.SetStoreForTest(listTeamsErrorGate{Store: real, orgID: "o1"})
		w := httptest.NewRecorder()
		s.handleCreateOrgInvitation(w, orgReq(orgAdminCtx(), "POST", "/api/orgs/o1/invitations",
			`{"email":"newcomer@example.com","role":"member"}`, "o1"))
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("minting with no team_id while the org's teams cannot be listed: code=%d body=%s, want 500",
				w.Code, w.Body.String())
		}
		for _, want := range []string{"resolve the org's teams", "connection reset by peer"} {
			if body := w.Body.String(); !strings.Contains(body, want) {
				t.Errorf("the 500 does not say %q: %s", want, body)
			}
		}
		s.authSvc.SetStoreForTest(real)
		invs, err := real.ListInvitationsByTeam(ctx, "t1")
		if err != nil {
			t.Fatal(err)
		}
		if len(invs) != 0 {
			t.Fatalf("the refused mint wrote an invitation anyway: %+v", invs)
		}
	})
	t.Run("the org's SSO providers", func(t *testing.T) {
		s := newOrgCredsTestServer(t)
		s.authSvc.SetStoreForTest(listTeamsErrorGate{Store: s.authStore(), orgID: "o1"})
		w := httptest.NewRecorder()
		s.handleListOrgSSOProviders(w, orgReq(orgAdminCtx(), "GET", "/api/orgs/o1/sso/providers", "", "o1"))
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("listing the org's SSO providers while its teams cannot be listed: code=%d body=%s, want 500",
				w.Code, w.Body.String())
		}
		for _, want := range []string{"resolve the org's teams", "connection reset by peer"} {
			if body := w.Body.String(); !strings.Contains(body, want) {
				t.Errorf("the 500 does not say %q: %s", want, body)
			}
		}
	})
	t.Run("the org's SSO domains", func(t *testing.T) {
		s := newOrgCredsTestServer(t)
		s.authSvc.SetStoreForTest(listTeamsErrorGate{Store: s.authStore(), orgID: "o1"})
		w := httptest.NewRecorder()
		s.handleListOrgSSODomains(w, orgReq(orgAdminCtx(), "GET", "/api/orgs/o1/sso/domains", "", "o1"))
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("listing the org's SSO domains while its teams cannot be listed: code=%d body=%s, want 500",
				w.Code, w.Body.String())
		}
		for _, want := range []string{"resolve the org's teams", "connection reset by peer"} {
			if body := w.Body.String(); !strings.Contains(body, want) {
				t.Errorf("the 500 does not say %q: %s", want, body)
			}
		}
	})
	// The list the remedy is found in: the invitation a mint left pending is
	// revoked by its id, read from here, so an empty list means nothing
	// pending — never "the store could not say".
	t.Run("the org's pending invitations", func(t *testing.T) {
		s := newOrgCredsTestServer(t)
		s.authSvc.SetStoreForTest(listTeamsErrorGate{Store: s.authStore(), orgID: "o1"})
		w := httptest.NewRecorder()
		s.handleListOrgInvitations(w, orgReq(orgAdminCtx(), "GET", "/api/orgs/o1/invitations", "", "o1"))
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("listing the org's invitations while its teams cannot be listed: code=%d body=%s, want 500",
				w.Code, w.Body.String())
		}
		for _, want := range []string{"resolve the org's teams", "connection reset by peer"} {
			if body := w.Body.String(); !strings.Contains(body, want) {
				t.Errorf("the 500 does not say %q: %s", want, body)
			}
		}
	})
	t.Run("a team whose invitations cannot be listed", func(t *testing.T) {
		s := newOrgCredsTestServer(t)
		s.authSvc.SetStoreForTest(listInvitationsErrorGate{Store: s.authStore(), teamID: "t1"})
		w := httptest.NewRecorder()
		s.handleListOrgInvitations(w, orgReq(orgAdminCtx(), "GET", "/api/orgs/o1/invitations", "", "o1"))
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("listing the org's invitations while a team's cannot be read: code=%d body=%s, want 500",
				w.Code, w.Body.String())
		}
		for _, want := range []string{"list the invitations of team t1", "connection reset by peer"} {
			if body := w.Body.String(); !strings.Contains(body, want) {
				t.Errorf("the 500 does not say %q: %s", want, body)
			}
		}
	})
	t.Run("an org that really has no team", func(t *testing.T) {
		s := newOrgCredsTestServer(t)
		seedOrg(t, s, "o3", "o3")
		if err := s.authStore().UpsertOrgMembership(ctx, identity.OrgMembership{
			UserID: "orgadmin", OrgID: "o3", Role: identity.OrgRoleAdmin, JoinedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		s.handleCreateOrgInvitation(w, orgReq(orgAdminCtx(), "POST", "/api/orgs/o3/invitations",
			`{"email":"newcomer@example.com","role":"member"}`, "o3"))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("minting into an org with no team: code=%d body=%s, want 400", w.Code, w.Body.String())
		}
		if body := w.Body.String(); !strings.Contains(body, "org has no team to invite into") {
			t.Errorf("the 400 does not say the org has no team: %s", body)
		}
		sso := httptest.NewRecorder()
		s.handleListOrgSSOProviders(sso, orgReq(orgAdminCtx(), "GET", "/api/orgs/o3/sso/providers", "", "o3"))
		if sso.Code != http.StatusNotFound {
			t.Fatalf("listing the SSO providers of an org with no team: code=%d body=%s, want 404", sso.Code, sso.Body.String())
		}
		dom := httptest.NewRecorder()
		s.handleListOrgSSODomains(dom, orgReq(orgAdminCtx(), "GET", "/api/orgs/o3/sso/domains", "", "o3"))
		if dom.Code != http.StatusNotFound {
			t.Fatalf("listing the SSO domains of an org with no team: code=%d body=%s, want 404", dom.Code, dom.Body.String())
		}
	})
}

// What an invitation with NO team_id grants, since the studio offers that as
// a choice and the API doc states it: the org's first NON-personal team — the
// storage tenant of ADR-048, not the personal team a founder's signup made —
// and, on accept, a team grant AT THE ROLE ASKED FOR there, besides the org
// membership the ladder mirrors up. There is no org-only invitation.
func TestOrgInvitations_theDefaultTeamIsTheOrgsFirstNonPersonalTeam(t *testing.T) {
	s := newOrgCredsTestServer(t)
	ctx := context.Background()
	real := s.authStore()
	seedOrg(t, s, "o4", "o4")
	// The personal team is created FIRST, so picking "the first team of the
	// org" rather than the first non-personal one answers p4.
	for _, tm := range []struct {
		id       string
		personal bool
		created  time.Time
	}{
		{"p4", true, time.Now().Add(-2 * time.Hour)},
		{"n4", false, time.Now().Add(-1 * time.Hour)},
	} {
		if _, err := real.CreateTeam(ctx, identity.Team{
			ID: tm.id, Name: tm.id, Slug: tm.id, OrgID: "o4", Personal: tm.personal, CreatedAt: tm.created,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := real.UpsertOrgMembership(ctx, identity.OrgMembership{
		UserID: "o4owner", OrgID: "o4", Role: identity.OrgRoleOwner, JoinedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := real.CreateUser(ctx, identity.User{
		ID: "newcomer", Email: "newcomer@example.com", Status: identity.UserStatusActive, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	s.handleCreateOrgInvitation(w, orgReq(auth.WithIdentity(ctx, auth.Identity{UserID: "o4owner"}),
		"POST", "/api/orgs/o4/invitations", `{"email":"newcomer@example.com","role":"owner"}`, "o4"))
	if w.Code != http.StatusOK {
		t.Fatalf("minting with no team_id: code=%d body=%s, want 200", w.Code, w.Body.String())
	}
	var minted struct {
		TeamID string `json:"team_id"`
		Token  string `json:"token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &minted); err != nil {
		t.Fatal(err)
	}
	if minted.TeamID != "n4" {
		t.Fatalf("the invitation joins team %q, want n4 — the org's first NON-personal team", minted.TeamID)
	}
	if _, err := s.authSvc.AcceptInvitationForExistingUser(ctx, "newcomer", minted.Token); err != nil {
		t.Fatalf("accepting it: %v", err)
	}
	m, err := real.GetMembership(ctx, "newcomer", "n4")
	if err != nil {
		t.Fatalf("the accept wrote no team grant in n4: %v", err)
	}
	if m.Role != identity.RoleOwner {
		t.Errorf("the team grant carries role %q, want owner — the role the invitation asked for", m.Role)
	}
	if om, err := real.GetOrgMembership(ctx, "newcomer", "o4"); err != nil || om.Role != identity.OrgRoleAdmin {
		t.Errorf("the org membership the ladder mirrors up is %+v (%v), want admin", om, err)
	}
}

// mustIdentity reads back the principal a test context carries, so a
// pre-check measures the caller the handler will see.
func mustIdentity(t *testing.T, ctx context.Context) auth.Identity {
	t.Helper()
	id, ok := auth.FromContext(ctx)
	if !ok {
		t.Fatal("the test context carries no identity")
	}
	return id
}

// teamReadErrorGate fails the read of ONE team, the way a saturated pool or a
// stepdown does.
type teamReadErrorGate struct {
	identity.Store
	teamID string
}

func (g teamReadErrorGate) GetTeam(ctx context.Context, teamID string) (identity.Team, error) {
	if teamID == g.teamID {
		return identity.Team{}, errors.New("identity: get team: connection reset by peer")
	}
	return g.Store.GetTeam(ctx, teamID)
}

// The team names the org the cap is read in, so a team that cannot be read is
// a cap that cannot be applied: the mint is refused rather than minted
// uncapped, and a store that is sick says so instead of answering the 404 of
// a team that does not exist.
func TestTeamInvitations_aTeamThatCannotBeReadRefusesTheMint(t *testing.T) {
	// Both caller classes the team-management check admits WITHOUT resolving
	// the team: a platform admin, and the holder of a grant in a team that is
	// gone — the orphan the cap exists for, who reaches the team read.
	for _, c := range []struct {
		who  string
		ctx  func(*testing.T, *Server) context.Context
		team string
	}{
		{"a platform admin", func(*testing.T, *Server) context.Context { return superAdminCtx() }, "nope"},
		{"the holder of a grant in a team that is gone", func(t *testing.T, s *Server) context.Context {
			t.Helper()
			if err := s.authStore().UpsertMembership(context.Background(), identity.Membership{
				UserID: "ghost", TeamID: "gone", Role: identity.RoleAdmin, JoinedAt: time.Now(),
			}); err != nil {
				t.Fatal(err)
			}
			return auth.WithIdentity(context.Background(), auth.Identity{UserID: "ghost"})
		}, "gone"},
	} {
		t.Run("a team that does not exist, minted by "+c.who, func(t *testing.T) {
			s := newOrgCredsTestServer(t)
			ctx := c.ctx(t, s)
			if !s.canManageTeam(context.Background(), mustIdentity(t, ctx), c.team) {
				t.Fatal("the minter does not pass the team-management check — then this is not the caller class the test measures")
			}
			mint := httptest.NewRecorder()
			s.handleCreateInvitation(mint, teamReq(ctx, "POST", "/api/teams/"+c.team+"/invitations",
				`{"email":"newcomer@example.com","role":"owner"}`, c.team, ""))
			if mint.Code != http.StatusNotFound {
				t.Fatalf("minting into a team that does not exist: code=%d body=%s, want 404",
					mint.Code, mint.Body.String())
			}
			// The route's own vocabulary, not the storage layer's: the service
			// answers this read with the same error, and a client told
			// "identity: not found" learns which layer failed instead of what
			// it asked for.
			if body := mint.Body.String(); !strings.Contains(body, auth.ErrTeamNotFound.Error()) {
				t.Errorf("the 404 does not name the team: %s", body)
			}
			invs, err := s.authStore().ListInvitationsByTeam(context.Background(), c.team)
			if err != nil {
				t.Fatal(err)
			}
			if len(invs) != 0 {
				t.Fatalf("the refused mint wrote an invitation anyway: %+v", invs)
			}
		})
	}
	t.Run("a team whose read fails", func(t *testing.T) {
		s := newOrgCredsTestServer(t)
		seedRacerAsTeamAdmin(t, s)
		real := s.authStore()
		s.authSvc.SetStoreForTest(teamReadErrorGate{Store: real, teamID: "t1"})
		mint := httptest.NewRecorder()
		s.handleCreateInvitation(mint, teamReq(auth.WithIdentity(context.Background(), auth.Identity{UserID: racerID}),
			"POST", "/api/teams/t1/invitations", `{"email":"racer@example.com","role":"owner"}`, "t1", ""))
		if mint.Code != http.StatusInternalServerError {
			t.Fatalf("minting while the team cannot be read: code=%d body=%s, want 500",
				mint.Code, mint.Body.String())
		}
		for _, want := range []string{"read the team", "connection reset by peer"} {
			if body := mint.Body.String(); !strings.Contains(body, want) {
				t.Errorf("the 500 does not say %q: %s", want, body)
			}
		}
		s.authSvc.SetStoreForTest(real)
		invs, err := real.ListInvitationsByTeam(context.Background(), "t1")
		if err != nil {
			t.Fatal(err)
		}
		if len(invs) != 0 {
			t.Fatalf("the refused mint wrote an invitation anyway: %+v", invs)
		}
	})
	// Those two answers are what the caller the team-admin check admitted on
	// their OWN grant — or a platform admin — meets. An ORG admin is admitted
	// by a check that resolves the team itself (orgAdminOfTeam, to find the
	// parent org they manage), so for them a team that cannot be read, and one
	// that does not exist, are the check's own 403: the limit of what the mint
	// can say about the team, and what the API doc states for this route.
	t.Run("an org admin meets the team-management check first", func(t *testing.T) {
		for _, c := range []struct {
			name   string
			teamID string
			gate   bool
		}{
			{"a team whose read fails", "t1", true},
			{"a team that does not exist", "nope", false},
		} {
			t.Run(c.name, func(t *testing.T) {
				s := newOrgCredsTestServer(t)
				real := s.authStore()
				if c.gate {
					s.authSvc.SetStoreForTest(teamReadErrorGate{Store: real, teamID: c.teamID})
				}
				if !s.canManageOrg(context.Background(), auth.Identity{UserID: "orgadmin"}, "o1") {
					t.Fatal("the minter does not manage the org — then this is not the caller class the test measures")
				}
				mint := httptest.NewRecorder()
				s.handleCreateInvitation(mint, teamReq(orgAdminCtx(), "POST",
					"/api/teams/"+c.teamID+"/invitations",
					`{"email":"newcomer@example.com","role":"owner"}`, c.teamID, ""))
				if mint.Code != http.StatusForbidden {
					t.Fatalf("an org admin minting into %s: code=%d body=%s, want 403 — the team-management check reads the team too",
						c.name, mint.Code, mint.Body.String())
				}
				if body := mint.Body.String(); !strings.Contains(body, "admin or owner required") {
					t.Errorf("the 403 is not the team-management check's: %s", body)
				}
				s.authSvc.SetStoreForTest(real)
				invs, err := real.ListInvitationsByTeam(context.Background(), c.teamID)
				if err != nil {
					t.Fatal(err)
				}
				if len(invs) != 0 {
					t.Fatalf("the refused mint wrote an invitation anyway: %+v", invs)
				}
			})
		}
	})
}

// An org whose teams are ALL personal — a founder's own org, which every
// signup creates — has no non-personal team for the default to pick, so the
// mint falls back to the org's FIRST team: a personal one, at the role the
// body asked for. What the API doc row states, and the branch the
// first-non-personal test never reaches.
func TestOrgInvitations_theDefaultTeamOfAnOrgWhoseTeamsAreAllPersonal(t *testing.T) {
	s := newOrgCredsTestServer(t)
	ctx := context.Background()
	real := s.authStore()
	seedOrg(t, s, "o5", "o5")
	// Two of them, so "the first team" is a measurement rather than the only
	// answer available.
	for _, tm := range []struct {
		id      string
		created time.Time
	}{
		{"p5a", time.Now().Add(-2 * time.Hour)},
		{"p5b", time.Now().Add(-1 * time.Hour)},
	} {
		if _, err := real.CreateTeam(ctx, identity.Team{
			ID: tm.id, Name: tm.id, Slug: tm.id, OrgID: "o5", Personal: true, CreatedAt: tm.created,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := real.UpsertOrgMembership(ctx, identity.OrgMembership{
		UserID: "o5owner", OrgID: "o5", Role: identity.OrgRoleOwner, JoinedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := real.CreateUser(ctx, identity.User{
		ID: "newcomer5", Email: "newcomer5@example.com", Status: identity.UserStatusActive, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	s.handleCreateOrgInvitation(w, orgReq(auth.WithIdentity(ctx, auth.Identity{UserID: "o5owner"}),
		"POST", "/api/orgs/o5/invitations", `{"email":"newcomer5@example.com","role":"owner"}`, "o5"))
	if w.Code != http.StatusOK {
		t.Fatalf("minting with no team_id into an org whose teams are all personal: code=%d body=%s, want 200",
			w.Code, w.Body.String())
	}
	var minted struct {
		TeamID string `json:"team_id"`
		Token  string `json:"token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &minted); err != nil {
		t.Fatal(err)
	}
	if minted.TeamID != "p5a" {
		t.Fatalf("the invitation joins team %q, want p5a — the org's first team, every one of them being personal",
			minted.TeamID)
	}
	joined, err := real.GetTeam(ctx, minted.TeamID)
	if err != nil {
		t.Fatal(err)
	}
	if !joined.Personal {
		t.Fatalf("team %q is not personal — then this is not the fallback the test measures", minted.TeamID)
	}
	if _, err := s.authSvc.AcceptInvitationForExistingUser(ctx, "newcomer5", minted.Token); err != nil {
		t.Fatalf("accepting it: %v", err)
	}
	if m, err := real.GetMembership(ctx, "newcomer5", "p5a"); err != nil || m.Role != identity.RoleOwner {
		t.Errorf("the team grant in the personal team is %+v (%v), want owner — the role the invitation asked for", m, err)
	}
}

// The standing is the ROW, not the claim: a token carries the org role its
// session was minted with, which a removal or a demotion since does not
// rewrite. Reading it here would hand the cap to the holder of a stale
// claim — the reason SwitchTeam reads the membership too.
func TestTeamInvitations_aTokenClaimingMoreThanTheRowDoesNotMint(t *testing.T) {
	s := newOrgCredsTestServer(t)
	seedRacerAsTeamAdmin(t, s)
	stale := auth.Identity{UserID: racerID, OrgID: "o1", OrgRole: identity.OrgRoleAdmin}

	mint := httptest.NewRecorder()
	s.handleCreateInvitation(mint, teamReq(auth.WithIdentity(context.Background(), stale),
		"POST", "/api/teams/t1/invitations", `{"email":"racer@example.com","role":"owner"}`, "t1", ""))
	if mint.Code != http.StatusForbidden {
		t.Fatalf("minting on a claim the membership row does not back: code=%d body=%s, want 403",
			mint.Code, mint.Body.String())
	}
	if body := mint.Body.String(); !strings.Contains(body, "your own org role in this team's organization is member") {
		t.Errorf("the refusal does not name the role the ROW carries: %s", body)
	}
}

// orgStandingVanishGate answers the FIRST read of one account's org
// membership and reports it gone from the second on — a removal landing
// between a gate and the act it authorized.
type orgStandingVanishGate struct {
	identity.Store
	userID, orgID string

	mu    sync.Mutex
	reads int
}

func (g *orgStandingVanishGate) GetOrgMembership(ctx context.Context, userID, orgID string) (identity.OrgMembership, error) {
	if userID != g.userID || orgID != g.orgID {
		return g.Store.GetOrgMembership(ctx, userID, orgID)
	}
	g.mu.Lock()
	g.reads++
	n := g.reads
	g.mu.Unlock()
	if n > 1 {
		return identity.OrgMembership{}, identity.ErrNotFound
	}
	return g.Store.GetOrgMembership(ctx, userID, orgID)
}

func (g *orgStandingVanishGate) readCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.reads
}

// The cap reads the standing ITSELF on the org route, where canManageOrg read
// it before the body named the team the invitation joins: a membership that
// goes away in between refuses the mint rather than carrying a standing that
// no longer exists.
func TestOrgInvitations_aStandingThatVanishesAfterTheGateRefusesTheMint(t *testing.T) {
	s := newOrgCredsTestServer(t)
	ctx := context.Background()
	real := s.authStore()
	gate := &orgStandingVanishGate{Store: real, userID: "orgadmin", orgID: "o1"}
	s.authSvc.SetStoreForTest(gate)

	w := httptest.NewRecorder()
	s.handleCreateOrgInvitation(w, orgReq(orgAdminCtx(), "POST", "/api/orgs/o1/invitations",
		`{"email":"newcomer@example.com","role":"owner","team_id":"t1"}`, "o1"))
	if w.Code != http.StatusForbidden {
		t.Fatalf("minting on a standing that vanished after the gate: code=%d body=%s, want 403",
			w.Code, w.Body.String())
	}
	if body := w.Body.String(); !strings.Contains(body, "your own org role in this team's organization is none") {
		t.Errorf("the refusal does not name the standing it read: %s", body)
	}
	if got := gate.readCount(); got != 2 {
		t.Errorf("the minter's org membership was read %d times, want 2 — the gate's read and the cap's own", got)
	}
	s.authSvc.SetStoreForTest(real)
	invs, err := real.ListInvitationsByTeam(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(invs) != 0 {
		t.Fatalf("the refused mint wrote an invitation anyway: %+v", invs)
	}
}

// What the cap does NOT reach, measured rather than claimed: a pending
// invitation carries the role it was minted with, and the mint is the only
// place the minter's standing is read. An invitation whose creator is
// off-boarded (or demoted) before it is accepted still mirrors up — the
// remedy is to revoke it, which the org admin route does, and which the
// off-boarding cascade already does for the invitations addressed TO a
// removed member.
func TestTeamInvitations_aPendingInvitationOutlivesItsCreatorsStanding(t *testing.T) {
	s := newOrgCredsTestServer(t)
	ctx := context.Background()
	real := s.authStore()
	if _, err := real.CreateUser(ctx, identity.User{
		ID: "successor", Email: "successor@example.com", Status: identity.UserStatusActive, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	mint := httptest.NewRecorder()
	s.handleCreateInvitation(mint, teamReq(orgAdminCtx(), "POST", "/api/teams/t1/invitations",
		`{"email":"successor@example.com","role":"owner"}`, "t1", ""))
	if mint.Code != http.StatusOK {
		t.Fatalf("an org admin minting an owner invitation: code=%d body=%s", mint.Code, mint.Body.String())
	}
	var minted struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(mint.Body.Bytes(), &minted); err != nil || minted.Token == "" {
		t.Fatalf("the mint answered no token: %v %s", err, mint.Body.String())
	}
	inv, err := real.GetInvitation(ctx, minted.ID)
	if err != nil {
		t.Fatal(err)
	}
	if inv.InvitedBy != "orgadmin" {
		t.Fatalf("the invitation records %q as its creator, want orgadmin", inv.InvitedBy)
	}
	// The creator leaves the org, the way a hand-over ends.
	if err := real.DeleteOrgMembership(ctx, "orgadmin", "o1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.authSvc.AcceptInvitationForExistingUser(ctx, "successor", minted.Token); err != nil {
		t.Fatalf("accepting an invitation whose creator left the org: %v", err)
	}
	om, err := real.GetOrgMembership(ctx, "successor", "o1")
	if err != nil {
		t.Fatalf("the accept wrote no org membership: %v", err)
	}
	if om.Role != identity.OrgRoleAdmin {
		t.Errorf("the accepted owner invitation mirrored up to %q, want %q", om.Role, identity.OrgRoleAdmin)
	}
	// The remedy, on the state above: a pending invitation is revoked
	// before it is accepted, by whoever manages the org now.
	second := httptest.NewRecorder()
	s.handleCreateInvitation(second, teamReq(superAdminCtx(), "POST", "/api/teams/t1/invitations",
		`{"email":"later@example.com","role":"owner"}`, "t1", ""))
	if second.Code != http.StatusOK {
		t.Fatalf("minting a second invitation: code=%d body=%s", second.Code, second.Body.String())
	}
	var later struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(second.Body.Bytes(), &later); err != nil {
		t.Fatal(err)
	}
	del := httptest.NewRecorder()
	delReq := orgReq(auth.WithIdentity(ctx, auth.Identity{UserID: "successor"}),
		"DELETE", "/api/orgs/o1/invitations/"+later.ID, "", "o1")
	delReq.SetPathValue("invite_id", later.ID)
	s.handleDeleteOrgInvitation(del, delReq)
	if del.Code != http.StatusNoContent {
		t.Fatalf("revoking the pending invitation: code=%d body=%s, want 204", del.Code, del.Body.String())
	}
	if got, err := real.GetInvitation(ctx, later.ID); err == nil {
		t.Errorf("the revoked invitation stands: %+v", got)
	}
}

// The same residual through a DEMOTION rather than a removal: the accept reads
// nothing about the creator, so an invitation minted at org admin still
// mirrors up once its creator is an ordinary member.
func TestTeamInvitations_aPendingInvitationOutlivesItsCreatorsDemotion(t *testing.T) {
	s := newOrgCredsTestServer(t)
	ctx := context.Background()
	real := s.authStore()
	if _, err := real.CreateUser(ctx, identity.User{
		ID: "newcomer", Email: "newcomer@example.com", Status: identity.UserStatusActive, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	mint := httptest.NewRecorder()
	s.handleCreateInvitation(mint, teamReq(orgAdminCtx(), "POST", "/api/teams/t1/invitations",
		`{"email":"newcomer@example.com","role":"admin"}`, "t1", ""))
	if mint.Code != http.StatusOK {
		t.Fatalf("an org admin minting an admin invitation: code=%d body=%s", mint.Code, mint.Body.String())
	}
	var minted struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(mint.Body.Bytes(), &minted); err != nil || minted.Token == "" {
		t.Fatalf("the mint answered no token: %v %s", err, mint.Body.String())
	}
	// The creator is demoted to a plain member, below the mirror they minted.
	demote := httptest.NewRecorder()
	s.handleUpdateOrgMember(demote, teamReq(superAdminCtx(), "PATCH", "/api/orgs/o1/members/orgadmin",
		`{"role":"member"}`, "o1", "orgadmin"))
	if demote.Code != http.StatusOK {
		t.Fatalf("demoting the creator: code=%d body=%s", demote.Code, demote.Body.String())
	}
	if om, err := real.GetOrgMembership(ctx, "orgadmin", "o1"); err != nil || om.Role != identity.OrgRoleMember {
		t.Fatalf("the creator is %+v (%v), want a plain member", om, err)
	}
	if _, err := s.authSvc.AcceptInvitationForExistingUser(ctx, "newcomer", minted.Token); err != nil {
		t.Fatalf("accepting an invitation whose creator was demoted: %v", err)
	}
	om, err := real.GetOrgMembership(ctx, "newcomer", "o1")
	if err != nil {
		t.Fatalf("the accept wrote no org membership: %v", err)
	}
	if om.Role != identity.OrgRoleAdmin {
		t.Errorf("the accepted admin invitation mirrored up to %q, want %q", om.Role, identity.OrgRoleAdmin)
	}
}

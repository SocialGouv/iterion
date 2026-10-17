package server

// The rank rule on deleting a TEAM container (DELETE /api/teams/{id}): the
// route's gate admits an org admin/owner or a platform admin, and the org
// ladder on top of it refuses an author over a member of the team their own
// standing does not cover — the deletion clears every grant the team holds,
// so it is a roster write over each of them at once. Every witness drives the
// real handler and asserts the ROWS the store holds afterwards, not the
// answer alone.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/identity"
)

// memberOrgRowGate fails the read of one member's org row — the read the
// deletion's rank rule measures the roster with. The author's own standing
// read is the same store call, so the gate is keyed on the MEMBER's id and
// passes every other reader.
type memberOrgRowGate struct {
	identity.Store
	userID, orgID string
}

func (g *memberOrgRowGate) GetOrgMembership(ctx context.Context, userID, orgID string) (identity.OrgMembership, error) {
	if userID == g.userID && orgID == g.orgID {
		return identity.OrgMembership{}, fmt.Errorf("member org row unreadable (injected)")
	}
	return g.Store.GetOrgMembership(ctx, userID, orgID)
}

// seedTeamInOrg seeds a team that belongs to orgID, the way the store holds
// one (seedTeam leaves the org empty — a team with no parent org).
func seedTeamInOrg(t *testing.T, s *Server, id, orgID string) {
	t.Helper()
	if _, err := s.authStore().CreateTeam(context.Background(), identity.Team{
		ID: id, Name: id, Slug: id, OrgID: orgID, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
}

func deleteTeamAs(s *Server, ctx context.Context, teamID string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	s.handleDeleteTeam(w, teamReq(ctx, "DELETE", "/api/teams/"+teamID, "", teamID, ""))
	return w
}

// The container refusal: an org admin does not delete a team that holds an
// org OWNER's grant. The team and the grant are intact after the refusal —
// the rule answers before the first revoke — and the remedy it names is one
// that works on this route (an owner tops the ladder, a platform admin is
// exempt).
func TestTeamDelete_anOrgAdminDoesNotDeleteOverAnOrgOwner(t *testing.T) {
	s := newOrgCredsTestServer(t)
	ctx := context.Background()
	seedAccount(t, s, "boss", "boss@example.com")
	seedOrgRow(t, s, "boss", "o1", identity.OrgRoleOwner)
	seedTeamGrant(t, s, "boss", "t2", identity.RoleOwner)

	w := deleteTeamAs(s, orgAdminCtx(), "t2")
	if w.Code != http.StatusForbidden {
		t.Fatalf("delete a team holding an org owner's grant: code=%d body=%s, want 403",
			w.Code, w.Body.String())
	}
	wantBodyNames(t, w.Body.String(),
		"a team is not deleted over a member whose org rank the author's own does not cover",
		"the org role of boss in this org is owner",
		"your own is admin",
		"an owner of this organization, or a platform admin, deletes this team")
	if _, err := s.authStore().GetTeam(ctx, "t2"); err != nil {
		t.Fatalf("the team was deleted despite the refusal: %v", err)
	}
	if grant, ok := teamGrantRole(t, s, "boss", "t2"); !ok || grant != identity.RoleOwner {
		t.Fatalf("the owner's grant did not survive the refusal: %q (%v)", grant, ok)
	}
}

// Equal rank and below pass: the same author deletes a team whose members
// stand at or below their own rung, and every grant goes with the team.
func TestTeamDelete_aTeamOfLowerOrEqualMembersDeletesItsGrants(t *testing.T) {
	s := newOrgCredsTestServer(t)
	ctx := context.Background()
	seedRacer(t, s) // org member of o1
	seedTeamGrant(t, s, racerID, "t2", identity.RoleMember)
	seedTeamGrant(t, s, "orgadmin", "t2", identity.RoleAdmin) // the author's own row, equal rank

	w := deleteTeamAs(s, orgAdminCtx(), "t2")
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete a team of lower or equal members: code=%d body=%s, want 204",
			w.Code, w.Body.String())
	}
	if _, err := s.authStore().GetTeam(ctx, "t2"); err == nil {
		t.Fatal("the team survived a 204 delete")
	}
	for _, uid := range []string{racerID, "orgadmin"} {
		if _, ok := teamGrantRole(t, s, uid, "t2"); ok {
			t.Errorf("the grant of %s survived its team", uid)
		}
	}
}

// A team with no member at all deletes — nothing for the ladder to compare.
func TestTeamDelete_anEmptyTeamDeletes(t *testing.T) {
	s := newOrgCredsTestServer(t)
	seedTeamInOrg(t, s, "t3", "o1")

	w := deleteTeamAs(s, orgAdminCtx(), "t3")
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete an empty team: code=%d body=%s, want 204", w.Code, w.Body.String())
	}
}

// The remedy works on its route: an org OWNER deletes the very team the org
// admin was refused — nobody sits above an owner, their own row included.
func TestTeamDelete_anOrgOwnerDeletesTheTeamTheAdminWasRefused(t *testing.T) {
	s := newOrgCredsTestServer(t)
	seedAccount(t, s, "boss", "boss@example.com")
	seedOrgRow(t, s, "boss", "o1", identity.OrgRoleOwner)
	seedTeamGrant(t, s, "boss", "t2", identity.RoleOwner)

	if w := deleteTeamAs(s, orgAdminCtx(), "t2"); w.Code != http.StatusForbidden {
		t.Fatalf("the admin was not refused first — then this is not the pair the test describes: %d %s",
			w.Code, w.Body.String())
	}
	w := deleteTeamAs(s, memberCtx("boss"), "t2")
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete the same team as its org owner: code=%d body=%s, want 204",
			w.Code, w.Body.String())
	}
	if _, err := s.authStore().GetTeam(context.Background(), "t2"); err == nil {
		t.Fatal("the team survived the owner's 204 delete")
	}
}

// Platform admins are exempt: they hold no membership row for a ladder to
// place them on, and the route admits them at its gate.
func TestTeamDelete_aPlatformAdminDeletesOverAnyone(t *testing.T) {
	s := newOrgCredsTestServer(t)
	seedAccount(t, s, "boss", "boss@example.com")
	seedOrgRow(t, s, "boss", "o1", identity.OrgRoleOwner)
	seedTeamGrant(t, s, "boss", "t2", identity.RoleOwner)

	w := deleteTeamAs(s, superAdminCtx(), "t2")
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete as a platform admin: code=%d body=%s, want 204", w.Code, w.Body.String())
	}
	if _, err := s.authStore().GetTeam(context.Background(), "t2"); err == nil {
		t.Fatal("the team survived the platform admin's 204 delete")
	}
}

// Fail closed BEFORE any side effect: a member's org row that cannot be READ
// is not one that stands below every rung, so the deletion answers 500 naming
// that read, and the team and every grant are as they were.
func TestTeamDelete_anUnreadableMemberOrgRowRefusesBeforeAnySideEffect(t *testing.T) {
	s := newOrgCredsTestServer(t)
	ctx := context.Background()
	seedRacer(t, s)
	seedTeamGrant(t, s, racerID, "t2", identity.RoleMember)
	real := s.authStore()
	s.authSvc.SetStoreForTest(&memberOrgRowGate{Store: real, userID: racerID, orgID: "o1"})
	defer s.authSvc.SetStoreForTest(real)

	w := deleteTeamAs(s, orgAdminCtx(), "t2")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("delete over an unreadable member org row: code=%d body=%s, want 500",
			w.Code, w.Body.String())
	}
	wantBodyNames(t, w.Body.String(),
		"resolve the org membership of "+racerID)
	if _, err := s.authStore().GetTeam(ctx, "t2"); err != nil {
		t.Fatalf("the team was deleted despite the unreadable read: %v", err)
	}
	if _, ok := teamGrantRole(t, s, racerID, "t2"); !ok {
		t.Fatal("the member's grant did not survive the unreadable read")
	}
}

// A team with NO parent org answers at the gate, and the gate refuses
// EVERYONE there: the route's explicit OrgID disjunct fires before
// canManageOrg runs and before the platform-admin exemption is reached, so
// the org's data path (the super-admin org deletion) owns the orphan team —
// nobody deletes it from this route, and the team and its grants survive
// every refusal.
func TestTeamDelete_anOrphanTeamIsDeletedByNobodyFromThisRoute(t *testing.T) {
	s := newOrgCredsTestServer(t)
	seedTeam(t, s, "orphan", "orphan")
	seedAccount(t, s, "boss", "boss@example.com")
	seedTeamGrant(t, s, "boss", "orphan", identity.RoleOwner)

	for _, tc := range []struct {
		name string
		ctx  context.Context
	}{
		{"an org admin of another org", orgAdminCtx()},
		{"the orphan team's own owner", memberCtx("boss")},
		{"a platform admin", superAdminCtx()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := deleteTeamAs(s, tc.ctx, "orphan")
			if w.Code != http.StatusForbidden {
				t.Fatalf("delete an orphan team as %s: code=%d body=%s, want 403",
					tc.name, w.Code, w.Body.String())
			}
			wantBodyNames(t, w.Body.String(), "org admin or owner required")
			if _, err := s.authStore().GetTeam(context.Background(), "orphan"); err != nil {
				t.Fatalf("the orphan team was deleted despite the refusal: %v", err)
			}
			if grant, ok := teamGrantRole(t, s, "boss", "orphan"); !ok || grant != identity.RoleOwner {
				t.Fatalf("the orphan team's owner grant did not survive the refusal: %q (%v)", grant, ok)
			}
		})
	}
}

// Over a rank this build cannot PLACE, the owner standing the ordinary
// refusal names does not work: OrgRole.AtLeast is false on both sides of rank
// zero, so even an owner is refused there. The refusal names the one remedy
// that works — a platform admin, who reaches the row — and never the owner
// standing an author would execute and be refused by again. The named remedy
// is then executed on the same team, and it lands.
func TestTeamDelete_anUnplaceableMemberRankNamesAPlatformAdminAlone(t *testing.T) {
	s := newOrgCredsTestServer(t)
	ctx := context.Background()
	seedRacer(t, s)
	seedTeamGrant(t, s, racerID, "t2", identity.RoleMember)
	real := s.authStore()
	s.authSvc.SetStoreForTest(&unrankableRowGate{Store: real, userID: racerID, orgID: "o1", role: "archon"})
	defer s.authSvc.SetStoreForTest(real)

	w := deleteTeamAs(s, orgAdminCtx(), "t2")
	if w.Code != http.StatusForbidden {
		t.Fatalf("delete over an unplaceable member rank: code=%d body=%s, want 403",
			w.Code, w.Body.String())
	}
	wantBodyNames(t, w.Body.String(),
		"the org role of "+racerID+" in this org is archon",
		"a platform admin deletes this team")
	wantBodyLacks(t, w.Body.String(), "an owner of this organization")
	if _, err := s.authStore().GetTeam(ctx, "t2"); err != nil {
		t.Fatalf("the team was deleted despite the refusal: %v", err)
	}
	if grant, ok := teamGrantRole(t, s, racerID, "t2"); !ok || grant != identity.RoleMember {
		t.Fatalf("the member's grant did not survive the refusal: %q (%v)", grant, ok)
	}

	// The remedy the refusal names is one that works: the platform admin
	// deletes the very team the org admin was refused.
	if super := deleteTeamAs(s, superAdminCtx(), "t2"); super.Code != http.StatusNoContent {
		t.Fatalf("the named remedy on the same team: code=%d body=%s, want 204",
			super.Code, super.Body.String())
	}
	if _, err := s.authStore().GetTeam(ctx, "t2"); err == nil {
		t.Fatal("the team survived the platform admin's 204 delete")
	}
}

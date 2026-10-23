package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
)

// listTeamMembers drives GET /api/teams/ta/members for one caller.
func listTeamMembers(f *authRouteFixture, caller auth.Identity) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", "/api/teams/ta/members", nil)
	r.SetPathValue("id", "ta")
	r = r.WithContext(auth.WithIdentity(context.Background(), caller))
	w := httptest.NewRecorder()
	f.s.handleListTeamMembers(w, r)
	return w
}

// newMemberListFixture is the auth-route fixture with GetMembership gated
// (the read the authorization makes) and one account that holds nothing: no
// grant in team ta, no standing in its org o1.
func newMemberListFixture(t *testing.T) (*authRouteFixture, auth.Identity) {
	t.Helper()
	f := newAuthRouteFixture(t)
	f.svc.SetStoreForTest(&storeGatePlus{storeGate: f.ids})
	seedActiveUser(t, f.s, "u-out", "out@example.com")
	return f, auth.Identity{UserID: "u-out", TeamID: "t-elsewhere", Role: identity.RoleOwner}
}

// TestHandleListTeamMembers_ARefusalRestingOnAFailedReadIsAFailure: the
// member list's authorization resolves up to three rows — the caller's grant
// in the team, the team's parent org, the caller's standing in that org — and
// a "no" that rests on a read the store could not COMPLETE is not "not a
// member". The route's contract (docs/cloud-rest-api.md) says a store failure
// answers the generic 500 and is logged while a refusal keeps its own status,
// so the gate hands THIS route the error that stopped it instead of the
// fail-closed false every other caller of these helpers still takes.
func TestHandleListTeamMembers_ARefusalRestingOnAFailedReadIsAFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		read string
	}{
		{"the caller's grant in the team", "GetMembership"},
		{"the team's parent org", "GetTeam"},
		{"the caller's standing in the team's org", "GetOrgMembership"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, caller := newMemberListFixture(t)
			f.ids.fail(tc.read, 0)
			w := listTeamMembers(f, caller)
			assertIdentityUnavailable(t, w, f.logs, "the member list whose read of "+tc.name+" failed")
		})
	}
}

// TestHandleListTeamMembers_ANonMemberIsStillRefused is the other half: with
// the store answering, a caller who holds nothing in the team and nothing in
// its org is refused 403 with its own message, and writes no ERROR line —
// which an operator reads as a store outage.
func TestHandleListTeamMembers_ANonMemberIsStillRefused(t *testing.T) {
	f, caller := newMemberListFixture(t)
	w := listTeamMembers(f, caller)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "not a member") {
		t.Fatalf("a non-member's member list answered %d %s, want 403 %q", w.Code, strings.TrimSpace(w.Body.String()), "not a member")
	}
	if logged := f.logs.String(); logged != "" {
		t.Fatalf("a refusal wrote to the server log: %q", logged)
	}
}

// TestHandleListTeamMembers_AFailedReadBesideARowThatGrantsChangesNothing:
// only a refusal carries the failure. A caller the ORG makes an admin of
// every team in it is entitled whatever their team grant says, so a failed
// read of that grant is not a verdict the server has to withhold — it
// answers the list, and logs nothing.
func TestHandleListTeamMembers_AFailedReadBesideARowThatGrantsChangesNothing(t *testing.T) {
	f := newAuthRouteFixture(t)
	f.svc.SetStoreForTest(&storeGatePlus{storeGate: f.ids})
	// The fixture's account is an ADMIN of org o1, which holds ta.
	f.ids.fail("GetMembership", 0)
	w := listTeamMembers(f, auth.Identity{UserID: f.user.User.ID, TeamID: "t-elsewhere", Role: identity.RoleOwner})
	if w.Code != http.StatusOK {
		t.Fatalf("an org admin's member list answered %d %s, want 200", w.Code, strings.TrimSpace(w.Body.String()))
	}
	var body struct {
		Members []struct {
			UserID string `json:"user_id"`
		} `json:"members"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || len(body.Members) == 0 {
		t.Fatalf("the member list answered %s (err %v)", w.Body.String(), err)
	}
	if logged := f.logs.String(); logged != "" {
		t.Fatalf("a granted read wrote to the server log: %q", logged)
	}
}

// TestCanViewTeam_AFailedReadStaysFailClosedForEveryOtherCaller pins the
// posture the bool helpers keep: they are gates, and a read they could not
// complete grants nothing. The (bool, error) form is what reports WHY, for
// the one route whose contract promises it; every other caller of
// canViewTeam / canManageOrg / orgAdminOfTeam keeps the fail-closed false
// (r2-fix class inventory §3: turning all of them is its own change).
func TestCanViewTeam_AFailedReadStaysFailClosedForEveryOtherCaller(t *testing.T) {
	f := newAuthRouteFixture(t)
	f.svc.SetStoreForTest(&storeGatePlus{storeGate: f.ids})
	ctx := context.Background()
	member := auth.Identity{UserID: f.user.User.ID, TeamID: "t-elsewhere", Role: identity.RoleOwner}
	if !f.s.canViewTeam(ctx, member, "ta") {
		t.Fatalf("a member of ta cannot view it with the store answering")
	}
	f.ids.fail("GetMembership", 0)
	f.ids.fail("GetTeam", 0)
	f.ids.fail("GetOrgMembership", 0)
	if f.s.canViewTeam(ctx, member, "ta") {
		t.Fatalf("canViewTeam granted access from reads it could not complete")
	}
	if f.s.canManageOrg(ctx, member, "o1") {
		t.Fatalf("canManageOrg granted management from a read it could not complete")
	}
	if f.s.orgAdminOfTeam(ctx, member, "ta") {
		t.Fatalf("orgAdminOfTeam granted management from a read it could not complete")
	}
	if f.s.canManageTeam(ctx, member, "ta") {
		t.Fatalf("canManageTeam granted management from reads it could not complete")
	}
}

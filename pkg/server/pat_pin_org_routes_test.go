package server

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/audit"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/identity"
)

// pinnedOrgFixture is the invitation fixture plus a sibling team tb in o1:
// the personal access token is pinned to ta, and its account is an admin of
// o1, so the org routes let it in and the pin alone keeps it out of tb.
func pinnedOrgFixture(t *testing.T) (*derivationFixture, string) {
	t.Helper()
	f, patToken := invitationMailFixture(t, nil)
	if _, err := f.s.authStore().CreateTeam(context.Background(), identity.Team{ID: "tb", Name: "TB", Slug: "tb", OrgID: "o1"}); err != nil {
		t.Fatalf("create sibling team tb: %v", err)
	}
	return f, patToken
}

func maxConcurrentRunsOf(t *testing.T, f *derivationFixture, teamID string) int {
	t.Helper()
	team, err := f.s.authStore().GetTeam(context.Background(), teamID)
	if err != nil {
		t.Fatalf("read team %s: %v", teamID, err)
	}
	return team.MaxConcurrentRuns
}

// TestOrgTeamCaps_APinnedTokenChangesItsOwnTeamsCapsOnly: through the org
// route, a token pinned to ta sets ta's executor caps and is refused on a
// sibling team, with the same answer whether that team exists or not.
func TestOrgTeamCaps_APinnedTokenChangesItsOwnTeamsCapsOnly(t *testing.T) {
	f, patToken := pinnedOrgFixture(t)
	for _, team := range []string{"tb", "no-such-team"} {
		code, _, body := f.call(t, http.MethodPatch, "/api/orgs/o1/teams/"+team+"/caps", patToken, `{"max_concurrent_runs":7}`)
		if code != http.StatusForbidden || !strings.Contains(body, "pinned to team ta") {
			t.Fatalf("a ta-pinned PAT setting the caps of %s: %d %s, want 403 naming the pin", team, code, body)
		}
	}
	if n := maxConcurrentRunsOf(t, f, "tb"); n != 0 {
		t.Fatalf("tb's max_concurrent_runs = %d after the refused PATCH, want 0", n)
	}
	code, _, body := f.call(t, http.MethodPatch, "/api/orgs/o1/teams/ta/caps", patToken, `{"max_concurrent_runs":7}`)
	if code != http.StatusOK {
		t.Fatalf("a ta-pinned PAT setting ta's caps: %d %s, want 200", code, body)
	}
	if n := maxConcurrentRunsOf(t, f, "ta"); n != 7 {
		t.Fatalf("ta's max_concurrent_runs = %d, want 7", n)
	}
	// The session of the same org admin pins nothing.
	if code, _, body := f.call(t, http.MethodPatch, "/api/orgs/o1/teams/tb/caps", f.user.AccessToken, `{"max_concurrent_runs":3}`); code != http.StatusOK {
		t.Fatalf("the org admin's session setting tb's caps: %d %s, want 200", code, body)
	}
}

// TestProvisionApprovals_APinnedTokenDecidesItsOwnTeamsRequestsOnly: through
// the org routes, a token pinned to ta lists, approves and rejects ta's
// provisioning requests only. A sibling team's request is one it cannot read.
func TestProvisionApprovals_APinnedTokenDecidesItsOwnTeamsRequestsOnly(t *testing.T) {
	f, patToken := pinnedOrgFixture(t)
	store := forge.NewMemoryProvisionApprovalStore()
	f.s.provisionApprovals = store
	// The server registers these routes with a forge orchestrator, which
	// this fixture does not build; the handlers need only the store, and
	// reject mints nothing.
	f.s.registerForgeApprovalRoutes()
	ctx := context.Background()
	for _, a := range []forge.ProvisionApproval{
		{ID: "req-ta", OrgID: "o1", TenantID: "ta", RepoFullName: "group/own"},
		{ID: "req-tb", OrgID: "o1", TenantID: "tb", RepoFullName: "group/sibling"},
	} {
		if err := store.Create(ctx, a); err != nil {
			t.Fatalf("seed %s: %v", a.ID, err)
		}
	}
	code, _, body := f.call(t, http.MethodGet, "/api/orgs/o1/provision-approvals", patToken, "")
	if code != http.StatusOK || !strings.Contains(body, "req-ta") || strings.Contains(body, "req-tb") {
		t.Fatalf("a ta-pinned PAT listing the org's requests: %d %s, want ta's only", code, body)
	}
	code, _, body = f.call(t, http.MethodGet, "/api/orgs/o1/provision-approvals", f.user.AccessToken, "")
	if code != http.StatusOK || !strings.Contains(body, "req-ta") || !strings.Contains(body, "req-tb") {
		t.Fatalf("the org admin's session listing the org's requests: %d %s, want both", code, body)
	}
	for _, verb := range []string{"approve", "reject"} {
		code, _, body := f.call(t, http.MethodPost, "/api/orgs/o1/provision-approvals/req-tb/"+verb, patToken, "")
		if code != http.StatusNotFound {
			t.Fatalf("a ta-pinned PAT asked to %s tb's request: %d %s, want 404", verb, code, body)
		}
		if _, err := store.Get(ctx, "req-tb"); err != nil {
			t.Fatalf("tb's request is gone after a refused %s: %v", verb, err)
		}
	}
	// Its own team's request passes the pin: the approve reaches the
	// orchestrator, which this fixture does not build.
	if code, _, body := f.call(t, http.MethodPost, "/api/orgs/o1/provision-approvals/req-ta/approve", patToken, ""); code != http.StatusServiceUnavailable {
		t.Fatalf("a ta-pinned PAT approving ta's request: %d %s, want the 503 of the missing orchestrator, past the pin", code, body)
	}
	if code, _, body := f.call(t, http.MethodPost, "/api/orgs/o1/provision-approvals/req-ta/reject", patToken, ""); code != http.StatusNoContent {
		t.Fatalf("a ta-pinned PAT rejecting ta's request: %d %s, want 204", code, body)
	}
	if _, err := store.Get(ctx, "req-ta"); err == nil {
		t.Fatalf("ta's request survived its reject")
	}
}

// TestOrgAudit_APinnedTokenReadsNoSiblingTeamsRows: the org trail keeps,
// from a token pinned to ta, the rows about sibling team tb — the invitation
// into it and the change of its caps — which the org's lists and the team
// routes keep from it already. The org admin's session reads them all, and
// the page offset still counts the whole trail.
func TestOrgAudit_APinnedTokenReadsNoSiblingTeamsRows(t *testing.T) {
	f, patToken := pinnedOrgFixture(t)
	f.s.auditStore = audit.NewMemoryStore()
	f.s.registerAuditRoutes()
	for _, team := range []string{"ta", "tb"} {
		if code, _, body := f.call(t, http.MethodPost, "/api/orgs/o1/invitations", f.user.AccessToken,
			`{"email":"`+team+`-invitee@example.com","role":"member","team_id":"`+team+`"}`); code != http.StatusOK {
			t.Fatalf("a session's invitation into %s: %d %s", team, code, body)
		}
	}
	if code, _, body := f.call(t, http.MethodPatch, "/api/orgs/o1/teams/tb/caps", f.user.AccessToken, `{"max_concurrent_runs":3}`); code != http.StatusOK {
		t.Fatalf("the session setting tb's caps: %d %s", code, body)
	}
	waitForTenantAuditRows(t, f.s.auditStore, "o1", "team.caps_updated", 1)
	waitForTenantAuditRows(t, f.s.auditStore, "o1", "org_invitation.created", 2)

	code, _, all := f.call(t, http.MethodGet, "/api/orgs/o1/audit", f.user.AccessToken, "")
	if code != http.StatusOK || !strings.Contains(all, "tb-invitee@example.com") || !strings.Contains(all, "team.caps_updated") {
		t.Fatalf("the org admin's session reading the org trail: %d %s, want tb's rows", code, all)
	}
	code, _, pinned := f.call(t, http.MethodGet, "/api/orgs/o1/audit", patToken, "")
	if code != http.StatusOK || !strings.Contains(pinned, "ta-invitee@example.com") {
		t.Fatalf("a ta-pinned PAT reading the org trail: %d %s, want ta's rows", code, pinned)
	}
	if strings.Contains(pinned, "tb-invitee@example.com") || strings.Contains(pinned, "team.caps_updated") {
		t.Fatalf("a ta-pinned PAT read sibling team tb's rows from the org trail: %s", pinned)
	}
	if !strings.Contains(pinned, `"next_offset":3`) {
		t.Fatalf("the pinned page's next_offset does not count the whole trail (3 rows): %s", pinned)
	}
}

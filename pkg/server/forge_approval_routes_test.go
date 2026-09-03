package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/audit"
	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// newApprovalTestServer builds the forge test server with the approval
// store wired plus an org (flag ON) owning team t1, a team admin (not
// org admin) and an org admin.
func newApprovalTestServer(t *testing.T) (*Server, *mockGitLab, func()) {
	t.Helper()
	gl := newMockGitLab()
	srv := gl.server()
	s := newForgeTestServer(t)
	s.provisionApprovals = forge.NewMemoryProvisionApprovalStore()
	s.auditStore = audit.NewMemoryStore()
	ctx := context.Background()
	if _, err := s.authStore().CreateOrg(ctx, identity.Org{
		ID: "o1", Name: "o1", Slug: "o1", RequireProvisionApproval: true, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.authStore().CreateTeam(ctx, identity.Team{
		ID: "t1", Name: "t1", Slug: "t1", OrgID: "o1", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.authStore().UpsertMembership(ctx, identity.Membership{
		UserID: "teamadmin", TeamID: "t1", Role: identity.RoleAdmin,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.authStore().UpsertOrgMembership(ctx, identity.OrgMembership{
		UserID: "orgadmin", OrgID: "o1", Role: identity.OrgRoleAdmin,
	}); err != nil {
		t.Fatal(err)
	}
	// Point connect at the mock GitLab and open a PAT connection as the
	// team admin (connections are not gated by the approval flow).
	w := httptest.NewRecorder()
	body := `{"provider":"gitlab","mode":"pat","forge_base_url":"` + srv.URL + `","pat":"glpat-token"}`
	s.handleConnectForge(w, forgeReq(teamAdminCtx(), "POST", "/api/teams/t1/forge/connections", body, "t1"))
	if w.Code != http.StatusOK {
		t.Fatalf("connect: code=%d body=%s", w.Code, w.Body.String())
	}
	return s, gl, srv.Close
}

func teamAdminCtx() context.Context {
	return auth.WithIdentity(context.Background(), auth.Identity{UserID: "teamadmin"})
}

func orgAdminCtx() context.Context {
	return auth.WithIdentity(context.Background(), auth.Identity{UserID: "orgadmin"})
}

func firstConnID(t *testing.T, s *Server) string {
	t.Helper()
	conns, err := s.forgeConnections.ListByTenant(context.Background(), "t1")
	if err != nil || len(conns) == 0 {
		t.Fatalf("no connection: %v", err)
	}
	return conns[0].ID
}

func enableBody(connID string) string {
	return `{"connection_id":"` + connID + `","repo":"group/api","bot_ids":["review-pr"]}`
}

// A team admin's enable on an approval-required org parks the request:
// 202, nothing provisioned, the approval visible org- and team-side;
// the org admin's approve then executes the exact request.
func TestProvisionApproval_ParkThenApprove(t *testing.T) {
	s, gl, done := newApprovalTestServer(t)
	defer done()
	connID := firstConnID(t, s)

	w := httptest.NewRecorder()
	s.handleEnableForgeRepoBots(w, forgeReq(teamAdminCtx(), "POST", "/api/teams/t1/forge/repo-bots", enableBody(connID), "t1"))
	if w.Code != http.StatusAccepted {
		t.Fatalf("enable should park: code=%d body=%s", w.Code, w.Body.String())
	}
	var parked struct {
		PendingApproval bool   `json:"pending_approval"`
		ApprovalID      string `json:"approval_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &parked); err != nil || !parked.PendingApproval || parked.ApprovalID == "" {
		t.Fatalf("bad parked response: %s (%v)", w.Body.String(), err)
	}
	// Zero forge-side surface while pending.
	gl.mu.Lock()
	hooked := len(gl.hooks)
	gl.mu.Unlock()
	if hooked != 0 {
		t.Fatalf("forge hook created while pending: %d", hooked)
	}
	if ints, _ := s.forgeIntegrations.ListByTenant(context.Background(), "t1"); len(ints) != 0 {
		t.Fatalf("integration created while pending: %d", len(ints))
	}

	// Visible in the org queue and the team list.
	w = httptest.NewRecorder()
	s.handleListOrgProvisionApprovals(w, orgReq(orgAdminCtx(), "GET", "/api/orgs/o1/provision-approvals", "", "o1"))
	if w.Code != http.StatusOK {
		t.Fatalf("org list: code=%d body=%s", w.Code, w.Body.String())
	}
	var lr struct {
		Approvals []forge.ProvisionApproval `json:"approvals"`
	}
	json.Unmarshal(w.Body.Bytes(), &lr)
	if len(lr.Approvals) != 1 || lr.Approvals[0].RequestedBy != "teamadmin" {
		t.Fatalf("org list wrong: %+v", lr.Approvals)
	}
	w = httptest.NewRecorder()
	s.handleListTeamProvisionApprovals(w, forgeReq(teamAdminCtx(), "GET", "/api/teams/t1/provision-approvals", "", "t1"))
	json.Unmarshal(w.Body.Bytes(), &lr)
	if len(lr.Approvals) != 1 {
		t.Fatalf("team list wrong: %+v", lr.Approvals)
	}

	// A duplicate submit while pending is refused, not queued twice.
	w = httptest.NewRecorder()
	s.handleEnableForgeRepoBots(w, forgeReq(teamAdminCtx(), "POST", "/api/teams/t1/forge/repo-bots", enableBody(connID), "t1"))
	if w.Code != http.StatusConflict {
		t.Fatalf("duplicate submit: code=%d body=%s", w.Code, w.Body.String())
	}

	// The team admin cannot approve (org admin required).
	w = httptest.NewRecorder()
	req := orgReq(teamAdminCtx(), "POST", "/api/orgs/o1/provision-approvals/"+parked.ApprovalID+"/approve", "", "o1")
	req.SetPathValue("approval_id", parked.ApprovalID)
	s.handleApproveProvision(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("team admin approve should 403: code=%d", w.Code)
	}

	// Org admin approves → the provision executes for real.
	w = httptest.NewRecorder()
	req = orgReq(orgAdminCtx(), "POST", "/api/orgs/o1/provision-approvals/"+parked.ApprovalID+"/approve", "", "o1")
	req.SetPathValue("approval_id", parked.ApprovalID)
	s.handleApproveProvision(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("approve: code=%d body=%s", w.Code, w.Body.String())
	}
	var res forge.ProvisionResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil || res.IntegrationID == "" {
		t.Fatalf("approve result: %s (%v)", w.Body.String(), err)
	}
	if ints, _ := s.forgeIntegrations.ListByTenant(context.Background(), "t1"); len(ints) != 1 {
		t.Fatalf("integration not created on approve: %d", len(ints))
	}
	if list, _ := s.provisionApprovals.ListByOrg(context.Background(), "o1"); len(list) != 0 {
		t.Fatalf("approval not consumed: %d", len(list))
	}
}

func TestProvisionApproval_Reject(t *testing.T) {
	s, _, done := newApprovalTestServer(t)
	defer done()
	connID := firstConnID(t, s)

	w := httptest.NewRecorder()
	s.handleEnableForgeRepoBots(w, forgeReq(teamAdminCtx(), "POST", "/api/teams/t1/forge/repo-bots", enableBody(connID), "t1"))
	if w.Code != http.StatusAccepted {
		t.Fatalf("enable should park: code=%d", w.Code)
	}
	var parked struct {
		ApprovalID string `json:"approval_id"`
	}
	json.Unmarshal(w.Body.Bytes(), &parked)

	w = httptest.NewRecorder()
	req := orgReq(orgAdminCtx(), "POST", "/api/orgs/o1/provision-approvals/"+parked.ApprovalID+"/reject", `{"reason":"not this repo"}`, "o1")
	req.SetPathValue("approval_id", parked.ApprovalID)
	s.handleRejectProvision(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("reject: code=%d body=%s", w.Code, w.Body.String())
	}
	if list, _ := s.provisionApprovals.ListByOrg(context.Background(), "o1"); len(list) != 0 {
		t.Fatalf("approval not removed: %d", len(list))
	}
	if ints, _ := s.forgeIntegrations.ListByTenant(context.Background(), "t1"); len(ints) != 0 {
		t.Fatalf("integration created on reject: %d", len(ints))
	}
}

// An org admin (who holds the approval right) provisions directly even
// when the org requires approval — the gate is for those who cannot
// approve themselves.
func TestProvisionApproval_OrgAdminBypasses(t *testing.T) {
	s, _, done := newApprovalTestServer(t)
	defer done()
	connID := firstConnID(t, s)

	w := httptest.NewRecorder()
	s.handleEnableForgeRepoBots(w, forgeReq(orgAdminCtx(), "POST", "/api/teams/t1/forge/repo-bots", enableBody(connID), "t1"))
	if w.Code != http.StatusOK {
		t.Fatalf("org admin enable should provision directly: code=%d body=%s", w.Code, w.Body.String())
	}
	if ints, _ := s.forgeIntegrations.ListByTenant(context.Background(), "t1"); len(ints) != 1 {
		t.Fatalf("integration missing: %d", len(ints))
	}
}

// Bot-set UPDATES only need approval when they EXPAND the surface; a
// removal goes through directly.
func TestProvisionApproval_UpdateExpansionOnly(t *testing.T) {
	s, _, done := newApprovalTestServer(t)
	defer done()
	connID := firstConnID(t, s)

	// Provision review-pr directly as the org admin.
	w := httptest.NewRecorder()
	s.handleEnableForgeRepoBots(w, forgeReq(orgAdminCtx(), "POST", "/api/teams/t1/forge/repo-bots", enableBody(connID), "t1"))
	if w.Code != http.StatusOK {
		t.Fatalf("seed enable: code=%d body=%s", w.Code, w.Body.String())
	}
	var res forge.ProvisionResult
	json.Unmarshal(w.Body.Bytes(), &res)

	// Team admin re-submitting the SAME set is not an expansion → direct.
	w = httptest.NewRecorder()
	req := forgeReq(teamAdminCtx(), "PATCH", "/api/teams/t1/forge/repo-bots/"+res.IntegrationID, `{"bot_ids":["review-pr"]}`, "t1")
	req.SetPathValue("integration_id", res.IntegrationID)
	s.handleUpdateForgeRepoBots(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("same-set update should be direct: code=%d body=%s", w.Code, w.Body.String())
	}

	// Turning the zero-touch fixer lane ON expands the automated surface
	// even with the bot set unchanged → parked (gate-bypass regression).
	w = httptest.NewRecorder()
	req = forgeReq(teamAdminCtx(), "PATCH", "/api/teams/t1/forge/repo-bots/"+res.IntegrationID, `{"bot_ids":["review-pr"],"auto_fix_on_gate_failure":true}`, "t1")
	req.SetPathValue("integration_id", res.IntegrationID)
	s.handleUpdateForgeRepoBots(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("auto-fix ON should park: code=%d body=%s", w.Code, w.Body.String())
	}
	if err := s.provisionApprovals.Delete(context.Background(), approvalIDFrom(t, w)); err != nil {
		t.Fatal(err)
	}
	// Turning it OFF (or leaving it) tightens → direct.
	w = httptest.NewRecorder()
	req = forgeReq(teamAdminCtx(), "PATCH", "/api/teams/t1/forge/repo-bots/"+res.IntegrationID, `{"bot_ids":["review-pr"],"auto_fix_on_gate_failure":false}`, "t1")
	req.SetPathValue("integration_id", res.IntegrationID)
	s.handleUpdateForgeRepoBots(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("auto-fix OFF should be direct: code=%d body=%s", w.Code, w.Body.String())
	}

	// A schedule change arms unattended recurring launches → parked, even
	// with the bot set unchanged.
	w = httptest.NewRecorder()
	req = forgeReq(teamAdminCtx(), "PATCH", "/api/teams/t1/forge/repo-bots/"+res.IntegrationID, `{"bot_ids":["review-pr"],"schedule_crons":{"review-pr":"0 3 * * *"}}`, "t1")
	req.SetPathValue("integration_id", res.IntegrationID)
	s.handleUpdateForgeRepoBots(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("schedule change should park: code=%d body=%s", w.Code, w.Body.String())
	}
	if err := s.provisionApprovals.Delete(context.Background(), approvalIDFrom(t, w)); err != nil {
		t.Fatal(err)
	}

	// Adding a bot expands → parked.
	w = httptest.NewRecorder()
	req = forgeReq(teamAdminCtx(), "PATCH", "/api/teams/t1/forge/repo-bots/"+res.IntegrationID, `{"bot_ids":["review-pr","dep-guard"]}`, "t1")
	req.SetPathValue("integration_id", res.IntegrationID)
	s.handleUpdateForgeRepoBots(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("expanding update should park: code=%d body=%s", w.Code, w.Body.String())
	}
	// The live integration keeps serving its current bot set meanwhile.
	ri, err := s.forgeIntegrations.Get(context.Background(), res.IntegrationID)
	if err != nil || len(ri.BotIDs) != 1 || ri.BotIDs[0] != "review-pr" {
		t.Fatalf("live integration mutated while pending: %+v (%v)", ri.BotIDs, err)
	}
}

func TestOrgSettings_RequireProvisionApproval(t *testing.T) {
	s, _, done := newApprovalTestServer(t)
	defer done()

	// Team admin cannot flip the org setting.
	w := httptest.NewRecorder()
	s.handleUpdateOrgSettings(w, orgReq(teamAdminCtx(), "PATCH", "/api/orgs/o1/settings", `{"require_provision_approval":false}`, "o1"))
	if w.Code != http.StatusForbidden {
		t.Fatalf("team admin settings patch should 403: code=%d", w.Code)
	}

	// Org admin flips it off; the next team-admin enable is direct.
	w = httptest.NewRecorder()
	s.handleUpdateOrgSettings(w, orgReq(orgAdminCtx(), "PATCH", "/api/orgs/o1/settings", `{"require_provision_approval":false}`, "o1"))
	if w.Code != http.StatusOK {
		t.Fatalf("settings patch: code=%d body=%s", w.Code, w.Body.String())
	}
	var view orgSettingsView
	json.Unmarshal(w.Body.Bytes(), &view)
	if view.RequireProvisionApproval {
		t.Fatal("flag should be off")
	}
	connID := firstConnID(t, s)
	w = httptest.NewRecorder()
	s.handleEnableForgeRepoBots(w, forgeReq(teamAdminCtx(), "POST", "/api/teams/t1/forge/repo-bots", enableBody(connID), "t1"))
	if w.Code != http.StatusOK {
		t.Fatalf("flag off: enable should be direct: code=%d body=%s", w.Code, w.Body.String())
	}
}

// The gate FAILS CLOSED: when the team's parent org cannot be resolved
// (store error — here a dangling OrgID), the request is refused (503),
// never provisioned as if no approval were required.
func TestProvisionApproval_GateFailsClosed(t *testing.T) {
	s, gl, done := newApprovalTestServer(t)
	defer done()
	connID := firstConnID(t, s)

	tm, err := s.authStore().GetTeam(context.Background(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	tm.OrgID = "ghost-org" // GetOrg will error
	if err := s.authStore().UpdateTeam(context.Background(), tm); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	s.handleEnableForgeRepoBots(w, forgeReq(teamAdminCtx(), "POST", "/api/teams/t1/forge/repo-bots", enableBody(connID), "t1"))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("unresolvable org should 503 (fail closed), got %d: %s", w.Code, w.Body.String())
	}
	gl.mu.Lock()
	hooked := len(gl.hooks)
	gl.mu.Unlock()
	if hooked != 0 {
		t.Fatalf("provisioned despite gate failure: %d hooks", hooked)
	}
	if ints, _ := s.forgeIntegrations.ListByTenant(context.Background(), "t1"); len(ints) != 0 {
		t.Fatalf("integration created despite gate failure: %d", len(ints))
	}
}

// Approving a request whose target the team tore down meanwhile must not
// resurrect it: the approve answers 409 and the record stays for reject.
func TestProvisionApproval_StaleTargetRefused(t *testing.T) {
	s, _, done := newApprovalTestServer(t)
	defer done()
	connID := firstConnID(t, s)

	// Seed an integration (org admin, direct), then park an expanding update.
	w := httptest.NewRecorder()
	s.handleEnableForgeRepoBots(w, forgeReq(orgAdminCtx(), "POST", "/api/teams/t1/forge/repo-bots", enableBody(connID), "t1"))
	if w.Code != http.StatusOK {
		t.Fatalf("seed enable: code=%d body=%s", w.Code, w.Body.String())
	}
	var res forge.ProvisionResult
	json.Unmarshal(w.Body.Bytes(), &res)
	w = httptest.NewRecorder()
	req := forgeReq(teamAdminCtx(), "PATCH", "/api/teams/t1/forge/repo-bots/"+res.IntegrationID, `{"bot_ids":["review-pr","dep-guard"]}`, "t1")
	req.SetPathValue("integration_id", res.IntegrationID)
	s.handleUpdateForgeRepoBots(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("park update: code=%d body=%s", w.Code, w.Body.String())
	}
	aid := approvalIDFrom(t, w)

	// PARTIAL teardown first: the live bot set diverges from the snapshot
	// taken at park time (here via a direct store mutation, standing in for
	// any change that raced the decision) — approve must refuse rather than
	// resurrect what the team changed.
	ri, err := s.forgeIntegrations.Get(context.Background(), res.IntegrationID)
	if err != nil {
		t.Fatal(err)
	}
	ri.BotIDs = []string{"gate-bot"}
	if err := s.forgeIntegrations.Update(context.Background(), ri); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	req = orgReq(orgAdminCtx(), "POST", "/api/orgs/o1/provision-approvals/"+aid+"/approve", "", "o1")
	req.SetPathValue("approval_id", aid)
	s.handleApproveProvision(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("diverged bot set should 409: code=%d body=%s", w.Code, w.Body.String())
	}

	// FULL teardown: the team deletes the integration before the decision.
	if err := s.forgeIntegrations.Delete(context.Background(), res.IntegrationID); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	req = orgReq(orgAdminCtx(), "POST", "/api/orgs/o1/provision-approvals/"+aid+"/approve", "", "o1")
	req.SetPathValue("approval_id", aid)
	s.handleApproveProvision(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("stale approve should 409: code=%d body=%s", w.Code, w.Body.String())
	}
	// The record survives for an explicit reject.
	if list, _ := s.provisionApprovals.ListByOrg(context.Background(), "o1"); len(list) != 1 {
		t.Fatalf("stale record should stay pending: %d", len(list))
	}
}

// POST /forge/repo-bots on an ALREADY-connected repo is the add-bots
// gesture (Provision merges when Replace is false — it is how the studio's
// BindBotWizard binds one more bot). Parking it as a NEW-repo request made
// approve refuse it forever with "provisioned after the request was
// parked": the repo was already provisioned AT park time. The park must
// snapshot the live integration so approve takes the update branch.
func TestProvisionApproval_AddBotsToConnectedRepoIsApprovable(t *testing.T) {
	s, _, done := newApprovalTestServer(t)
	defer done()
	connID := firstConnID(t, s)

	// Seed: org admin connects the repo directly (ungated by right).
	w := httptest.NewRecorder()
	s.handleEnableForgeRepoBots(w, forgeReq(orgAdminCtx(), "POST", "/api/teams/t1/forge/repo-bots", enableBody(connID), "t1"))
	if w.Code != http.StatusOK {
		t.Fatalf("seed enable: code=%d body=%s", w.Code, w.Body.String())
	}
	var seed forge.ProvisionResult
	json.Unmarshal(w.Body.Bytes(), &seed)

	// The team admin binds one MORE bot to that same repo → parks.
	w = httptest.NewRecorder()
	body := `{"connection_id":"` + connID + `","repo":"group/api","bot_ids":["review-pr","dep-guard"]}`
	s.handleEnableForgeRepoBots(w, forgeReq(teamAdminCtx(), "POST", "/api/teams/t1/forge/repo-bots", body, "t1"))
	if w.Code != http.StatusAccepted {
		t.Fatalf("add-bots enable should park: code=%d body=%s", w.Code, w.Body.String())
	}
	aid := approvalIDFrom(t, w)

	// The record must carry the live integration, not a new-repo blank.
	a, err := s.provisionApprovals.Get(context.Background(), aid)
	if err != nil {
		t.Fatal(err)
	}
	if a.IntegrationID != seed.IntegrationID {
		t.Fatalf("park lost the existing integration: got %q want %q", a.IntegrationID, seed.IntegrationID)
	}
	if !equalStringSets(a.BaseBotIDs, []string{"review-pr"}) {
		t.Fatalf("park lost the base bot set: %v", a.BaseBotIDs)
	}

	// Approve must SUCCEED and merge, not 409.
	w = httptest.NewRecorder()
	req := orgReq(orgAdminCtx(), "POST", "/api/orgs/o1/provision-approvals/"+aid+"/approve", "", "o1")
	req.SetPathValue("approval_id", aid)
	s.handleApproveProvision(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("approving an add-bots request must not 409: code=%d body=%s", w.Code, w.Body.String())
	}
	var res forge.ProvisionResult
	json.Unmarshal(w.Body.Bytes(), &res)
	if !equalStringSets(res.BotIDs, []string{"review-pr", "dep-guard"}) {
		t.Fatalf("bots not merged on approve: %v", res.BotIDs)
	}
	if ints, _ := s.forgeIntegrations.ListByTenant(context.Background(), "t1"); len(ints) != 1 {
		t.Fatalf("add-bots must update the integration, not duplicate it: %d", len(ints))
	}
}

// The org approval gate would be theatre if the settings it arbitrates
// stayed directly PATCHable on the config the approval produced: the
// webhook config is where they are ENFORCED at delivery time. A team admin
// in an approval-required org must not be able to get one modest bot
// approved and then widen the surface through
// PATCH /api/teams/{id}/webhooks/{webhook_id}.
func TestProvisionApproval_WebhookPatchCannotBypassGate(t *testing.T) {
	s, _, done := newApprovalTestServer(t)
	defer done()
	connID := firstConnID(t, s)

	// Seed a PROVISIONED webhook (org admin, direct — ungated by right).
	w := httptest.NewRecorder()
	s.handleEnableForgeRepoBots(w, forgeReq(orgAdminCtx(), "POST", "/api/teams/t1/forge/repo-bots", enableBody(connID), "t1"))
	if w.Code != http.StatusOK {
		t.Fatalf("seed enable: code=%d body=%s", w.Code, w.Body.String())
	}
	var res forge.ProvisionResult
	json.Unmarshal(w.Body.Bytes(), &res)
	if res.WebhookID == "" {
		t.Fatal("seed produced no webhook")
	}

	patch := func(ctx context.Context, id, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := forgeReq(ctx, "PATCH", "/api/teams/t1/webhooks/"+id, body, "t1")
		req.SetPathValue("webhook_id", id)
		s.handleUpdateWebhook(rec, req)
		return rec
	}
	// pre puts the config in the "before" state each case needs, straight
	// through the store so the guard under test is not what sets it up.
	pre := func(t *testing.T, mut func(*webhooks.Config)) {
		t.Helper()
		cfg, err := s.webhookConfigs.Get(context.Background(), res.WebhookID)
		if err != nil {
			t.Fatal(err)
		}
		mut(&cfg)
		if err := s.webhookConfigs.Update(context.Background(), cfg); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		name string
		mut  func(*webhooks.Config)
		body string
		want int
	}{
		// ── EXPANSIONS: refused ────────────────────────────────────────
		{"re-enable a disabled webhook", func(c *webhooks.Config) { c.Enabled = false },
			`{"enabled":true}`, http.StatusConflict},
		{"wildcard bot scope", nil,
			`{"wildcard_bots":true}`, http.StatusConflict},
		{"add a bot", nil,
			`{"bot_ids":["review-pr","dep-guard"]}`, http.StatusConflict},
		{"clear the project allowlist", func(c *webhooks.Config) { c.ProjectAllowlist = []string{"group/api"} },
			`{"project_allowlist":[]}`, http.StatusConflict},
		{"add a project", func(c *webhooks.Config) { c.ProjectAllowlist = []string{"group/api"} },
			`{"project_allowlist":["group/api","group/other"]}`, http.StatusConflict},
		{"star the project allowlist", func(c *webhooks.Config) { c.ProjectAllowlist = []string{"group/api"} },
			`{"project_allowlist":["*"]}`, http.StatusConflict},
		{"clear the author allowlist", func(c *webhooks.Config) { c.AuthorAllowlist = []string{"dependabot[bot]"} },
			`{"author_allowlist":[]}`, http.StatusConflict},
		{"widen the label allowlist", func(c *webhooks.Config) { c.LabelAllowlist = []string{"implement"} },
			`{"label_allowlist":["implement","chore"]}`, http.StatusConflict},
		{"change the event allowlist", func(c *webhooks.Config) { c.EventAllowlist = []string{"merge_request"} },
			`{"event_allowlist":["merge_request","note"]}`, http.StatusConflict},
		{"grant a command replier", func(c *webhooks.Config) { c.AuthorizedRepliers = nil },
			`{"authorized_repliers":["mallory"]}`, http.StatusConflict},
		{"lower the replier role floor", func(c *webhooks.Config) { c.MinReplierRole = "maintainer" },
			`{"min_replier_role":"reporter"}`, http.StatusConflict},
		{"lift a hold label", func(c *webhooks.Config) { c.HoldLabels = []string{"automation-hold"} },
			`{"hold_labels":[]}`, http.StatusConflict},
		{"turn the zero-touch lane on", func(c *webhooks.Config) { c.AutoImplementOnOpen = false },
			`{"auto_implement_on_open":true}`, http.StatusConflict},
		{"turn review-on-sync on", func(c *webhooks.Config) { c.ReviewOnSync = false },
			`{"review_on_sync":true}`, http.StatusConflict},
		{"drop the fork protection", func(c *webhooks.Config) { c.BlockForkPRs = true },
			`{"block_fork_prs":false}`, http.StatusConflict},
		{"unbound the overlap policy", func(c *webhooks.Config) { c.Overlap = "supersede" },
			`{"overlap":"allow"}`, http.StatusConflict},

		// ── TIGHTENINGS AND NO-OPS: the guard must stay narrow ─────────
		{"remove a bot", func(c *webhooks.Config) { c.BotIDs = []string{"review-pr", "dep-guard"} },
			`{"bot_ids":["review-pr"]}`, http.StatusOK},
		{"narrow the project allowlist", func(c *webhooks.Config) {
			c.ProjectAllowlist = []string{"group/api", "group/other"}
		}, `{"project_allowlist":["group/api"]}`, http.StatusOK},
		{"bound an open project allowlist", func(c *webhooks.Config) { c.ProjectAllowlist = nil },
			`{"project_allowlist":["group/api"]}`, http.StatusOK},
		{"narrow a starred allowlist", func(c *webhooks.Config) { c.ProjectAllowlist = []string{"*"} },
			`{"project_allowlist":["group/api"]}`, http.StatusOK},
		{"add a hold label", func(c *webhooks.Config) { c.HoldLabels = nil },
			`{"hold_labels":["automation-hold"]}`, http.StatusOK},
		{"turn the zero-touch lane off", func(c *webhooks.Config) { c.AutoImplementOnOpen = true },
			`{"auto_implement_on_open":false}`, http.StatusOK},
		{"raise the replier role floor", func(c *webhooks.Config) { c.MinReplierRole = "reporter" },
			`{"min_replier_role":"owner"}`, http.StatusOK},
		{"revoke a command replier", func(c *webhooks.Config) { c.AuthorizedRepliers = []string{"mallory"} },
			`{"authorized_repliers":[]}`, http.StatusOK},
		{"rename (no surface change)", nil,
			`{"name":"renamed"}`, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Reset to a known-neutral baseline, then apply the case's own.
			pre(t, func(c *webhooks.Config) {
				c.Enabled, c.WildcardBots = true, false
				c.BotIDs = []string{"review-pr"}
				c.ProjectAllowlist, c.EventAllowlist, c.AuthorAllowlist = nil, nil, nil
				c.LabelAllowlist, c.HoldLabels, c.AuthorizedRepliers = nil, nil, nil
				c.MinReplierRole, c.Overlap = "", ""
				c.AutoImplementOnOpen, c.ReviewOnSync, c.BlockForkPRs = false, false, false
			})
			if tc.mut != nil {
				pre(t, tc.mut)
			}
			if got := patch(teamAdminCtx(), res.WebhookID, tc.body).Code; got != tc.want {
				t.Fatalf("team admin PATCH %s: code=%d want=%d", tc.body, got, tc.want)
			}
		})
	}

	// ── The guard must not fire outside its three preconditions ────────
	t.Run("org admin is ungated", func(t *testing.T) {
		pre(t, func(c *webhooks.Config) { c.WildcardBots = false; c.BotIDs = []string{"review-pr"} })
		if got := patch(orgAdminCtx(), res.WebhookID, `{"wildcard_bots":true}`).Code; got != http.StatusOK {
			t.Fatalf("org admin must be able to widen directly: code=%d", got)
		}
	})
	t.Run("flag off is ungated", func(t *testing.T) {
		pre(t, func(c *webhooks.Config) { c.WildcardBots = false; c.BotIDs = []string{"review-pr"} })
		o, err := s.authStore().GetOrg(context.Background(), "o1")
		if err != nil {
			t.Fatal(err)
		}
		o.RequireProvisionApproval = false
		if err := s.authStore().UpdateOrg(context.Background(), o); err != nil {
			t.Fatal(err)
		}
		defer func() {
			o.RequireProvisionApproval = true
			if err := s.authStore().UpdateOrg(context.Background(), o); err != nil {
				t.Fatal(err)
			}
		}()
		if got := patch(teamAdminCtx(), res.WebhookID, `{"wildcard_bots":true}`).Code; got != http.StatusOK {
			t.Fatalf("gate must not fire with the org flag off: code=%d", got)
		}
	})
	t.Run("a hand-made webhook is ungated", func(t *testing.T) {
		cfg := webhooks.Config{
			ID: "wh-manual", TenantID: "t1", Name: "manual", Provider: "gitlab",
			Enabled: true, BotIDs: []string{"review-pr"}, CreatedAt: time.Now().UTC(),
		}
		if err := s.webhookConfigs.Create(context.Background(), cfg); err != nil {
			t.Fatal(err)
		}
		if got := patch(teamAdminCtx(), "wh-manual", `{"wildcard_bots":true}`).Code; got != http.StatusOK {
			t.Fatalf("gate must only cover PROVISIONED webhooks: code=%d", got)
		}
	})
}

// A decision must be claimed BEFORE any forge side effect. Reading the
// record, provisioning, and deleting it afterwards left a window in which
// a reject returned 204 — and wrote a "rejected" audit row — for a repo an
// approve was already creating on the forge. The mock pins the approve
// inside the hook CREATE so the window is real and the test deterministic.
func TestProvisionApproval_DecisionIsClaimedBeforeSideEffects(t *testing.T) {
	s, gl, done := newApprovalTestServer(t)
	defer done()
	connID := firstConnID(t, s)

	w := httptest.NewRecorder()
	s.handleEnableForgeRepoBots(w, forgeReq(teamAdminCtx(), "POST", "/api/teams/t1/forge/repo-bots", enableBody(connID), "t1"))
	if w.Code != http.StatusAccepted {
		t.Fatalf("enable should park: code=%d body=%s", w.Code, w.Body.String())
	}
	aid := approvalIDFrom(t, w)

	gl.enterHook, gl.releaseHook = make(chan struct{}), make(chan struct{})
	// Always unblock the pinned request, including on t.Fatalf: this defer
	// is registered after the suite's srv.Close, so it runs BEFORE it.
	// Without it a failing assertion leaves the mock handler parked and the
	// test hangs to its 10-minute timeout instead of reporting.
	var release sync.Once
	releaseHook := func() { release.Do(func() { close(gl.releaseHook) }) }
	defer releaseHook()

	approved := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		req := orgReq(orgAdminCtx(), "POST", "/api/orgs/o1/provision-approvals/"+aid+"/approve", "", "o1")
		req.SetPathValue("approval_id", aid)
		s.handleApproveProvision(rec, req)
		approved <- rec.Code
	}()

	// The approve is now suspended mid-provision, past the point of no
	// return: the hook is about to be created on the forge.
	select {
	case <-gl.enterHook:
	case <-time.After(5 * time.Second):
		t.Fatal("approve never reached the forge hook create")
	}

	// A second admin rejects in that window. It must NOT succeed — the
	// decision is already claimed.
	rec := httptest.NewRecorder()
	req := orgReq(orgAdminCtx(), "POST", "/api/orgs/o1/provision-approvals/"+aid+"/reject", `{"reason":"race"}`, "o1")
	req.SetPathValue("approval_id", aid)
	s.handleRejectProvision(rec, req)
	if rec.Code == http.StatusNoContent {
		t.Fatalf("reject succeeded while an approve was provisioning the repo — the decision invariant is broken")
	}

	// A second APPROVE in the same window must not duplicate the forge work.
	rec2 := httptest.NewRecorder()
	req2 := orgReq(orgAdminCtx(), "POST", "/api/orgs/o1/provision-approvals/"+aid+"/approve", "", "o1")
	req2.SetPathValue("approval_id", aid)
	s.handleApproveProvision(rec2, req2)
	if rec2.Code == http.StatusOK {
		t.Fatalf("a second approve executed the same request twice: code=%d", rec2.Code)
	}

	releaseHook()
	if code := <-approved; code != http.StatusOK {
		t.Fatalf("the winning approve should succeed: code=%d", code)
	}

	// Exactly one integration, and exactly one hook created on the forge.
	if ints, _ := s.forgeIntegrations.ListByTenant(context.Background(), "t1"); len(ints) != 1 {
		t.Fatalf("want exactly 1 integration, got %d", len(ints))
	}
	gl.mu.Lock()
	hooks := len(gl.hooks)
	gl.mu.Unlock()
	if hooks != 1 {
		t.Fatalf("want exactly 1 forge hook, got %d", hooks)
	}
}

// A provision that FAILS must return the request to the queue: claiming
// the decision up front must not silently consume it on a transient forge
// error, or the admin has nothing left to retry or reject.
func TestProvisionApproval_FailedProvisionStaysPending(t *testing.T) {
	s, _, done := newApprovalTestServer(t)
	defer done()
	connID := firstConnID(t, s)

	w := httptest.NewRecorder()
	s.handleEnableForgeRepoBots(w, forgeReq(teamAdminCtx(), "POST", "/api/teams/t1/forge/repo-bots", enableBody(connID), "t1"))
	if w.Code != http.StatusAccepted {
		t.Fatalf("enable should park: code=%d body=%s", w.Code, w.Body.String())
	}
	aid := approvalIDFrom(t, w)

	// Break the provision: an unresolvable bot fails inside the orchestrator,
	// after the claim.
	a, err := s.provisionApprovals.Get(context.Background(), aid)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.provisionApprovals.Delete(context.Background(), aid); err != nil {
		t.Fatal(err)
	}
	a.BotIDs = []string{"no-such-bot"}
	if err := s.provisionApprovals.Create(context.Background(), a); err != nil {
		t.Fatal(err)
	}

	w = httptest.NewRecorder()
	req := orgReq(orgAdminCtx(), "POST", "/api/orgs/o1/provision-approvals/"+aid+"/approve", "", "o1")
	req.SetPathValue("approval_id", aid)
	s.handleApproveProvision(w, req)
	if w.Code == http.StatusOK {
		t.Fatalf("provision of an unresolvable bot should fail: code=%d body=%s", w.Code, w.Body.String())
	}
	got, err := s.provisionApprovals.Get(context.Background(), aid)
	if err != nil {
		t.Fatalf("a failed approve must leave the request in the queue: %v", err)
	}
	if got.ID != aid || got.RequestedBy != "teamadmin" {
		t.Fatalf("the restored record must be the original: %+v", got)
	}
}

// The approval replay REPLACES every setting the request carries — it does
// not merge. So a tightening the team applies while a request waits in the
// queue would be silently undone on approval. The sharpest case: park a
// "lift the hold labels" request, then add a NEW hold label, then approve —
// the brake must survive.
func TestProvisionApproval_StaleOperatorFieldRefused(t *testing.T) {
	s, _, done := newApprovalTestServer(t)
	defer done()
	connID := firstConnID(t, s)

	// Seed an integration carrying a hold label (org admin, direct).
	w := httptest.NewRecorder()
	body := `{"connection_id":"` + connID + `","repo":"group/api","bot_ids":["review-pr"],"hold_labels":["automation-hold"]}`
	s.handleEnableForgeRepoBots(w, forgeReq(orgAdminCtx(), "POST", "/api/teams/t1/forge/repo-bots", body, "t1"))
	if w.Code != http.StatusOK {
		t.Fatalf("seed enable: code=%d body=%s", w.Code, w.Body.String())
	}
	var res forge.ProvisionResult
	json.Unmarshal(w.Body.Bytes(), &res)

	// Team admin asks to LIFT the hold (an expansion) → parked.
	w = httptest.NewRecorder()
	req := forgeReq(teamAdminCtx(), "PATCH", "/api/teams/t1/forge/repo-bots/"+res.IntegrationID,
		`{"bot_ids":["review-pr"],"hold_labels":[]}`, "t1")
	req.SetPathValue("integration_id", res.IntegrationID)
	s.handleUpdateForgeRepoBots(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("lifting a hold label should park: code=%d body=%s", w.Code, w.Body.String())
	}
	aid := approvalIDFrom(t, w)

	// Meanwhile the team adds a SECOND hold label — a tightening, so it goes
	// through directly and never sees the queue.
	w = httptest.NewRecorder()
	req = forgeReq(teamAdminCtx(), "PATCH", "/api/teams/t1/forge/repo-bots/"+res.IntegrationID,
		`{"bot_ids":["review-pr"],"hold_labels":["automation-hold","incident"]}`, "t1")
	req.SetPathValue("integration_id", res.IntegrationID)
	s.handleUpdateForgeRepoBots(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("adding a hold label should be direct: code=%d body=%s", w.Code, w.Body.String())
	}

	// Approving the stale request must refuse, naming the diverged field.
	w = httptest.NewRecorder()
	areq := orgReq(orgAdminCtx(), "POST", "/api/orgs/o1/provision-approvals/"+aid+"/approve", "", "o1")
	areq.SetPathValue("approval_id", aid)
	s.handleApproveProvision(w, areq)
	if w.Code != http.StatusConflict {
		t.Fatalf("approving over a newer tightening should 409: code=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "hold labels") {
		t.Fatalf("the 409 should name the diverged field: %s", w.Body.String())
	}

	// The brake is intact and the record stays pending for an explicit reject.
	ri, err := s.forgeIntegrations.Get(context.Background(), res.IntegrationID)
	if err != nil {
		t.Fatal(err)
	}
	if !equalStringSets(ri.HoldLabels, []string{"automation-hold", "incident"}) {
		t.Fatalf("the newer hold labels were overwritten: %v", ri.HoldLabels)
	}
	if _, err := s.provisionApprovals.Get(context.Background(), aid); err != nil {
		t.Fatalf("stale record should stay pending: %v", err)
	}
}

// A field the request never mentioned adopts the live value at replay time,
// so an unrelated change must NOT block the approval — the staleness guard
// has to stay narrow or every queued request rots on the first edit.
func TestProvisionApproval_UnrelatedChangeDoesNotBlockApproval(t *testing.T) {
	s, _, done := newApprovalTestServer(t)
	defer done()
	connID := firstConnID(t, s)

	w := httptest.NewRecorder()
	s.handleEnableForgeRepoBots(w, forgeReq(orgAdminCtx(), "POST", "/api/teams/t1/forge/repo-bots", enableBody(connID), "t1"))
	if w.Code != http.StatusOK {
		t.Fatalf("seed enable: code=%d body=%s", w.Code, w.Body.String())
	}
	var res forge.ProvisionResult
	json.Unmarshal(w.Body.Bytes(), &res)

	// Park a request that mentions ONLY the bot set.
	w = httptest.NewRecorder()
	req := forgeReq(teamAdminCtx(), "PATCH", "/api/teams/t1/forge/repo-bots/"+res.IntegrationID,
		`{"bot_ids":["review-pr","dep-guard"]}`, "t1")
	req.SetPathValue("integration_id", res.IntegrationID)
	s.handleUpdateForgeRepoBots(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("adding a bot should park: code=%d body=%s", w.Code, w.Body.String())
	}
	aid := approvalIDFrom(t, w)

	// The team tightens a DIFFERENT field meanwhile.
	w = httptest.NewRecorder()
	req = forgeReq(teamAdminCtx(), "PATCH", "/api/teams/t1/forge/repo-bots/"+res.IntegrationID,
		`{"bot_ids":["review-pr"],"hold_labels":["incident"]}`, "t1")
	req.SetPathValue("integration_id", res.IntegrationID)
	s.handleUpdateForgeRepoBots(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("adding a hold label should be direct: code=%d body=%s", w.Code, w.Body.String())
	}

	// The bot-set snapshot still matches (the tightening kept review-pr), and
	// hold labels were never part of the request → approval proceeds, and the
	// newer hold label survives because the replay leaves it alone.
	w = httptest.NewRecorder()
	areq := orgReq(orgAdminCtx(), "POST", "/api/orgs/o1/provision-approvals/"+aid+"/approve", "", "o1")
	areq.SetPathValue("approval_id", aid)
	s.handleApproveProvision(w, areq)
	if w.Code != http.StatusOK {
		t.Fatalf("an unrelated tightening must not block approval: code=%d body=%s", w.Code, w.Body.String())
	}
	ri, err := s.forgeIntegrations.Get(context.Background(), res.IntegrationID)
	if err != nil {
		t.Fatal(err)
	}
	if !equalStringSets(ri.HoldLabels, []string{"incident"}) {
		t.Fatalf("the unmentioned hold label should survive the replay: %v", ri.HoldLabels)
	}
}

// The approver reads /api/orgs/{id}/audit, which lists by ORG id. Approve
// and reject write org-keyed rows, but the REQUEST was recorded only
// team-side — so the org admin saw decisions with no trace of what had
// been asked, and the ProvisionApproval row is deleted on decision, so the
// queue no longer held it either.
func TestProvisionApproval_RequestReachesTheOrgAuditLog(t *testing.T) {
	s, _, done := newApprovalTestServer(t)
	defer done()
	connID := firstConnID(t, s)

	w := httptest.NewRecorder()
	s.handleEnableForgeRepoBots(w, forgeReq(teamAdminCtx(), "POST", "/api/teams/t1/forge/repo-bots", enableBody(connID), "t1"))
	if w.Code != http.StatusAccepted {
		t.Fatalf("enable should park: code=%d body=%s", w.Code, w.Body.String())
	}

	// Audit writes are detached (goSafe), so poll rather than assume.
	find := func(tenant string) *audit.Event {
		for i := 0; i < 100; i++ {
			evs, err := s.auditStore.ListByTenant(context.Background(), tenant, audit.Page{Limit: 50})
			if err == nil {
				for j := range evs {
					if evs[j].Action == "forge.provision.approval_requested" {
						return &evs[j]
					}
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
		return nil
	}
	if find("t1") == nil {
		t.Fatal("the team's own audit log lost the request")
	}
	ev := find("o1")
	if ev == nil {
		t.Fatal("the request never reached the ORG audit log the approver reads")
	}
	if ev.Meta["team_id"] != "t1" || ev.Meta["requested_by"] != "teamadmin" {
		t.Fatalf("the org-side row must name the requesting team and user: %+v", ev.Meta)
	}
}

func approvalIDFrom(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var parked struct {
		ApprovalID string `json:"approval_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &parked); err != nil || parked.ApprovalID == "" {
		t.Fatalf("no approval id in %s (%v)", w.Body.String(), err)
	}
	return parked.ApprovalID
}

// The platform default is a ceiling for the DELEGATED caps route: an org
// admin may tighten below it, never raise above it.
func TestOrgTeamCaps_PlatformCeiling(t *testing.T) {
	s, _, done := newApprovalTestServer(t)
	defer done()
	s.orgDefaults.MaxConcurrentRuns = 5
	s.orgDefaults.LaunchRatePerMin = 20

	req := orgReq(orgAdminCtx(), "PATCH", "/api/orgs/o1/teams/t1/caps", `{"max_concurrent_runs":6}`, "o1")
	req.SetPathValue("team_id", "t1")
	w := httptest.NewRecorder()
	s.handleUpdateOrgTeamCaps(w, req)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("above-ceiling should 422: code=%d body=%s", w.Code, w.Body.String())
	}
	// At or below the ceiling passes; 0 = inherit is always allowed.
	req = orgReq(orgAdminCtx(), "PATCH", "/api/orgs/o1/teams/t1/caps", `{"max_concurrent_runs":5,"launch_rate_per_min":0}`, "o1")
	req.SetPathValue("team_id", "t1")
	w = httptest.NewRecorder()
	s.handleUpdateOrgTeamCaps(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("at-ceiling should pass: code=%d body=%s", w.Code, w.Body.String())
	}

	// A STORED above-ceiling value (super-admin set) must not lock the row:
	// patching only the other field validates only the submitted field.
	tm, err := s.authStore().GetTeam(context.Background(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	tm.MaxConcurrentRuns = 50 // above the ceiling of 5, super-admin territory
	if err := s.authStore().UpdateTeam(context.Background(), tm); err != nil {
		t.Fatal(err)
	}
	req = orgReq(orgAdminCtx(), "PATCH", "/api/orgs/o1/teams/t1/caps", `{"launch_rate_per_min":10}`, "o1")
	req.SetPathValue("team_id", "t1")
	w = httptest.NewRecorder()
	s.handleUpdateOrgTeamCaps(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("patching the other field should pass despite a stored above-ceiling cap: code=%d body=%s", w.Code, w.Body.String())
	}
	tm, _ = s.authStore().GetTeam(context.Background(), "t1")
	if tm.MaxConcurrentRuns != 50 || tm.LaunchRatePerMin != 10 {
		t.Fatalf("stored cap clobbered or rate not applied: %+v", tm)
	}
}

func TestOrgTeamCaps_Update(t *testing.T) {
	s, _, done := newApprovalTestServer(t)
	defer done()

	req := orgReq(orgAdminCtx(), "PATCH", "/api/orgs/o1/teams/t1/caps", `{"max_concurrent_runs":4,"launch_rate_per_min":10}`, "o1")
	req.SetPathValue("team_id", "t1")
	w := httptest.NewRecorder()
	s.handleUpdateOrgTeamCaps(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("caps patch: code=%d body=%s", w.Code, w.Body.String())
	}
	tm, err := s.authStore().GetTeam(context.Background(), "t1")
	if err != nil || tm.MaxConcurrentRuns != 4 || tm.LaunchRatePerMin != 10 {
		t.Fatalf("caps not persisted: %+v (%v)", tm, err)
	}

	// A team outside the org → 404 (no cross-org reach).
	if _, err := s.authStore().CreateTeam(context.Background(), identity.Team{ID: "t2", Name: "t2", Slug: "t2", OrgID: "other", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	req = orgReq(orgAdminCtx(), "PATCH", "/api/orgs/o1/teams/t2/caps", `{"max_concurrent_runs":1}`, "o1")
	req.SetPathValue("team_id", "t2")
	w = httptest.NewRecorder()
	s.handleUpdateOrgTeamCaps(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("cross-org caps should 404: code=%d", w.Code)
	}

	// Negative value → 400.
	req = orgReq(orgAdminCtx(), "PATCH", "/api/orgs/o1/teams/t1/caps", `{"max_concurrent_runs":-1}`, "o1")
	req.SetPathValue("team_id", "t1")
	w = httptest.NewRecorder()
	s.handleUpdateOrgTeamCaps(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("negative caps should 400: code=%d", w.Code)
	}
}

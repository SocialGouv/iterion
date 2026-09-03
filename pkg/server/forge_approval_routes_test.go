package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/identity"
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

	// The team deletes the integration before the org admin decides.
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

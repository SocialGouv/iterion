package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
)

// Round-6 verifier witnesses. The findings:
//
//   - A park accepts a launch-vars write the direct path refuses; the
//     parked record replays into a deterministic refusal on EVERY approve
//     and (pre-fix) stays pending — the queue holds an unapprovable
//     request and the requester only sees "awaiting approval".
//   - A PARTIAL hold-label lift (or allowlist widening) parks precisely
//     because it re-arms automation, and the org wire carried only the
//     destination set — the widening the org is deciding on was invisible
//     (the base_bot_ids precedent had no twin for the two sets).
//   - The three Set flags rode bson tags with no codec-level pin.

func r6OrgList(t *testing.T, s *Server) []map[string]any {
	t.Helper()
	w := httptest.NewRecorder()
	s.handleListOrgProvisionApprovals(w, orgReq(orgAdminCtx(), "GET", "/api/orgs/o1/provision-approvals", "", "o1"))
	if w.Code != http.StatusOK {
		t.Fatalf("org list: code=%d body=%s", w.Code, w.Body.String())
	}
	var lr struct {
		Approvals []map[string]any `json:"approvals"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &lr); err != nil {
		t.Fatalf("org list decode: %v", err)
	}
	return lr.Approvals
}

func r6Approve(t *testing.T, s *Server, id string) *httptest.ResponseRecorder {
	t.Helper()
	req := orgReq(orgAdminCtx(), "POST", "/api/orgs/o1/provision-approvals/"+id+"/approve", "", "o1")
	req.SetPathValue("approval_id", id)
	w := httptest.NewRecorder()
	s.handleApproveProvision(w, req)
	return w
}

// F1: a parked request whose launch-vars map would drop a stored pin is a
// DETERMINISTIC dead end — the orchestrator refuses it against the same
// stores on every retry, and the refusal's remedy (echo the pins, or the
// replace gesture) is advice only the REQUESTER can apply. Approve must
// consume the record with its reason (409), not answer 400 and leave it
// squatting in the queue. The direct path's refusal of the same body is
// the proof the park accepted what it should never have parked.
func TestRV_R6VF_DeadEndApproval_ConsumedWithItsReason(t *testing.T) {
	s, _, done := newApprovalTestServer(t)
	defer done()
	connID := firstConnID(t, s)

	res := seedIntegration(t, s, connID, "group/api", `,"launch_vars":{"gate_context":"api"}`)

	// schedule_crons is what parks (expandsProvisionSurface); the dropping
	// launch_vars map rides beside it, unvalidated on the park path.
	body := `{"schedule_crons":{"review-pr":"0 3 * * *"},"launch_vars":{"other":"v"}}`
	w := httptest.NewRecorder()
	req := forgeReq(teamAdminCtx(), "PATCH", "/api/teams/t1/forge/repo-bots/"+res.IntegrationID, body, "t1")
	req.SetPathValue("integration_id", res.IntegrationID)
	s.handleUpdateForgeRepoBots(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("the dropping map should park (precondition): code=%d body=%s", w.Code, w.Body.String())
	}
	rec, _ := orgApprovalWire(t, s)
	id := rec["id"].(string)

	// The SAME body on the direct path (org admin — the gate skips org
	// admins) is refused: the park accepted what the direct path refuses.
	dw := httptest.NewRecorder()
	dreq := forgeReq(orgAdminCtx(), "PATCH", "/api/teams/t1/forge/repo-bots/"+res.IntegrationID, body, "t1")
	dreq.SetPathValue("integration_id", res.IntegrationID)
	s.handleUpdateForgeRepoBots(dw, dreq)
	if dw.Code != http.StatusBadRequest || !strings.Contains(dw.Body.String(), "would drop pinned key(s)") {
		t.Fatalf("direct path must refuse the dropping map with the drop reason: code=%d body=%s", dw.Code, dw.Body.String())
	}

	// Approve replays the recorded request: a refusal that can never land
	// consumes the record instead of parking it forever.
	aw := r6Approve(t, s, id)
	if aw.Code != http.StatusConflict || !strings.Contains(aw.Body.String(), "would drop pinned key(s)") {
		t.Fatalf("approve must answer 409 naming the deterministic refusal: code=%d body=%s", aw.Code, aw.Body.String())
	}
	// The queue holds no unapprovable request: the record is gone from the
	// org list and the team list.
	if got := r6OrgList(t, s); len(got) != 0 {
		t.Fatalf("the unapprovable record stayed pending after approve: %v", got)
	}
	tw := httptest.NewRecorder()
	s.handleListTeamProvisionApprovals(tw, forgeReq(teamAdminCtx(), "GET", "/api/teams/t1/provision-approvals", "", "t1"))
	var tl struct {
		Approvals []map[string]any `json:"approvals"`
	}
	json.Unmarshal(tw.Body.Bytes(), &tl)
	if len(tl.Approvals) != 0 {
		t.Fatalf("the requester's list still holds the consumed record: %s", tw.Body.String())
	}
	// Nothing landed forge-side or store-side: a consumed approval is not
	// a provisioned one.
	if ints, _ := s.forgeIntegrations.ListByTenant(context.Background(), "t1"); len(ints) != 1 {
		t.Fatalf("approve must not provision a refused request: %d integrations", len(ints))
	}
}

// F1, the guard against an over-wide predicate: a TRANSIENT Provision
// failure (the forge fell over mid-hook-write) must keep the record
// pending exactly as before — a retry can land it, so approving must not
// consume it. BOTH bots resolve as provisionable (an unprovisionable bot is
// the deterministic dead end the arm consumes) and a bot-set change is what
// reaches the forge-side hook write — a schedule-only re-provision
// short-circuits before any forge call.
func TestRV_R6VF_TransientProvisionFailure_KeepsTheRecordPending(t *testing.T) {
	s, gl, done := newApprovalTestServer(t)
	defer done()
	connID := firstConnID(t, s)

	res := seedIntegration(t, s, connID, "group/api", ``)
	// Give docs-refresh a forge block for THIS test only: the shared lookup
	// refuses it, and the point here is the forge call, not the refusal.
	orig := s.forgeOrchestrator.Bots
	s.forgeOrchestrator.Bots = func(_ context.Context, _, botID string) (*bundle.ForgeRequirements, error) {
		if botID == "docs-refresh" {
			return &bundle.ForgeRequirements{
				Events:      []string{bundle.ForgeEventPullRequest},
				TokenScopes: map[string]string{"pull_requests": "write"},
				Secret:      "forge_token",
			}, nil
		}
		return orig(context.Background(), "t1", botID)
	}
	id := parkUpdate(t, s, res.IntegrationID, `{"bot_ids":["review-pr","docs-refresh"]}`)

	gl.mu.Lock()
	gl.hookWriteStatus = http.StatusInternalServerError
	gl.mu.Unlock()

	aw := r6Approve(t, s, id)
	if aw.Code == http.StatusConflict {
		t.Fatalf("a transient forge failure must not consume the approval: %s", aw.Body.String())
	}
	if aw.Code != http.StatusBadGateway {
		t.Fatalf("approve of a forge-down record answers the forge's story, got code=%d body=%s", aw.Code, aw.Body.String())
	}
	// The 502 is the HOOK WRITE's story — the scenario is self-identifying:
	// the refusal the arm consumes would name the bot instead.
	if strings.Contains(aw.Body.String(), "cannot auto-provision") {
		t.Fatalf("the scenario must reach the forge hook write, not the bot refusal: %s", aw.Body.String())
	}
	if !strings.Contains(aw.Body.String(), "hook") || !strings.Contains(aw.Body.String(), "HTTP 500") {
		t.Fatalf("the 502 must name the forge call that failed: %s", aw.Body.String())
	}
	if got := r6OrgList(t, s); len(got) != 1 {
		t.Fatalf("the transiently-failed record must stay pending for a retry: %v", got)
	}
}

// F1, the divergence arm: a record SILENT about launch vars replays into
// ErrProvisionDiverged when the two stores disagree — a state only the
// explicit gesture of a re-submission resolves, never a retry of the same
// record. Approve consumes it with the divergence named, like the drop
// refusal above. The diverged state is modeled at the stores: it is the
// crashed config-first/integration-second write the refusal exists for,
// and no shipped HTTP surface writes OperatorLaunchVars out-of-band.
func TestRV_R6VF_DivergedStores_ApproveConsumesTheRecord(t *testing.T) {
	s, _, done := newApprovalTestServer(t)
	defer done()
	connID := firstConnID(t, s)

	res := seedIntegration(t, s, connID, "group/api", `,"launch_vars":{"gate_context":"api"}`)
	cfg, err := s.webhookConfigs.Get(context.Background(), res.WebhookID)
	if err != nil {
		t.Fatal(err)
	}
	cfg.OperatorLaunchVars = map[string]string{"gate_context": "api", "extra": "1"}
	if err := s.webhookConfigs.Update(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}

	id := parkUpdate(t, s, res.IntegrationID, `{"schedule_crons":{"review-pr":"0 3 * * *"}}`)
	aw := r6Approve(t, s, id)
	if aw.Code != http.StatusConflict || !strings.Contains(aw.Body.String(), "diverge") {
		t.Fatalf("approve must consume a deterministically-diverged record with its reason: code=%d body=%s", aw.Code, aw.Body.String())
	}
	if got := r6OrgList(t, s); len(got) != 0 {
		t.Fatalf("the diverged record stayed pending after approve: %v", got)
	}
}

// F2: a PARTIAL hold-label lift parks precisely because it lifts a pause,
// and the org wire carried only the destination set — the widening the org
// is deciding on was invisible. The record now snapshots the integration's
// sets at park time (the base_bot_ids precedent) so the card can name what
// the change lifts or widens. One repo throughout: the mock serves a
// single repo path.
func TestRV_R6VF_PartialLift_WireCarriesTheBase(t *testing.T) {
	s, _, done := newApprovalTestServer(t)
	defer done()
	connID := firstConnID(t, s)

	// Stored [pause-a pause-b] / [implement]: both deltas available.
	res := seedIntegration(t, s, connID, "group/api",
		`,"hold_labels":["pause-a","pause-b"],"label_allowlist":["implement"]`)

	// Phase 1 — the partial lift: spoken [pause-a] over the stored pair.
	id := parkUpdate(t, s, res.IntegrationID, `{"hold_labels":["pause-a"]}`)
	rec, raw := orgApprovalWire(t, s)
	if rec["hold_labels_set"] != true {
		t.Fatalf("precondition drifted (hold_labels_set): %s", raw)
	}
	base, _ := rec["base_hold_labels"].([]any)
	if len(base) != 2 || base[0] != "pause-a" || base[1] != "pause-b" {
		t.Fatalf("the org wire must carry the base set the lift acts on (base_bot_ids precedent): %s", raw)
	}
	approveRecord(t, s, id)
	ri, err := s.forgeIntegrations.Get(context.Background(), res.IntegrationID)
	if err != nil || len(ri.HoldLabels) != 1 {
		t.Fatalf("approve still executes the lift: %+v (%v)", ri.HoldLabels, err)
	}

	// Phase 2 — the widening: spoken [implement review] over the stored one.
	id2 := parkUpdate(t, s, res.IntegrationID, `{"label_allowlist":["implement","review"]}`)
	rec2, raw2 := orgApprovalWire(t, s)
	base2, _ := rec2["base_label_allowlist"].([]any)
	if len(base2) != 1 || base2[0] != "implement" {
		t.Fatalf("the org wire must carry the base allowlist the widen acts on: %s", raw2)
	}
	approveRecord(t, s, id2)
	ri2, err := s.forgeIntegrations.Get(context.Background(), res.IntegrationID)
	if err != nil || len(ri2.LabelAllowlist) != 2 {
		t.Fatalf("approve still executes the widen: %+v (%v)", ri2.LabelAllowlist, err)
	}

	// Phase 3 — a NEW-repo request has no base: the keys stay absent, not
	// empty. The park touches no forge surface (nothing is provisioned
	// while pending), so the single-repo mock serves it.
	w := httptest.NewRecorder()
	body := `{"connection_id":"` + connID + `","repo":"group/new","bot_ids":["review-pr"],"hold_labels":["x"]}`
	s.handleEnableForgeRepoBots(w, forgeReq(teamAdminCtx(), "POST", "/api/teams/t1/forge/repo-bots", body, "t1"))
	if w.Code != http.StatusAccepted {
		t.Fatalf("new-repo enable should park: code=%d body=%s", w.Code, w.Body.String())
	}
	rec3, raw3 := orgApprovalWire(t, s)
	if rec3["base_hold_labels"] != nil || rec3["base_label_allowlist"] != nil {
		t.Fatalf("a new-repo record must carry no base sets: %s", raw3)
	}
	if _, ok := rec3["hold_labels"]; !ok {
		t.Fatalf("precondition drifted (hold_labels on the wire): %s", raw3)
	}
}

// The second park caller: an enable on an ALREADY-connected repo (the
// add-bots path) snapshots the same bases — it replayed the same switches
// and parks under the same predicate.
func TestRV_R6VF_AddBotsEnable_CarriesTheBaseSets(t *testing.T) {
	s, _, done := newApprovalTestServer(t)
	defer done()
	connID := firstConnID(t, s)

	seedIntegration(t, s, connID, "group/api", `,"hold_labels":["pause-a"]`)
	w := httptest.NewRecorder()
	body := `{"connection_id":"` + connID + `","repo":"group/api","bot_ids":["review-pr","docs-refresh"]}`
	s.handleEnableForgeRepoBots(w, forgeReq(teamAdminCtx(), "POST", "/api/teams/t1/forge/repo-bots", body, "t1"))
	if w.Code != http.StatusAccepted {
		t.Fatalf("add-bots enable should park: code=%d body=%s", w.Code, w.Body.String())
	}
	rec, raw := orgApprovalWire(t, s)
	base, _ := rec["base_hold_labels"].([]any)
	if len(base) != 1 || base[0] != "pause-a" {
		t.Fatalf("the add-bots park must snapshot the base hold labels: %s", raw)
	}
}

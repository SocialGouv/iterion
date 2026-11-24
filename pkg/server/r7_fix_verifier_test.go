package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/forge"
)

// Round-7 fix witnesses. The findings they pin:
//
//   - The approval arm consumed records whose ErrProvisionDiverged carries a
//     retry-convergent remedy (a mid-write two-store failure, a read-back
//     blip): approve deleted a record a same-request re-run lands, and the
//     remedy it named in its own 409 was the one it had destroyed. The
//     retry-convergent sites now carry forge.ErrProvisionDivergedRetry and
//     the arm keeps those records pending.
//   - The auto-provision refusal (a bot that declares neither a forge: block
//     nor a forge/command invocation) is a static property of the recorded
//     bot set — no retry changes it — yet it stayed pending forever. It now
//     rides forge.ErrBotNotProvisionable and the arm consumes the record.
//   - A legacy (pre-Set-flags) record that spoke launch_vars {} beside
//     launch_vars_replace replays into a whole-map clear while its json
//     omitempty collapsed the empty map to an absent key: the org wire was
//     byte-identical to a silent record and the card denied the clear the
//     approval executes. The map now rides the wire (null = silent, {} =
//     spoken clear) and the card reads presence.

// r7FlakyIntegrations injects one-shot store failures at the exact sites the
// retry-convergent ErrProvisionDiverged flavors live: an Update that fails
// mid-two-store-write, and a Get that fails in the post-write read-back.
type r7FlakyIntegrations struct {
	forge.RepoIntegrationStore
	mu              sync.Mutex
	failUpdates     int  // the next N Update calls fail
	armGetAfterUpd  bool // arm one Get failure after the next successful Update
	armedGetPending bool
}

func (f *r7FlakyIntegrations) Update(ctx context.Context, ri forge.RepoIntegration) error {
	f.mu.Lock()
	if f.failUpdates > 0 {
		f.failUpdates--
		f.mu.Unlock()
		return context.DeadlineExceeded // a transient store failure, injected
	}
	arm := f.armGetAfterUpd
	f.armGetAfterUpd = false
	f.mu.Unlock()
	if err := f.RepoIntegrationStore.Update(ctx, ri); err != nil {
		return err
	}
	if arm {
		f.mu.Lock()
		f.armedGetPending = true
		f.mu.Unlock()
	}
	return nil
}

func (f *r7FlakyIntegrations) Get(ctx context.Context, id string) (forge.RepoIntegration, error) {
	f.mu.Lock()
	armed := f.armedGetPending
	f.armedGetPending = false
	f.mu.Unlock()
	if armed {
		return forge.RepoIntegration{}, context.DeadlineExceeded // a transient store read failure, injected
	}
	return f.RepoIntegrationStore.Get(ctx, id)
}

// r7WrapIntegrations routes the server AND the orchestrator through the
// flaky store (the orchestrator holds the instance it was built with).
func r7WrapIntegrations(s *Server, flaky *r7FlakyIntegrations) {
	s.forgeIntegrations = flaky
	s.forgeOrchestrator.Integrations = flaky
}

// F1, mid-write flavor: a transient failure on the integration store's
// Update leaves the two stores diverged with the enforcement half written —
// the exact state whose remedy is re-running the SAME request. Approve must
// keep the record pending (the retry lands), not consume it and destroy the
// remedy its own 409 names.
func TestR7_MidWriteTransient_ApproveKeepsARecordARerunLands(t *testing.T) {
	s, _, done := newApprovalTestServer(t)
	defer done()
	connID := firstConnID(t, s)

	res := seedIntegration(t, s, connID, "group/api", `,"launch_vars":{"gate_context":"api"}`)
	flaky := &r7FlakyIntegrations{RepoIntegrationStore: forge.NewMemoryRepoIntegrationStore()}
	// Re-seed the wrapper from the live store so the seeded row is visible.
	if err := flaky.Create(context.Background(), mustInteg(t, s, res.IntegrationID)); err != nil {
		t.Fatal(err)
	}
	r7WrapIntegrations(s, flaky)
	flaky.mu.Lock()
	flaky.failUpdates = 1 // the first Integrations.Update of the approve flow
	flaky.mu.Unlock()

	body := `{"schedule_crons":{"review-pr":"0 3 * * *"},"launch_vars":{"gate_context":"api","other":"x"}}`
	id := parkUpdate(t, s, res.IntegrationID, body)

	aw := r6Approve(t, s, id)
	if aw.Code == http.StatusConflict {
		t.Fatalf("approve consumed a record whose remedy is a retry of the same request: %s", aw.Diagnostic(t))
	}
	if aw.Code != http.StatusInternalServerError || !strings.Contains(aw.Diagnostic(t), "converges") {
		t.Fatalf("a mid-write divergence must stay pending with the converge remedy named: code=%d body=%s", aw.Code, aw.Diagnostic(t))
	}
	if got := r6OrgList(t, s); len(got) != 1 {
		t.Fatalf("the retry-convergent record must stay pending: %v", got)
	}

	// The stores are diverged (the config half landed) — the state a re-run heals.
	integ, err := s.forgeIntegrations.Get(context.Background(), res.IntegrationID)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := s.webhookConfigs.Get(context.Background(), res.WebhookID)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.OperatorLaunchVars) != 2 {
		t.Fatalf("precondition: the enforcement half must carry the write: %v", cfg.OperatorLaunchVars)
	}
	if len(integ.LaunchVars) != 1 {
		t.Fatalf("precondition: the report half must lag: %v", integ.LaunchVars)
	}

	// The remedy the refusal destroys: the SAME body on the direct path lands
	// and heals the divergence.
	dw := newErrorRecorder()
	dreq := forgeReq(orgAdminCtx(), "PATCH", "/api/teams/t1/forge/repo-bots/"+res.IntegrationID, body, "t1")
	dreq.SetPathValue("integration_id", res.IntegrationID)
	s.handleUpdateForgeRepoBots(dw, dreq)
	if dw.Code != http.StatusOK {
		t.Fatalf("the same request re-run must land and heal: code=%d body=%s", dw.Code, dw.Diagnostic(t))
	}
	integ, err = s.forgeIntegrations.Get(context.Background(), res.IntegrationID)
	if err != nil || len(integ.LaunchVars) != 2 {
		t.Fatalf("the divergence did not heal: %v (%v)", integ.LaunchVars, err)
	}
	cfg, err = s.webhookConfigs.Get(context.Background(), res.WebhookID)
	if err != nil || len(cfg.OperatorLaunchVars) != 2 {
		t.Fatalf("the enforcement half lost the pins: %v (%v)", cfg.OperatorLaunchVars, err)
	}
}

// F1, read-back flavor: a transient READ failure in the post-write verify
// wraps ErrProvisionDiverged AFTER both writes landed. Approve must keep the
// record pending — the provision is already in force, and a second approve
// converges as a no-op — not delete a record whose change took effect.
func TestR7_ReadBackBlip_ApproveKeepsARecordThatLanded(t *testing.T) {
	s, _, done := newApprovalTestServer(t)
	defer done()
	connID := firstConnID(t, s)

	res := seedIntegration(t, s, connID, "group/api", `,"launch_vars":{"gate_context":"api"}`)
	flaky := &r7FlakyIntegrations{RepoIntegrationStore: forge.NewMemoryRepoIntegrationStore()}
	if err := flaky.Create(context.Background(), mustInteg(t, s, res.IntegrationID)); err != nil {
		t.Fatal(err)
	}
	r7WrapIntegrations(s, flaky)
	flaky.mu.Lock()
	flaky.armGetAfterUpd = true // the verify's read-back, right after the Update
	flaky.mu.Unlock()

	body := `{"schedule_crons":{"review-pr":"0 3 * * *"},"launch_vars":{"gate_context":"api","other":"x"}}`
	id := parkUpdate(t, s, res.IntegrationID, body)

	aw := r6Approve(t, s, id)
	if aw.Code == http.StatusConflict {
		t.Fatalf("approve consumed a record whose change had already landed: %s", aw.Body.String())
	}
	if aw.Code != http.StatusInternalServerError {
		t.Fatalf("a read-back blip must stay pending, got code=%d body=%s", aw.Code, aw.Body.String())
	}
	if got := r6OrgList(t, s); len(got) != 1 {
		t.Fatalf("the record must stay pending after a read blip: %v", got)
	}
	// The provision ALREADY LANDED: both stores carry the settings.
	integ, err := s.forgeIntegrations.Get(context.Background(), res.IntegrationID)
	if err != nil || len(integ.LaunchVars) != 2 {
		t.Fatalf("the landed write must be in force: %v (%v)", integ.LaunchVars, err)
	}
	// A retry converges: the same approval executes as a no-op and clears the queue.
	approveRecord(t, s, id)
	if got := r6OrgList(t, s); len(got) != 0 {
		t.Fatalf("the retry did not consume the record: %v", got)
	}
}

// F5a: a parked record naming a bot the catalog cannot auto-provision is a
// DETERMINISTIC dead end — the refusal is a static property of the recorded
// bot ids and no retry changes it. Approve must consume the record with its
// reason, not leave it pending forever with "awaiting approval" showing to
// the requester.
func TestR7_UnprovisionableBot_ApproveConsumesTheDeadEnd(t *testing.T) {
	s, _, done := newApprovalTestServer(t)
	defer done()
	connID := firstConnID(t, s)

	res := seedIntegration(t, s, connID, "group/api", ``)
	id := parkUpdate(t, s, res.IntegrationID, `{"bot_ids":["review-pr","docs-refresh"]}`)

	aw := r6Approve(t, s, id)
	if aw.Code != http.StatusConflict {
		t.Fatalf("an unprovisionable bot set is a deterministic dead end and must be consumed: code=%d body=%s", aw.Code, aw.Body.String())
	}
	if !strings.Contains(aw.Body.String(), "docs-refresh") {
		t.Fatalf("the 409 must name the refusal the re-submission must fix: %s", aw.Body.String())
	}
	if got := r6OrgList(t, s); len(got) != 0 {
		t.Fatalf("the dead-end record stayed pending: %v", got)
	}
	// Nothing landed: a consumed approval is not a provisioned one.
	integ, err := s.forgeIntegrations.Get(context.Background(), res.IntegrationID)
	if err != nil || len(integ.BotIDs) != 1 {
		t.Fatalf("approve must not provision a refused set: %v (%v)", integ.BotIDs, err)
	}
	// The record is gone: a second approve answers 404.
	w := httptest.NewRecorder()
	req := orgReq(orgAdminCtx(), "POST", "/api/orgs/o1/provision-approvals/"+id+"/approve", "", "o1")
	req.SetPathValue("approval_id", id)
	s.handleApproveProvision(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("the consumed record must be gone: code=%d body=%s", w.Code, w.Body.String())
	}
}

// The second static refusal of the same class: a bot whose forge block and
// invocations yield no forge events to subscribe to can never be
// provisioned either — the arm consumes the record with that reason.
func TestR7_NoForgeEvents_SameDeterministicDeadEnd(t *testing.T) {
	s, _, done := newApprovalTestServer(t)
	defer done()
	connID := firstConnID(t, s)

	res := seedIntegration(t, s, connID, "group/api", ``)
	orig := s.forgeOrchestrator.Bots
	s.forgeOrchestrator.Bots = func(_ context.Context, _, botID string) (*bundle.ForgeRequirements, error) {
		if botID == "docs-refresh" {
			// A forge block with NO events and no invocations: the event
			// union is empty, the refusal is static.
			return &bundle.ForgeRequirements{
				Events:      []string{},
				TokenScopes: map[string]string{"pull_requests": "write"},
				Secret:      "forge_token",
			}, nil
		}
		return orig(context.Background(), "t1", botID)
	}
	id := parkUpdate(t, s, res.IntegrationID, `{"bot_ids":["docs-refresh"]}`)

	aw := r6Approve(t, s, id)
	if aw.Code != http.StatusConflict {
		t.Fatalf("an empty event union is a deterministic dead end: code=%d body=%s", aw.Code, aw.Body.String())
	}
	if !strings.Contains(aw.Body.String(), "no forge events") {
		t.Fatalf("the 409 must name the refusal: %s", aw.Body.String())
	}
	if got := r6OrgList(t, s); len(got) != 0 {
		t.Fatalf("the dead-end record stayed pending: %v", got)
	}
}

// F2, the spoken half: a legacy record (parked before the Set flags shipped)
// that spoke launch_vars {} beside launch_vars_replace replays into a
// whole-map clear. The wire must carry the map the replay executes — an
// empty-but-PRESENT key — so the card renders the clear instead of "the
// stored pins stay".
func TestR7_LegacySpokenEmptyClear_WireCarriesTheClearTheReplayExecutes(t *testing.T) {
	s, _, done := newApprovalTestServer(t)
	defer done()
	connID := firstConnID(t, s)

	res := seedIntegration(t, s, connID, "group/api", `,"launch_vars":{"gate_context":"api"}`)

	// Exactly what a pre-flags park wrote: the spoken-empty map, the replace
	// gesture, no Set fact.
	legacy := forge.ProvisionApproval{
		ID:                "pa-legacy",
		OrgID:             "o1",
		TenantID:          "t1",
		ConnectionID:      connID,
		RepoFullName:      "group/api",
		BotIDs:            []string{"review-pr"},
		IntegrationID:     res.IntegrationID,
		BaseBotIDs:        []string{"review-pr"},
		LaunchVars:        map[string]string{},
		LaunchVarsReplace: true,
	}
	if err := s.provisionApprovals.Create(context.Background(), legacy); err != nil {
		t.Fatal(err)
	}
	rec, raw := orgApprovalWire(t, s)
	if _, ok := rec["launch_vars"]; !ok {
		t.Fatalf("the org wire must carry the spoken empty map the approval will execute (a silent record marshals null, a spoken one {}): %s", raw)
	}
	m, _ := rec["launch_vars"].(map[string]any)
	if m == nil {
		t.Fatalf("the spoken empty map collapsed to null on the org wire — byte-identical to a silent record: %s", raw)
	}

	// The replay executes the clear the wire now shows.
	w := httptest.NewRecorder()
	req := orgReq(orgAdminCtx(), "POST", "/api/orgs/o1/provision-approvals/pa-legacy/approve", "", "o1")
	req.SetPathValue("approval_id", "pa-legacy")
	s.handleApproveProvision(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("approve: code=%d body=%s", w.Code, w.Body.String())
	}
	integ, err := s.forgeIntegrations.Get(context.Background(), res.IntegrationID)
	if err != nil || len(integ.LaunchVars) != 0 {
		t.Fatalf("the clear must execute: %v (%v)", integ.LaunchVars, err)
	}
	cfg, err := s.webhookConfigs.Get(context.Background(), res.WebhookID)
	if err != nil || cfg.OperatorLaunchVars != nil {
		t.Fatalf("the enforcement half must be cleared too: %v (%v)", cfg.OperatorLaunchVars, err)
	}
}

// F2, the silent contrast: a legacy record that was genuinely silent about
// launch vars marshals a NULL map on the wire — the exact byte the card's
// "no map spoken" branch reads — and approve keeps the stored pins.
func TestR7_LegacySilentReplace_WireStaysNull(t *testing.T) {
	s, _, done := newApprovalTestServer(t)
	defer done()
	connID := firstConnID(t, s)

	res := seedIntegration(t, s, connID, "group/api", `,"launch_vars":{"gate_context":"api"}`)

	silent := forge.ProvisionApproval{
		ID:                "pa-silent",
		OrgID:             "o1",
		TenantID:          "t1",
		ConnectionID:      connID,
		RepoFullName:      "group/api",
		BotIDs:            []string{"review-pr"},
		IntegrationID:     res.IntegrationID,
		BaseBotIDs:        []string{"review-pr"},
		LaunchVarsReplace: true,
	}
	if err := s.provisionApprovals.Create(context.Background(), silent); err != nil {
		t.Fatal(err)
	}
	rec, raw := orgApprovalWire(t, s)
	if v, ok := rec["launch_vars"]; ok && v != nil {
		t.Fatalf("a silent record must marshal a null map, got %v: %s", v, raw)
	}
	approveRecord(t, s, "pa-silent")
	integ, err := s.forgeIntegrations.Get(context.Background(), res.IntegrationID)
	if err != nil || integ.LaunchVars["gate_context"] != "api" {
		t.Fatalf("a silent replace must keep the stored pins: %v (%v)", integ.LaunchVars, err)
	}
}

// The arm's delete-failure branch: when the record cannot be removed, the
// 409 must not claim it was — the approver is told to reject the record
// instead of trusting a queue that still holds it.
func TestR7_ConsumedButNotDeleted_The409SaysSo(t *testing.T) {
	s, _, done := newApprovalTestServer(t)
	defer done()
	connID := firstConnID(t, s)

	res := seedIntegration(t, s, connID, "group/api", ``)
	id := parkUpdate(t, s, res.IntegrationID, `{"bot_ids":["review-pr","docs-refresh"]}`)

	orig := s.provisionApprovals
	s.provisionApprovals = &r7StuckApprovals{ProvisionApprovalStore: orig}
	aw := r6Approve(t, s, id)
	if aw.Code != http.StatusConflict {
		t.Fatalf("a deterministic refusal answers 409 even when the delete fails: code=%d body=%s", aw.Code, aw.Body.String())
	}
	if strings.Contains(aw.Body.String(), "it has been removed from the queue") {
		t.Fatalf("the 409 must not claim a removal that did not happen: %s", aw.Body.String())
	}
	if !strings.Contains(aw.Body.String(), "could not be removed") {
		t.Fatalf("the 409 must say the record stayed: %s", aw.Body.String())
	}
	// The record is in fact still there.
	if got := r6OrgList(t, s); len(got) != 1 {
		t.Fatalf("the record should have survived the failed delete: %v", got)
	}
}

// r7StuckApprovals fails every Delete — a store that cannot remove a record.
type r7StuckApprovals struct {
	forge.ProvisionApprovalStore
}

func (s *r7StuckApprovals) Delete(ctx context.Context, id string) error {
	return context.DeadlineExceeded
}

func mustInteg(t *testing.T, s *Server, id string) forge.RepoIntegration {
	t.Helper()
	ri, err := s.forgeIntegrations.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return ri
}

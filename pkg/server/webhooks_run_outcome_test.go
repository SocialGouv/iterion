package server

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/usernotify"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// seedWebhookDelivery writes a launched delivery row naming runID.
func seedWebhookDelivery(t *testing.T, st webhooks.DeliveryStore, id, tenantID, runID string) {
	t.Helper()
	if err := st.Insert(context.Background(), webhooks.Delivery{
		ID: id, TenantID: tenantID, WebhookID: "hook", Provider: webhooks.ProviderGitHub,
		IdempotencyKey: "key-" + id, Status: webhooks.StatusLaunched, RunID: runID,
		BotID: "review-pr", ReceivedAt: time.Now().UTC().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("seed delivery %s: %v", id, err)
	}
}

// seedDeliveryRun writes a run in `status` whose typed provenance is the
// inbound delivery `deliveryID` — the shape the webhook launch path stamps —
// and returns the listing row the cloud window scan would produce for it.
func seedDeliveryRun(t *testing.T, f *spineFixture, runID, tenantID, deliveryID string, status store.RunStatus, errMsg string, code store.FailureCode, at time.Time) usernotify.RunRef {
	t.Helper()
	rs := f.srv.runs.RunStore()
	rctx := store.WithTenant(context.Background(), tenantID)
	if _, err := rs.CreateRun(rctx, runID, "review-pr", nil); err != nil {
		t.Fatalf("create run %s: %v", runID, err)
	}
	r, err := rs.LoadRun(rctx, runID)
	if err != nil {
		t.Fatalf("load run %s: %v", runID, err)
	}
	r.TenantID = tenantID
	r.Status = status
	r.Error = errMsg
	r.FailureCode = code
	r.UpdatedAt = at
	r.Source = &store.RunSource{Kind: store.RunSourceKindWebhook, DeliveryID: deliveryID}
	if err := rs.SaveRun(rctx, r); err != nil {
		t.Fatalf("save run %s: %v", runID, err)
	}
	return usernotify.RunRef{ID: runID, Status: string(status), UpdatedAt: at}
}

// TestRunOutcomeSpineStampsTheWebhookDelivery is unit 1501.3's sink witness:
// a webhook-launched run that reached a terminal status with NO run-outcome
// event reaching a subscriber is still reported on the delivery row that
// launched it, because the delivery is a fourth runOutcomeSink on the spine
// whose sweep is the authority.
//
// Mutations, each on the CODE:
//   - delete the webhook-delivery sink from runOutcomeSinks → LastRunStatus
//     stays empty: red on its own assertion;
//   - gate deliveryOutcomeRecordID on the id alone (drop the Kind check) and
//     it still passes — which is why the foreign-kind row below exists;
//   - drop `last_run_status` from either store twin's write → red (the
//     store-level witness is TestDeliveryStore_RunOutcomeContract).
func TestRunOutcomeSpineStampsTheWebhookDelivery(t *testing.T) {
	f := newSpineFixture(t)
	deliveries := webhooks.NewMemoryDeliveryStore()
	f.srv.webhookDeliveries = deliveries
	const tenantID = "tenant-A"

	// Terminated 10 minutes ago: inside the sweep's lookback, outside its
	// grace, so the sweep owns it.
	now := time.Now().UTC()
	terminatedAt := now.Add(-10 * time.Minute)
	seedWebhookDelivery(t, deliveries, "d-launched", tenantID, "run-webhook")
	ref := seedDeliveryRun(t, f, "run-webhook", tenantID, "d-launched",
		store.RunStatusFailed, "boom", store.FailureSandboxDriverUnavailable, terminatedAt)
	f.listed = []usernotify.RunRef{ref}

	f.srv.runOutcomeSweepPass(context.Background(), now)

	got, err := deliveries.Get(context.Background(), "d-launched")
	if err != nil {
		t.Fatalf("get delivery: %v", err)
	}
	if got.LastRunStatus != string(store.RunStatusFailed) {
		t.Errorf("LastRunStatus=%q; want %q — the delivery still reads as merely launched", got.LastRunStatus, string(store.RunStatusFailed))
	}
	if got.LastRunError != "boom" {
		t.Errorf("LastRunError=%q; want %q", got.LastRunError, "boom")
	}
	if got.LastRunErrorCode != string(store.FailureSandboxDriverUnavailable) {
		t.Errorf("LastRunErrorCode=%q; want %q", got.LastRunErrorCode, string(store.FailureSandboxDriverUnavailable))
	}
	if got.LastRunAt == nil || !got.LastRunAt.Equal(terminatedAt) {
		t.Errorf("LastRunAt=%v; want the run document's own instant %v", got.LastRunAt, terminatedAt)
	}
	// The delivery's OWN verdict is a separate fact and must be untouched.
	if got.Status != webhooks.StatusLaunched {
		t.Errorf("delivery Status=%q; want %q — the run's outcome overwrote the delivery's own verdict", got.Status, webhooks.StatusLaunched)
	}
}

// TestRunOutcomeSpineLeavesAForeignDeliveryIDAlone pins the Kind half of the
// delivery sink's back-reference: `delivery_id` has exactly one producer, so
// a run carrying one under another kind is a corrupt record, not a launch
// path that forgot its kind — and it must not reach the delivery store.
//
// Mutation: drop the `run.Source.Kind != store.RunSourceKindWebhook` guard
// from deliveryOutcomeRecordID → the dispatcher-kind run stamps the delivery
// → red on the untouched assertion.
func TestRunOutcomeSpineLeavesAForeignDeliveryIDAlone(t *testing.T) {
	f := newSpineFixture(t)
	deliveries := webhooks.NewMemoryDeliveryStore()
	f.srv.webhookDeliveries = deliveries
	const tenantID = "tenant-A"
	now := time.Now().UTC()
	terminatedAt := now.Add(-10 * time.Minute)
	seedWebhookDelivery(t, deliveries, "d-foreign", tenantID, "run-foreign")
	ref := seedDeliveryRun(t, f, "run-foreign", tenantID, "d-foreign",
		store.RunStatusFailed, "boom", store.FailureSandboxDriverUnavailable, terminatedAt)

	// Re-stamp the run with the SAME delivery id under another kind.
	rs := f.srv.runs.RunStore()
	rctx := store.WithTenant(context.Background(), tenantID)
	r, err := rs.LoadRun(rctx, "run-foreign")
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	r.Source = &store.RunSource{Kind: store.RunSourceKindDispatcher, DeliveryID: "d-foreign"}
	r.UpdatedAt = terminatedAt
	if err := rs.SaveRun(rctx, r); err != nil {
		t.Fatalf("save run: %v", err)
	}
	f.listed = []usernotify.RunRef{ref}

	f.srv.runOutcomeSweepPass(context.Background(), now)

	got, err := deliveries.Get(context.Background(), "d-foreign")
	if err != nil {
		t.Fatalf("get delivery: %v", err)
	}
	if got.LastRunStatus != "" || got.LastRunAt != nil {
		t.Errorf("a run whose Kind is not webhook stamped the delivery: (%q, %v); want untouched", got.LastRunStatus, got.LastRunAt)
	}
}

// TestWebhookLaunchStampsTheDeliveryProvenance is unit 1501.3's LAUNCH-side
// witness, the half the sink cannot prove: the back-reference the spine reads
// only exists because the webhook launch path puts it on the run it starts.
// The published LaunchSpec is the oracle — it is what carries provenance to
// the queued doc (the RunMessage has no source field, see
// cloudpublisher.SubmitLaunch).
//
// Mutation: drop the `spec.SourceRef = &store.RunSource{…}` block from
// launchWebhookBot → SourceRef is nil: red on its own assertion, and the
// spine's delivery sink goes silent for every real webhook run.
func TestWebhookLaunchStampsTheDeliveryProvenance(t *testing.T) {
	pub := &tierPublisher{}
	s, rs := newTeamForkServer(t, pub)
	pub.onLaunch = func(runID string) { finishRunAs(t, rs, runID, store.RunStatusFinished) }
	configs := webhooks.NewMemoryConfigStore()
	s.webhookConfigs = configs
	cfg := webhooks.Config{ID: "hook", TenantID: "t1", Enabled: true, BotIDs: []string{"probe"}, DefaultBotID: "probe"}
	if err := configs.Create(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	ctx := store.WithIdentity(auth.WithIdentity(t.Context(), auth.Identity{
		UserID: "webhook:hook", TeamID: "t1", Kind: auth.KindWebhook,
	}), "t1", "webhook:hook")
	if _, err := s.launchWebhookBot(ctx, cfg, "probe", map[string]string{}, "", "", "acme/repo",
		nil, nil, store.RunTrustDefault, "", "d-42"); err != nil {
		t.Fatalf("launchWebhookBot = %v, want nil", err)
	}
	spec := pub.only(t)
	if spec.SourceRef == nil {
		t.Fatal("the published LaunchSpec carries no SourceRef — a webhook run reaches the store with no delivery back-reference, and the spine's delivery sink can never find its row")
	}
	if spec.SourceRef.Kind != store.RunSourceKindWebhook || spec.SourceRef.DeliveryID != "d-42" {
		t.Fatalf("SourceRef = %+v; want {kind webhook, delivery_id d-42}", spec.SourceRef)
	}
}

// A launch with no delivery row behind it — the /revi approve lane and the
// relaunch surfaces call the launcher directly — must stay unstamped: an
// empty delivery_id would make deriveSourceKind report "webhook" over a
// back-reference that points at nothing, and the spine would offer the
// delivery store an empty id on every such terminal.
func TestWebhookLaunchWithoutADeliveryLeavesSourceNil(t *testing.T) {
	pub := &tierPublisher{}
	s, rs := newTeamForkServer(t, pub)
	pub.onLaunch = func(runID string) { finishRunAs(t, rs, runID, store.RunStatusFinished) }
	configs := webhooks.NewMemoryConfigStore()
	s.webhookConfigs = configs
	cfg := webhooks.Config{ID: "hook", TenantID: "t1", Enabled: true, BotIDs: []string{"probe"}, DefaultBotID: "probe"}
	if err := configs.Create(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	ctx := store.WithIdentity(auth.WithIdentity(t.Context(), auth.Identity{
		UserID: "webhook:hook", TeamID: "t1", Kind: auth.KindWebhook,
	}), "t1", "webhook:hook")
	if _, err := s.launchWebhookBot(ctx, cfg, "probe", map[string]string{}, "", "", "acme/repo",
		nil, nil, store.RunTrustDefault, "", ""); err != nil {
		t.Fatalf("launchWebhookBot = %v, want nil", err)
	}
	if spec := pub.only(t); spec.SourceRef != nil {
		t.Fatalf("SourceRef = %+v; want nil when no delivery row backs the launch", spec.SourceRef)
	}
}

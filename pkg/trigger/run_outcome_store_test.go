package trigger

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// assertSubscriptionRunOutcomeContract is the shared conformance check for
// SubscriptionStore.MarkRunOutcome — both twins (memory + mongo) run it, so
// the contract stays one place (#1501). It is the sibling of
// cloudsched's assertMarkRunOutcomeContract, deliberately the same five
// properties: (1) a failure stamps id + status + error + code + at; (2) a
// subsequent success CLEARS the error/code (the field is a health signal,
// not a permanent record); (3) an OUT-OF-ORDER delivery is a silent no-op,
// so the health fields never move backwards; (4) an equal-timestamp
// redelivery still commits; (5) an unknown id returns
// ErrSubscriptionNotFound.
//
// Mutation that reddens it on BOTH twins: drop `last_run_status` from the
// store's write (the `$set` entry in mongostore.go, the field assignment in
// memstore.go) → the status row below reddens on its own assertion.
func assertSubscriptionRunOutcomeContract(t *testing.T, store SubscriptionStore, subID string, now time.Time) {
	t.Helper()
	ctx := context.Background()

	// Failure: stamps all five fields.
	if err := store.MarkRunOutcome(ctx, subID, "run-boom", "failed",
		"sandbox: strict mode requested but no runtime available",
		"SANDBOX_DRIVER_UNAVAILABLE", now); err != nil {
		t.Fatalf("MarkRunOutcome failure: %v", err)
	}
	got, err := store.Get(ctx, subID)
	if err != nil {
		t.Fatalf("Get after failure: %v", err)
	}
	if got.LastRunID != "run-boom" || got.LastRunStatus != "failed" {
		t.Errorf("outcome fields = (%q, %q); want (run-boom, failed)", got.LastRunID, got.LastRunStatus)
	}
	if got.LastRunError == "" || got.LastRunErrorCode != "SANDBOX_DRIVER_UNAVAILABLE" {
		t.Errorf("outcome error = (%q, %q); want non-empty msg + SANDBOX_DRIVER_UNAVAILABLE code", got.LastRunError, got.LastRunErrorCode)
	}
	if got.LastRunAt == nil {
		t.Error("LastRunAt is nil after MarkRunOutcome")
	}
	// A launch-time verdict and a run outcome are different facts on the
	// same row: stamping the outcome must not disturb LastError.
	if err := store.MarkLaunchError(ctx, subID, "", now); err != nil {
		t.Fatalf("MarkLaunchError(clear): %v", err)
	}
	got, _ = store.Get(ctx, subID)
	if got.LastRunID != "run-boom" {
		t.Errorf("LastRunID=%q after a launch-health write; want it untouched — the two verdicts are independent fields", got.LastRunID)
	}

	// Success on the next firing CLEARS the previous error/code so the
	// health surface tracks the LAST outcome, not the last FAILURE.
	if err := store.MarkRunOutcome(ctx, subID, "run-ok", "finished", "", "", now.Add(time.Minute)); err != nil {
		t.Fatalf("MarkRunOutcome success: %v", err)
	}
	got, _ = store.Get(ctx, subID)
	if got.LastRunID != "run-ok" || got.LastRunStatus != "finished" {
		t.Errorf("success fields = (%q, %q); want (run-ok, finished)", got.LastRunID, got.LastRunStatus)
	}
	if got.LastRunError != "" || got.LastRunErrorCode != "" {
		t.Errorf("success should clear error = (%q, %q); want empty", got.LastRunError, got.LastRunErrorCode)
	}

	// Out-of-order delivery: a stamp OLDER than the one on the row (the
	// failed run finishing late, after its successor's success was
	// recorded) must be a silent no-op. Mutation: drop the monotonicity
	// guard in either twin → last_run_id reverts to "run-boom" → red.
	if err := store.MarkRunOutcome(ctx, subID, "run-boom", "failed",
		"late delivery of the failed run", "SANDBOX_DRIVER_UNAVAILABLE", now.Add(30*time.Second)); err != nil {
		t.Fatalf("MarkRunOutcome out-of-order: %v", err)
	}
	got, _ = store.Get(ctx, subID)
	if got.LastRunID != "run-ok" || got.LastRunStatus != "finished" {
		t.Errorf("out-of-order delivery moved the field backwards: (%q, %q); want (run-ok, finished)", got.LastRunID, got.LastRunStatus)
	}
	if got.LastRunError != "" || got.LastRunErrorCode != "" {
		t.Errorf("out-of-order delivery resurrected the error = (%q, %q); want empty", got.LastRunError, got.LastRunErrorCode)
	}

	// Equal-timestamp redelivery (the same event replayed by the queue
	// group, or two replicas sweeping at once) must be an idempotent no-op
	// that still commits — the `$lte` / `!After` guard admits
	// `at == last_run_at`. This row pins that the boundary stays admitted.
	if err := store.MarkRunOutcome(ctx, subID, "run-ok", "finished", "", "", now.Add(time.Minute)); err != nil {
		t.Fatalf("MarkRunOutcome equal-at redelivery: %v", err)
	}
	got, _ = store.Get(ctx, subID)
	if got.LastRunID != "run-ok" || got.LastRunStatus != "finished" || got.LastRunError != "" {
		t.Errorf("equal-at redelivery changed the row: (%q, %q, %q); want unchanged", got.LastRunID, got.LastRunStatus, got.LastRunError)
	}

	// Unknown id → ErrSubscriptionNotFound (both twins). The spine warns on
	// anything else, so a purged row must be distinguishable from an outage.
	if err := store.MarkRunOutcome(ctx, "does-not-exist", "run-x", "failed", "boom", "code", now); !errors.Is(err, ErrSubscriptionNotFound) {
		t.Errorf("MarkRunOutcome on an unknown id = %v, want ErrSubscriptionNotFound", err)
	}
}

// TestMemorySubscriptionStore_RunOutcomeContract runs the shared contract
// against the in-memory twin.
func TestMemorySubscriptionStore_RunOutcomeContract(t *testing.T) {
	store := NewMemorySubscriptionStore()
	now := time.Now().UTC().Truncate(time.Millisecond)
	if err := store.Create(context.Background(), Subscription{
		ID: "sub-1", TenantID: "t1", BotID: "feed-watch", Enabled: true,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	assertSubscriptionRunOutcomeContract(t, store, "sub-1", now)
}

// TestMongoSubscriptionStore_RunOutcomeContract runs the SAME contract
// against the mongo twin, so the two can never drift. Gated on the
// mongo-conformance harness like every other mongo row in this package.
func TestMongoSubscriptionStore_RunOutcomeContract(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; requires Mongo conformance harness")
	}
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatal(err)
	}
	db := client.Database("iterion_sub_outcome_" + bson.NewObjectID().Hex())
	t.Cleanup(func() {
		ctx, cancel := mongotest.TeardownCtx()
		defer cancel()
		_ = db.Drop(ctx)
		_ = client.Disconnect(ctx)
	})
	store := NewMongoSubscriptionStore(db)
	if err := store.EnsureSchema(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	if err := store.Create(ctx, Subscription{
		ID: "sub-1", TenantID: "t1", BotID: "feed-watch", Enabled: true,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	assertSubscriptionRunOutcomeContract(t, store, "sub-1", now)
}

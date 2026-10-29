package webhooks

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestDeliveryStore_RunOutcomeContract is the shared conformance check for
// DeliveryStore.MarkRunOutcome — both twins run it, so the contract stays
// one place (#1501). It is the sibling of
// pkg/cloudsched's assertMarkRunOutcomeContract and of trigger's
// assertSubscriptionRunOutcomeContract, with the same five properties plus
// the one a delivery adds:
//
//  1. a failure stamps status + error + code + at;
//  2. a subsequent success CLEARS the error/code (health signal, not an
//     audit trail);
//  3. an OUT-OF-ORDER delivery is a silent no-op, so the fields never move
//     backwards;
//  4. an equal-timestamp redelivery still commits;
//  5. an unknown id returns ErrNotFound;
//  6. (the delivery's own) a stamp naming a run this row does NOT report is
//     refused — the retry of a failed delivery reuses the row under a NEW
//     run id, and the predecessor's late terminal must not describe the
//     successor.
//
// Mutation that reddens it on BOTH twins: drop `last_run_status` from the
// store's write (the `$set` entry in mongo.go, the field assignment in
// memory.go) → the status row below reddens on its own assertion.
func TestDeliveryStore_RunOutcomeContract(t *testing.T) {
	onDeliveryStoreTwins(t, func(t *testing.T, st DeliveryStore) {
		ctx := context.Background()
		now := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
		const id, runID = "d-outcome", "run-boom"
		if err := st.Insert(ctx, Delivery{
			ID: id, TenantID: "t1", WebhookID: "w1", Provider: ProviderGitHub,
			IdempotencyKey: "key-" + id, Status: StatusLaunched, RunID: runID,
			ReceivedAt: now.Add(-time.Minute),
		}); err != nil {
			t.Fatalf("Insert: %v", err)
		}

		// Failure: stamps the four fields.
		if err := st.MarkRunOutcome(ctx, id, runID, "failed",
			"sandbox: strict mode requested but no runtime available",
			"SANDBOX_DRIVER_UNAVAILABLE", now); err != nil {
			t.Fatalf("MarkRunOutcome failure: %v", err)
		}
		got, err := st.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get after failure: %v", err)
		}
		if got.LastRunStatus != "failed" {
			t.Errorf("LastRunStatus = %q; want failed", got.LastRunStatus)
		}
		if got.LastRunError == "" || got.LastRunErrorCode != "SANDBOX_DRIVER_UNAVAILABLE" {
			t.Errorf("outcome error = (%q, %q); want non-empty msg + SANDBOX_DRIVER_UNAVAILABLE code", got.LastRunError, got.LastRunErrorCode)
		}
		if got.LastRunAt == nil || !got.LastRunAt.Equal(now) {
			t.Errorf("LastRunAt = %v; want the run document's own instant %v", got.LastRunAt, now)
		}
		// The DELIVERY's own verdict is a different fact on the same row:
		// stamping the run outcome must not disturb Status/Error.
		if got.Status != StatusLaunched || got.Error != "" {
			t.Errorf("delivery verdict = (%q, %q) after a run-outcome write; want (%q, \"\") — the two verdicts are independent fields", got.Status, got.Error, StatusLaunched)
		}

		// A success on the same run CLEARS the previous error/code, so the
		// health surface tracks the LAST outcome, not the last FAILURE. (A
		// delivery reaches this through a resume of the same run — the
		// relaunch case is row 6 below.)
		if err := st.MarkRunOutcome(ctx, id, runID, "finished", "", "", now.Add(time.Minute)); err != nil {
			t.Fatalf("MarkRunOutcome success: %v", err)
		}
		got, _ = st.Get(ctx, id)
		if got.LastRunStatus != "finished" {
			t.Errorf("LastRunStatus = %q; want finished", got.LastRunStatus)
		}
		if got.LastRunError != "" || got.LastRunErrorCode != "" {
			t.Errorf("success should clear error = (%q, %q); want empty", got.LastRunError, got.LastRunErrorCode)
		}

		// Out-of-order: a stamp OLDER than the one on the row (the failed
		// episode delivered late, after the success was recorded) is a
		// silent no-op. Mutation: drop the monotonicity guard in either twin
		// → LastRunStatus reverts to "failed" → red.
		if err := st.MarkRunOutcome(ctx, id, runID, "failed",
			"late delivery of the failed episode", "SANDBOX_DRIVER_UNAVAILABLE", now.Add(30*time.Second)); err != nil {
			t.Fatalf("MarkRunOutcome out-of-order: %v", err)
		}
		got, _ = st.Get(ctx, id)
		if got.LastRunStatus != "finished" {
			t.Errorf("out-of-order delivery moved the field backwards: %q; want finished", got.LastRunStatus)
		}
		if got.LastRunError != "" || got.LastRunErrorCode != "" {
			t.Errorf("out-of-order delivery resurrected the error = (%q, %q); want empty", got.LastRunError, got.LastRunErrorCode)
		}

		// Equal-timestamp redelivery (the same event replayed by the queue
		// group, or two replicas sweeping at once) is an idempotent no-op
		// that still commits — the `$lte` / `!After` guard admits
		// `at == last_run_at`. This row pins that the boundary stays
		// admitted.
		if err := st.MarkRunOutcome(ctx, id, runID, "finished", "", "", now.Add(time.Minute)); err != nil {
			t.Fatalf("MarkRunOutcome equal-at redelivery: %v", err)
		}
		got, _ = st.Get(ctx, id)
		if got.LastRunStatus != "finished" || got.LastRunError != "" {
			t.Errorf("equal-at redelivery changed the row: (%q, %q); want unchanged", got.LastRunStatus, got.LastRunError)
		}

		// Unknown id → ErrNotFound (both twins). The spine warns on anything
		// else, so a purged row must be distinguishable from an outage.
		if err := st.MarkRunOutcome(ctx, "does-not-exist", "run-x", "failed", "boom", "code", now); !errors.Is(err, ErrNotFound) {
			t.Errorf("MarkRunOutcome on an unknown id = %v, want ErrNotFound", err)
		}

		// The row's own run is the MATCH KEY. A retried delivery relaunched
		// under "run-relaunched"; the predecessor's terminal arrives late and
		// must not describe the successor. Mutation: drop the `run_id` half of
		// the filter (mongo) or the RunID comparison (memory) → the row reads
		// "failed" for a run it does not report → red.
		relaunched := Delivery{
			ID: "d-relaunched", TenantID: "t1", WebhookID: "w1", Provider: ProviderGitHub,
			IdempotencyKey: "key-d-relaunched", Status: StatusLaunched, RunID: "run-relaunched",
			ReceivedAt: now.Add(-time.Minute),
		}
		if err := st.Insert(ctx, relaunched); err != nil {
			t.Fatalf("Insert relaunched: %v", err)
		}
		if err := st.MarkRunOutcome(ctx, relaunched.ID, "run-predecessor", "failed",
			"the attempt this row no longer reports", "SANDBOX_DRIVER_UNAVAILABLE", now); err != nil {
			t.Fatalf("MarkRunOutcome foreign run: %v", err)
		}
		got, err = st.Get(ctx, relaunched.ID)
		if err != nil {
			t.Fatalf("Get relaunched: %v", err)
		}
		if got.LastRunStatus != "" || got.LastRunError != "" || got.LastRunAt != nil {
			t.Errorf("a stamp naming run-predecessor landed on a row reporting run-relaunched: (%q, %q, %v); want untouched", got.LastRunStatus, got.LastRunError, got.LastRunAt)
		}

		// A row that launched NO run has no outcome of its own to carry: a
		// stamp against it is refused rather than matched by an empty run_id.
		filtered := Delivery{
			ID: "d-filtered", TenantID: "t1", WebhookID: "w1", Provider: ProviderGitHub,
			IdempotencyKey: "key-d-filtered", Status: StatusFiltered,
			ReceivedAt: now.Add(-time.Minute),
		}
		if err := st.Insert(ctx, filtered); err != nil {
			t.Fatalf("Insert filtered: %v", err)
		}
		if err := st.MarkRunOutcome(ctx, filtered.ID, "", "failed", "boom", "code", now); err != nil {
			t.Fatalf("MarkRunOutcome on a row that launched nothing: %v", err)
		}
		got, _ = st.Get(ctx, filtered.ID)
		if got.LastRunStatus != "" || got.LastRunAt != nil {
			t.Errorf("a run-less delivery was stamped: (%q, %v); want untouched", got.LastRunStatus, got.LastRunAt)
		}
	})
}

package boardmongo_test

import (
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
)

// runRunOutcomeSuite pins BoardStore.SetRunOutcome on BOTH twins — the
// #1501 row for the dispatcher card, sibling of cloudsched's
// assertMarkRunOutcomeContract and trigger's
// assertSubscriptionRunOutcomeContract.
//
// Properties: (1) a failed run stamps status + error + code + at; (2) a
// later successful run on the same card CLEARS the error/code (health
// signal, not audit trail — Runs is the audit); (3) an outcome naming a run
// the card has been re-dispatched PAST is dropped, because LastRunID is
// what says which run the card reports; (4) a card with no run at all is
// never stamped; (5) a redelivery of the same outcome is an idempotent
// no-op.
//
// Mutation that reddens it on BOTH twins: drop `lastrunstatus` from the
// store's write (the `$set`/`replace` field list in boardmongo, the field
// assignment in the FS twin) → row (1) reddens on its own assertion.
func runRunOutcomeSuite(t *testing.T, store native.BoardStore) {
	t.Helper()
	at := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)

	iss, err := store.Create(native.Issue{Title: "outcome card", State: native.StateReady})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// The launcher's own stamp: this is what makes the card report run-1.
	if err := store.SetLastRun(iss.ID, "run-1", ""); err != nil {
		t.Fatalf("SetLastRun: %v", err)
	}

	// (1) A failed run stamps the whole tuple.
	if err := store.SetRunOutcome(iss.ID, "run-1", "failed",
		"sandbox: strict mode requested but no runtime available",
		"SANDBOX_DRIVER_UNAVAILABLE", at); err != nil {
		t.Fatalf("SetRunOutcome(failed): %v", err)
	}
	got, err := store.Get(iss.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.LastRunStatus != "failed" {
		t.Errorf("LastRunStatus=%q; want %q — the card still reads as merely dispatched", got.LastRunStatus, "failed")
	}
	if got.LastRunError == "" || got.LastRunErrorCode != "SANDBOX_DRIVER_UNAVAILABLE" {
		t.Errorf("outcome error = (%q, %q); want non-empty msg + SANDBOX_DRIVER_UNAVAILABLE", got.LastRunError, got.LastRunErrorCode)
	}
	if got.LastRunAt == nil || !got.LastRunAt.Equal(at) {
		t.Errorf("LastRunAt=%v; want the run's own terminal instant %v", got.LastRunAt, at)
	}

	// (5) A redelivery of the same outcome changes nothing and still
	// answers nil — what lets every replica sweep at once.
	if err := store.SetRunOutcome(iss.ID, "run-1", "failed",
		"sandbox: strict mode requested but no runtime available",
		"SANDBOX_DRIVER_UNAVAILABLE", at.Add(time.Second)); err != nil {
		t.Fatalf("SetRunOutcome(redelivery): %v", err)
	}
	got, _ = store.Get(iss.ID)
	if got.LastRunAt == nil || !got.LastRunAt.Equal(at) {
		t.Errorf("LastRunAt=%v after an identical redelivery; want it untouched at %v", got.LastRunAt, at)
	}

	// (3) An outcome for a run the card has moved PAST is dropped: the
	// re-dispatch repointed LastRunID, and run-1's late terminal is no
	// longer this card's state.
	if err := store.SetLastRun(iss.ID, "run-2", ""); err != nil {
		t.Fatalf("SetLastRun(run-2): %v", err)
	}
	if err := store.SetRunOutcome(iss.ID, "run-1", "cancelled", "late", "LATE", at.Add(time.Minute)); err != nil {
		t.Fatalf("SetRunOutcome(stale run): %v", err)
	}
	got, _ = store.Get(iss.ID)
	if got.LastRunStatus != "failed" {
		t.Errorf("LastRunStatus=%q after a stale run's outcome; want the card to still report run-1's own %q", got.LastRunStatus, "failed")
	}

	// (2) The new run succeeds: the previous failure is cleared.
	if err := store.SetRunOutcome(iss.ID, "run-2", "finished", "", "", at.Add(2*time.Minute)); err != nil {
		t.Fatalf("SetRunOutcome(finished): %v", err)
	}
	got, _ = store.Get(iss.ID)
	if got.LastRunStatus != "finished" {
		t.Errorf("LastRunStatus=%q; want %q", got.LastRunStatus, "finished")
	}
	if got.LastRunError != "" || got.LastRunErrorCode != "" {
		t.Errorf("success should clear error = (%q, %q); want empty", got.LastRunError, got.LastRunErrorCode)
	}

	// (4) A card the dispatcher never launched anything for is never
	// stamped: there is no run to report, and LastRunID "" must not be
	// treated as "matches everything".
	fresh, err := store.Create(native.Issue{Title: "never launched", State: native.StateReady})
	if err != nil {
		t.Fatalf("Create(fresh): %v", err)
	}
	if err := store.SetRunOutcome(fresh.ID, "run-x", "failed", "boom", "CODE", at); err != nil {
		t.Fatalf("SetRunOutcome(no run): %v", err)
	}
	got, _ = store.Get(fresh.ID)
	if got.LastRunStatus != "" {
		t.Errorf("LastRunStatus=%q on a card with no run; want empty", got.LastRunStatus)
	}
}

// TestNativeStore_RunOutcomeConformance runs the suite against the
// reference filesystem implementation (always runs).
func TestNativeStore_RunOutcomeConformance(t *testing.T) {
	store, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("native.NewStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	runRunOutcomeSuite(t, store)
}

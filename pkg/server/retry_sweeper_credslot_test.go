package server

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/server/cloudpublisher"
	"github.com/SocialGouv/iterion/pkg/store"
	mongostore "github.com/SocialGouv/iterion/pkg/store/mongo"
)

// A run parked on a credential's hard concurrency cap asks for a slot again
// on the slot cadence, FREE: the resume refused before any publish, the
// attempt budget prices work that reaches the provider, and a slot wait
// charged per ask would retire the run in minutes on a key busy for an hour.
// Red when the ErrCredentialAtCapacity branch is dropped (the generic re-arm
// then spends an attempt, fifteen minutes out).
func TestSweepDueRetries_ACredentialSlotWaitIsDeferredFreeOfCharge(t *testing.T) {
	st := newFakeRetryStore()
	st.claimWins["run-slot"] = true
	resumer := &fakeResumer{failing: fmt.Errorf("cloudpublisher: resume of run run-slot refused before publishing, the run put back as it was: %w", cloudpublisher.ErrCredentialAtCapacity)}
	s := newRetrySweeperServer(t, st, resumer)

	now := time.Now().UTC()
	ref := dueRef("run-slot", now.Add(-time.Minute))
	ref.RetryState.Reason = store.RetryReasonCredSlot
	ref.RetryState.Code = string(store.FailureCredSlotWait)
	s.sweepDueRetries(context.Background(), &fakeRetryLister{refs: []mongostore.RetryDueRef{ref}}, resumer, now)

	if len(resumer.calls) != 1 {
		t.Fatalf("Resume called %d times, want 1", len(resumer.calls))
	}
	if _, paid := st.armBudget["run-slot"]; paid {
		t.Fatal("a slot wait spent an attempt (ScheduleRunRetry was called) — it must be deferred through DelayRunRetry")
	}
	if _, ok := st.abandoned["run-slot"]; ok {
		t.Fatalf("a slot wait inside max_wait was abandoned: %q", st.abandoned["run-slot"])
	}
	got, ok := st.rearmed["run-slot"]
	if !ok {
		t.Fatal("the slot wait was not deferred: DelayRunRetry never ran")
	}
	want := now.Add(cloudpublisher.CredSlotProbeInterval)
	if got.Before(want) || got.After(want.Add(time.Minute)) {
		t.Fatalf("deferred to %s, want about %s (one slot probe interval out)", got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// A run armed for a USAGE WINDOW whose resume finds its key full waits for a
// slot now: the deferral re-tags the row cred_slot in the same write, or
// every reader keyed on the reason — the probe pass under a short floor,
// UsageParkWall's notices, the audit trail — keeps treating it as the
// provider wall it no longer waits on. Red when the deferral passes no
// reason (the row keeps usage_window).
func TestSweepDueRetries_AUsageWindowRowRefusedOnTheCapIsRetaggedAsASlotWait(t *testing.T) {
	st := newFakeRetryStore()
	st.claimWins["run-wall"] = true
	resumer := &fakeResumer{failing: fmt.Errorf("cloudpublisher: resume of run run-wall refused before publishing, the run put back as it was: %w", cloudpublisher.ErrCredentialAtCapacity)}
	s := newRetrySweeperServer(t, st, resumer)

	now := time.Now().UTC()
	ref := dueRef("run-wall", now.Add(-time.Minute))
	ref.RetryState.Reason = store.RetryReasonUsageWindow
	ref.RetryState.Code = string(store.FailureUsageLimitBlocked)
	s.sweepDueRetries(context.Background(), &fakeRetryLister{refs: []mongostore.RetryDueRef{ref}}, resumer, now)

	if _, ok := st.rearmed["run-wall"]; !ok {
		t.Fatal("the slot wait was not deferred: DelayRunRetry never ran")
	}
	if got := st.delayedAs["run-wall"]; got != store.RetryReasonCredSlot {
		t.Fatalf("the deferral tagged the row %q, want %q — a usage-window row refused on the cap waits for a slot now", got, store.RetryReasonCredSlot)
	}
}

// The wait is bounded by the run's own max_wait, measured from the park's
// paid arming — the whole park, a provider wall the row waited on first
// included: a key busy for days must not hold a parked run forever. Past it
// the run is abandoned naming the bound and the instant it is measured from.
func TestSweepDueRetries_ACredentialSlotWaitPastMaxWaitIsAbandoned(t *testing.T) {
	st := newFakeRetryStore()
	st.claimWins["run-slot"] = true
	now := time.Now().UTC()
	scheduled := now.Add(-2 * time.Hour)
	at := now.Add(-time.Minute)
	st.loadRun["run-slot"] = &store.Run{
		ID: "run-slot", TenantID: "team-1", Status: store.RunStatusFailedResumable,
		RetryPolicy: &store.RunRetryPolicy{MaxWait: "1h"},
		RetryState:  &store.RunRetryState{RetryAfter: &at, ScheduledAt: &scheduled, Reason: store.RetryReasonCredSlot},
	}
	resumer := &fakeResumer{failing: fmt.Errorf("cloudpublisher: resume of run run-slot refused before publishing, the run put back as it was: %w", cloudpublisher.ErrCredentialAtCapacity)}
	s := newRetrySweeperServer(t, st, resumer)

	ref := dueRef("run-slot", at)
	ref.RetryState.Reason = store.RetryReasonCredSlot
	ref.RetryState.ScheduledAt = &scheduled
	s.sweepDueRetries(context.Background(), &fakeRetryLister{refs: []mongostore.RetryDueRef{ref}}, resumer, now)

	reason, ok := st.abandoned["run-slot"]
	if !ok || !strings.Contains(reason, "max_wait") || !strings.Contains(reason, scheduled.Format(time.RFC3339)) {
		t.Fatalf("a slot wait past max_wait was not abandoned naming the bound and the arming it is measured from: %q (%v)", reason, ok)
	}
	if _, ok := st.rearmed["run-slot"]; ok {
		t.Fatalf("a slot wait past max_wait was deferred again: %v", st.rearmed["run-slot"])
	}
}

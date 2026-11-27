package store

import (
	"strings"
	"testing"
	"time"
)

// The one binding of a frozen program to its run, one row per arm: a record
// re-admits only a run that started (a checkpoint, or a recorded attempt —
// either alone) and only while it names the run's attempt marker and its
// workflow hash. Red when the started predicate reads one arm only, or the
// binding drops its queued_at or its hash comparison.
func TestReadmitFrozen(t *testing.T) {
	q1 := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	q2 := q1.Add(time.Minute)
	record := func() *FrozenProgram {
		return &FrozenProgram{Key: "ir/r-bundle-program-d.json", Digest: "d", WorkflowHash: "h1", QueuedAt: q1, Source: FrozenProgramSourceCapacity}
	}
	at := func(t time.Time) *time.Time { return &t }
	for _, tc := range []struct {
		name string
		run  *Run
		want FrozenVerdict
	}{
		{"absent", &Run{Checkpoint: &Checkpoint{NodeID: "n"}, QueuedAt: at(q1), WorkflowHash: "h1"}, FrozenAbsent},
		{"neither checkpoint nor attempt", &Run{FrozenProgram: record(), QueuedAt: at(q1), WorkflowHash: "h1"}, FrozenNotStarted},
		{"checkpoint only", &Run{FrozenProgram: record(), Checkpoint: &Checkpoint{NodeID: "n"}, QueuedAt: at(q1), WorkflowHash: "h1"}, FrozenBound},
		{"attempts only", &Run{FrozenProgram: record(), Attempts: []RunAttempt{{ID: "a1"}}, QueuedAt: at(q1), WorkflowHash: "h1"}, FrozenBound},
		{"queued_at moved", &Run{FrozenProgram: record(), Checkpoint: &Checkpoint{NodeID: "n"}, QueuedAt: at(q2), WorkflowHash: "h1"}, FrozenStale},
		{"hash moved", &Run{FrozenProgram: record(), Checkpoint: &Checkpoint{NodeID: "n"}, QueuedAt: at(q1), WorkflowHash: "h2"}, FrozenStale},
		{"nil queued_at", &Run{FrozenProgram: record(), Checkpoint: &Checkpoint{NodeID: "n"}, WorkflowHash: "h1"}, FrozenStale},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fp, got := tc.run.ReadmitFrozen()
			if got != tc.want {
				t.Fatalf("ReadmitFrozen = %s, want %s", got, tc.want)
			}
			if (fp == nil) != (tc.run.FrozenProgram == nil) {
				t.Fatalf("record returned %v, want the run's (%v)", fp, tc.run.FrozenProgram)
			}
		})
	}
	var none *Run
	if _, v := none.ReadmitFrozen(); v != FrozenAbsent {
		t.Fatalf("nil run: %s, want absent", v)
	}
}

// A park carries a frozen program only from failed_resumable — a run parked
// from queued never started — and only a complete record. Red when Validate
// accepts a record on a queued park or an incomplete one.
func TestCapacityParkValidateFrozen(t *testing.T) {
	base := CapacityPark{Code: FailureCredSlotWait, Reason: RetryReasonCredSlot, RetryAfter: time.Now()}
	full := &FrozenProgram{Key: "k", Digest: "d", WorkflowHash: "h", Source: FrozenProgramSourceCapacity}

	queued := base
	queued.From, queued.ExactQueuedAt, queued.Frozen = RunStatusQueued, time.Now(), full
	if err := queued.Validate(); err == nil || !strings.Contains(err.Error(), "frozen program") {
		t.Fatalf("a queued park with a frozen program: %v, want refused", err)
	}
	partial := base
	partial.From, partial.TransitUntil, partial.Frozen = RunStatusFailedResumable, time.Now(), &FrozenProgram{Key: "k", Digest: "d", Source: FrozenProgramSourceCapacity}
	if err := partial.Validate(); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("an incomplete frozen program: %v, want refused", err)
	}
	ok := base
	ok.From, ok.TransitUntil, ok.Frozen = RunStatusFailedResumable, time.Now(), full
	if err := ok.Validate(); err != nil {
		t.Fatalf("a transit park with a complete frozen program: %v", err)
	}
}

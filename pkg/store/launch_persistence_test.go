package store

import (
	"context"
	"errors"
	"testing"
)

// replayingGuard is the guard shape PersistLaunchRun exists for: a scheduler's
// admission runs the write in a Mongo transaction, and mongo.Session's
// WithTransaction replays that transaction's callback on a
// TransientTransactionError — the aborted attempt's writes are rolled back,
// its error is not reported, and the callback runs again. attempts counts how
// many times the write ran.
func replayingGuard(ctx context.Context, replays int, attempts *int) context.Context {
	return WithLaunchPersistenceGuard(ctx, func(ctx context.Context, write func(context.Context) error) error {
		for i := 0; i < replays; i++ {
			*attempts++
			if err := write(ctx); err != nil {
				return err
			}
		}
		*attempts++
		return write(ctx)
	})
}

// handedState is the run state one save attempt was given — the fields a run
// saver assigns on the value it is handed, so a replay that inherited a
// previous attempt's mutations is visible here rather than only in the
// store's refusal.
type handedState struct {
	casVersion  int64
	failureCode FailureCode
	endReason   RunEndReason
}

// advancingSaver stands in for a run saver's effect on the value it is handed.
// Every mutation here is one both real savers make in place:
// mongo.Store.SaveRun advances r.CASVersion on a successful write and clears
// FailureCode/EndReason on a status that does not carry them
// (pkg/store/mongo/runs.go), and FilesystemRunStore.writeRun advances
// r.CASVersion the same way (pkg/store/store_run.go). Only the CAS advance is
// a read-modify-write, which is what makes the save non-idempotent on its own
// input.
func advancingSaver(handed *[]handedState) func(context.Context, *Run) error {
	return func(_ context.Context, r *Run) error {
		*handed = append(*handed, handedState{casVersion: r.CASVersion, failureCode: r.FailureCode, endReason: r.EndReason})
		r.FailureCode = ""
		r.EndReason = ""
		r.CASVersion++
		return nil
	}
}

// A replayed launch write starts from the run as it stood before the first
// attempt.
//
// A launch persisted under a scheduler's admission is saved inside a
// transaction the driver may replay. SaveRun advances the run's CAS version in
// place, so a callback that handed the same value to both attempts offered
// version 1 on the replay while the rolled-back insert had left no document at
// all: the save switches its upsert off, matches nothing, and refuses the
// launch with ErrRunConflict — a launch reported as a conflict precisely
// because the retry that should have rescued it ran.
func TestPersistLaunchRunHandsEveryAttemptTheInitialRun(t *testing.T) {
	var handed []handedState
	attempts := 0
	ctx := replayingGuard(context.Background(), 1, &attempts)
	r := &Run{ID: "run-1", Status: RunStatusQueued, FailureCode: FailureCode("stale-code"), EndReason: RunEndReason("stale-reason")}

	if err := PersistLaunchRun(ctx, r, advancingSaver(&handed)); err != nil {
		t.Fatalf("PersistLaunchRun: %v", err)
	}
	if attempts != 2 || len(handed) != 2 {
		t.Fatalf("attempts=%d handed=%+v, want the guard to have replayed the write once", attempts, handed)
	}
	want := handedState{casVersion: 0, failureCode: FailureCode("stale-code"), endReason: RunEndReason("stale-reason")}
	if handed[0] != want {
		t.Errorf("first attempt was handed %+v, wanted the run as built: %+v", handed[0], want)
	}
	if handed[1] != want {
		t.Errorf("the replayed attempt was handed %+v, want the SAME initial state %+v: a save that advanced the CAS version on the aborted attempt makes the replay write with its upsert off, against a document the rollback removed", handed[1], want)
	}
	// The committed attempt's post-save state is what the launch continues
	// with: a run left on version 0 would make the next writer collide.
	if r.CASVersion != 1 {
		t.Errorf("run CAS version after the launch = %d, want 1: the committed attempt's advance is published back", r.CASVersion)
	}
	if r.FailureCode != "" || r.EndReason != "" {
		t.Errorf("run carries failure_code=%q end_reason=%q, want the committed attempt's cleared values", r.FailureCode, r.EndReason)
	}
}

// Without a guard the run is saved once and still carries the save's effect:
// the ordinary launch path keeps its behaviour.
func TestPersistLaunchRunWithoutAGuardSavesOnce(t *testing.T) {
	var handed []handedState
	r := &Run{ID: "run-2", Status: RunStatusQueued}
	if err := PersistLaunchRun(context.Background(), r, advancingSaver(&handed)); err != nil {
		t.Fatalf("PersistLaunchRun: %v", err)
	}
	if len(handed) != 1 || handed[0].casVersion != 0 {
		t.Fatalf("handed=%+v, want exactly one attempt handed version 0", handed)
	}
	if r.CASVersion != 1 {
		t.Errorf("run CAS version = %d, want 1", r.CASVersion)
	}
}

// A refused launch leaves the run value as it was: the caller's rollback and
// its diagnostics read the state they built, never a failed attempt's.
func TestPersistLaunchRunLeavesTheRunUntouchedWhenRefused(t *testing.T) {
	refused := errors.New("admission lost")
	ctx := WithLaunchPersistenceGuard(context.Background(), func(ctx context.Context, write func(context.Context) error) error {
		if err := write(ctx); err != nil {
			return err
		}
		return refused
	})
	var handed []handedState
	r := &Run{ID: "run-3", Status: RunStatusQueued, FailureCode: FailureCode("stale-code")}
	err := PersistLaunchRun(ctx, r, advancingSaver(&handed))
	if !errors.Is(err, refused) {
		t.Fatalf("PersistLaunchRun = %v, want the guard's refusal", err)
	}
	if r.CASVersion != 0 || r.FailureCode != FailureCode("stale-code") {
		t.Errorf("refused launch left the run at version %d failure_code=%q, want its initial 0/%q", r.CASVersion, r.FailureCode, "stale-code")
	}
}

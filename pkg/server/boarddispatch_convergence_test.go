package server

import (
	"context"
	"fmt"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The FRESH re-check bails when the pointer turned finished mid-pass
// ("left for the next pass"). Prove that claim: the NEXT pass must file
// the card done, or the bail is a permanent strand and not a deferral.
func TestBoardDispatcher_FreshFinishedPointerConvergesOnTheNextPass(t *testing.T) {
	f := newFakeBoardCoord(readyCard("native:1", "feature-dev"))
	d := newBoardDispatcher(f, func(context.Context, string, native.Issue) error {
		return fmt.Errorf("run r1 ended: %w", errCardContinuable)
	}, "replica-A", 4, nil)
	stale := store.RunStatusFailedResumable
	d.statusFor = func(_ context.Context, _, _ string) (store.RunStatus, error) { return stale, nil }
	d.runFor = func(_ context.Context, _, id string) (*store.Run, error) {
		return &store.Run{ID: id, Status: store.RunStatusFinished}, nil
	}
	d.issueRuns = func(context.Context, string, string) ([]*store.Run, error) { return nil, nil }
	d.adoptRun = func(string, string, string, string) error { return nil }
	d.tick(context.Background())
	d.wg.Wait()
	f.mu.Lock()
	f.cands[0].Issue.LastRunID = "run-x"
	f.cands[0].Issue.State = d.inProgressState
	f.mu.Unlock()

	d.sweepForkAdoptions(context.Background())
	if got := f.states["native:1"]; got != d.inProgressState {
		t.Fatalf("pass 1: card is %q, want in_progress (the fresh finished pointer is deferred)", got)
	}
	// Next pass: the memo has expired and statusFor has caught up.
	d.reconcileMemoMu.Lock()
	d.reconcileMemo = nil
	d.reconcileMemoMu.Unlock()
	stale = store.RunStatusFinished
	f.mu.Lock()
	f.cands[0].Issue.State = d.inProgressState
	f.mu.Unlock()
	d.sweepForkAdoptions(context.Background())
	if got := f.states["native:1"]; got != d.doneState {
		t.Fatalf("pass 2: card is %q, want %q — the deferral never converges", got, d.doneState)
	}
}

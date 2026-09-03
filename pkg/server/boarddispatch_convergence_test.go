package server

import (
	"context"
	"fmt"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher"
	"github.com/SocialGouv/iterion/pkg/dispatcher/boardmongo"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
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

// The COMPOSITION the widened un-leased guard is justified by: a card
// it releases is a card the fork-adoption reconciler then FILES — the
// brick test (release happens) certified only the consumer's half. Gate
// OFF, one pass of each sweep: released AND filed, per disposition.
func TestCloudSweep_ReleasedUnleasedCardIsFiledByTheReconciler(t *testing.T) {
	t.Setenv(dispatcher.ClaimReaperEnvName(), "off")
	for _, tc := range []struct {
		name   string
		status store.RunStatus
		want   string
	}{
		{"finished is filed done", store.RunStatusFinished, native.StateDone},
		{"terminal failure is filed blocked", store.RunStatusFailed, native.StateBlocked},
		{"settled resumable is filed blocked", store.RunStatusFailedResumable, native.StateBlocked},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeBoardCoord()
			f.claimed["c-1"] = "podA-1"
			f.epochs["c-1"] = 0
			f.states["c-1"] = native.StateInProgress
			f.unleased = append(f.unleased, boardmongo.ExpiredCandidate{
				Tenant: "t1",
				Claim: tracker.ExpiredClaim{IssueID: "c-1", State: native.StateInProgress, LastRunID: "run-1",
					Prev: tracker.ClaimToken{Marker: "podA-1", Epoch: 0}},
			})
			d := newBoardDispatcher(f, nil, "replica-A", 1, iterlog.Nop())
			d.statusFor = func(_ context.Context, _, _ string) (store.RunStatus, error) {
				return tc.status, nil
			}
			d.runFor = func(_ context.Context, _, id string) (*store.Run, error) {
				return &store.Run{ID: id, Status: tc.status}, nil
			}
			d.issueRuns = func(context.Context, string, string) ([]*store.Run, error) { return nil, nil }
			d.adoptRun = func(string, string, string, string) error { return nil }

			runOnePass(t, d, func() bool {
				f.mu.Lock()
				defer f.mu.Unlock()
				_, held := f.claimed["c-1"]
				return !held
			})
			if _, held := f.claimed["c-1"]; held {
				t.Fatalf("precondition: the un-leased sweep must release, still held by %q", f.claimed["c-1"])
			}
			// The reconciler reads the CANDIDATE list: fold the released
			// card onto it as an eligible-listing candidate.
			f.mu.Lock()
			f.cands = []boardmongo.Candidate{{Tenant: "t1", Issue: native.Issue{
				ID: "c-1", State: native.StateInProgress, LastRunID: "run-1"}}}
			f.mu.Unlock()

			d.sweepForkAdoptions(context.Background())

			if got := f.states["c-1"]; got != tc.want {
				t.Fatalf("released card ended %q, want %q — the release is only justified by the filing that follows", got, tc.want)
			}
		})
	}
}

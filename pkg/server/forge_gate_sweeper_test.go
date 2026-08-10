package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/forge"
	mongostore "github.com/SocialGouv/iterion/pkg/store/mongo"
)

type fakeGateSweepLister struct {
	refs  []mongostore.NotifiableRunRef
	err   error
	calls int
	since time.Time
	// before is the upper bound the sweep asked for — the grace that keeps it
	// off runs the event path may still be handling.
	before time.Time
	limit  int
}

func (f *fakeGateSweepLister) ListNotifiableRuns(_ context.Context, since, before time.Time, limit int) ([]mongostore.NotifiableRunRef, error) {
	f.calls++
	f.since, f.before, f.limit = since, before, limit
	return f.refs, f.err
}

// The reason the sweep exists: the reconciler's only trigger is one event on a
// bus that is lossy by design. When that event is dropped — a replica taking
// the queue-group delivery mid-shutdown, a handler that never ran — the
// required check stays absent and the pull request waits forever, with nothing
// anywhere saying so. Offering the same run again from a poll is what makes
// the repair survive a lost event.
func TestGateSweep_ReconcilesARunWhoseEventWasLost(t *testing.T) {
	gc := &listingGateClient{fakeGateClient: fakeGateClient{headSHA: "deadbeef"}}
	s, runID := gateReconcileFixture(t, gatingInputs(), gc)
	lister := &fakeGateSweepLister{refs: []mongostore.NotifiableRunRef{{ID: runID}}}

	// No event was ever delivered for this run.
	s.sweepGates(context.Background(), lister, time.Now().UTC())

	if gc.setCalls != 1 {
		t.Fatalf("posted %d statuses, want 1 — a dropped outcome event leaves the PR blocked forever", gc.setCalls)
	}
	if gc.last.State != forge.CommitStateFailure {
		t.Errorf("state = %q, want failure — the review did not happen", gc.last.State)
	}
}

// The two triggers race on every run that ends. The repair re-reads the live
// status before writing, so the loser of the race must find a verdict and
// stand down rather than post a second one.
func TestGateSweep_IsIdempotentWithTheEventPath(t *testing.T) {
	gc := &listingGateClient{fakeGateClient: fakeGateClient{headSHA: "deadbeef"}}
	s, runID := gateReconcileFixture(t, gatingInputs(), gc)

	if err := s.reconcileGateForRun(context.Background(), terminalEvent(runID)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	// The event path posted a synthetic failure; feed the status back so the
	// sweep sees the same head state the forge would now report.
	gc.statuses = []forge.CommitStatus{gc.last}
	before := gc.setCalls

	s.sweepGates(context.Background(), &fakeGateSweepLister{refs: []mongostore.NotifiableRunRef{{ID: runID}}}, time.Now().UTC())

	if gc.setCalls != before {
		t.Fatalf("posted %d more statuses, want 0 — the sweep must not double-post behind the event path", gc.setCalls-before)
	}
}

// Both bounds carry a decision. The grace keeps the sweep off runs the event
// path is still working through (a repair does several forge round-trips), so
// the two paths race only on the dropped ones. The lookback is what stops the
// sweep reaching back into history and painting a synthetic failure onto pull
// requests that merged days ago.
func TestGateSweep_WindowIsBoundedOnBothSides(t *testing.T) {
	s, _ := gateReconcileFixture(t, gatingInputs(), &listingGateClient{})
	lister := &fakeGateSweepLister{}
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)

	s.sweepGates(context.Background(), lister, now)

	if lister.calls != 1 {
		t.Fatalf("scanned %d times, want 1", lister.calls)
	}
	if got, want := lister.before, now.Add(-gateSweepGrace); !got.Equal(want) {
		t.Errorf("upper bound = %s, want %s — without the grace the sweep races the event path on every run", got, want)
	}
	if got, want := lister.since, now.Add(-gateSweepLookback); !got.Equal(want) {
		t.Errorf("lower bound = %s, want %s — an unbounded reach posts failures onto long-merged PRs", got, want)
	}
	if lister.since.After(lister.before) {
		t.Error("empty window: the lookback must exceed the grace or nothing is ever examined")
	}
	// A pass must also outreach its own cadence, or a run that ends in the gap
	// between two windows is never examined by either.
	if gateSweepLookback <= gateSweepInterval+gateSweepGrace {
		t.Errorf("lookback %s <= interval %s + grace %s — runs fall through the gap between passes",
			gateSweepLookback, gateSweepInterval, gateSweepGrace)
	}
}

// The discrimination the repeated offer forced, in both directions. A
// synthetic failure is not a verdict, so it does not stand the repair down in
// general — that is what lets a SECOND death on the same head (a relaunched
// run dying too) escalate instead of mistaking the first death's marker for an
// answer. But the same run offered again is already answered. The target URL
// names the run the failure speaks for, which separates the two without
// bookkeeping a second replica would not share.
func TestGateReconcile_SyntheticFailureStandsDownOnlyForItsOwnRun(t *testing.T) {
	interruption := func(targetURL string) forge.CommitStatus {
		return forge.CommitStatus{
			Context:     "iterion/review",
			State:       forge.CommitStateFailure,
			Description: gateInterruptedDescription,
			TargetURL:   targetURL,
		}
	}
	t.Run("its own — already answered, stand down", func(t *testing.T) {
		gc := &listingGateClient{
			fakeGateClient: fakeGateClient{headSHA: "deadbeef"},
			statuses:       []forge.CommitStatus{interruption("https://iterion.test/runs/run-gating")},
		}
		s, runID := gateReconcileFixture(t, gatingInputs(), gc)
		if err := s.reconcileGateForRun(context.Background(), terminalEvent(runID)); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if gc.setCalls != 0 {
			t.Fatalf("posted %d statuses, want 0 — every sweep pass would re-post and re-enter the recovery", gc.setCalls)
		}
	})
	t.Run("another run's — the second death must still escalate", func(t *testing.T) {
		gc := &listingGateClient{
			fakeGateClient: fakeGateClient{headSHA: "deadbeef"},
			statuses:       []forge.CommitStatus{interruption("https://iterion.test/runs/some-earlier-run")},
		}
		s, runID := gateReconcileFixture(t, gatingInputs(), gc)
		if err := s.reconcileGateForRun(context.Background(), terminalEvent(runID)); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if gc.setCalls != 1 {
			t.Fatalf("posted %d statuses, want 1 — the repair goes silent exactly where the recovery runs out", gc.setCalls)
		}
	})
}

// A scan that fails must not take the ticker down with it: the next pass is
// the recovery, and a sweeper that exits on one Mongo hiccup silently stops
// being a net at all.
func TestGateSweep_SurvivesAScanError(t *testing.T) {
	s, _ := gateReconcileFixture(t, gatingInputs(), &listingGateClient{})
	s.sweepGates(context.Background(), &fakeGateSweepLister{err: errors.New("mongo down")}, time.Now().UTC())
}

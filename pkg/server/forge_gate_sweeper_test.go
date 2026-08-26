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

// The discrimination the repeated offer forced. A synthetic failure is not a
// verdict, so it does not stand the REPAIR down in general — a SECOND death on
// the same head (a relaunched run dying too) still walks the relaunch tail and
// escalates (pinned end-to-end in TestGateRelaunch) instead of mistaking the
// first death's marker for an answer. But the STATUS WRITE stands down either
// way: one marker per head is enough, and re-posting from a run the marker
// does not speak for is the storm — two dead runs re-pointing the target URL
// at themselves every sweep tick, 116 writes on one head in 15 minutes
// (buildkit-operator#21, 2026-08-17). The target URL names the run the
// failure speaks for, which separates "mine" from "another's" without
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
	t.Run("another run's — no re-post, the storm lives there", func(t *testing.T) {
		gc := &listingGateClient{
			fakeGateClient: fakeGateClient{headSHA: "deadbeef"},
			statuses:       []forge.CommitStatus{interruption("https://iterion.test/runs/some-earlier-run")},
		}
		s, runID := gateReconcileFixture(t, gatingInputs(), gc)
		if err := s.reconcileGateForRun(context.Background(), terminalEvent(runID)); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if gc.setCalls != 0 {
			t.Fatalf("posted %d statuses, want 0 — re-posting over another run's marker is the status storm", gc.setCalls)
		}
	})
}

// pagingLister serves a backlog one page at a time, honouring the `before`
// cursor exactly as the Mongo query does (newest-first, strictly older than
// the cursor).
type pagingLister struct {
	all      []mongostore.NotifiableRunRef // newest first
	requests []time.Time                   // the `before` of each call
}

func (p *pagingLister) ListNotifiableRuns(_ context.Context, _, before time.Time, limit int) ([]mongostore.NotifiableRunRef, error) {
	p.requests = append(p.requests, before)
	out := []mongostore.NotifiableRunRef{}
	for _, r := range p.all {
		if r.UpdatedAt.Before(before) && len(out) < limit {
			out = append(out, r)
		}
	}
	return out, nil
}

// A single page is not a bound, it is a silent truncation that REPEATS. Rows
// come back newest-first, so a dead gating run pushed off the page is pushed
// off again on every subsequent pass: its check is never repaired and the
// batch-full warning fires forever with no recovery. The window has to be
// paged with the cursor the query was built for.
func TestGateSweep_PagesTheWindowInsteadOfStarvingOldCandidates(t *testing.T) {
	gc := &listingGateClient{fakeGateClient: fakeGateClient{headSHA: "deadbeef"}}
	s, runID := gateReconcileFixture(t, gatingInputs(), gc)

	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	// A full first page of runs that gate nothing, then the real candidate
	// sitting just behind it — exactly the row a single page would drop.
	lister := &pagingLister{}
	for i := 0; i < gateSweepBatch; i++ {
		lister.all = append(lister.all, mongostore.NotifiableRunRef{
			ID:        "filler",
			UpdatedAt: now.Add(-gateSweepGrace - time.Duration(i+1)*time.Second),
		})
	}
	lister.all = append(lister.all, mongostore.NotifiableRunRef{
		ID:        runID,
		UpdatedAt: now.Add(-gateSweepGrace - time.Duration(gateSweepBatch+1)*time.Second),
	})

	s.sweepGates(context.Background(), lister, now)

	if len(lister.requests) < 2 {
		t.Fatalf("scanned %d page(s) — a full page must advance the cursor, or the oldest candidate is never examined", len(lister.requests))
	}
	if gc.setCalls != 1 {
		t.Fatalf("posted %d statuses, want 1 — the candidate behind the first page was starved", gc.setCalls)
	}
}

// A page that cannot advance the cursor (every row sharing one timestamp) must
// stop and say so, not re-scan the same rows up to the page cap.
func TestGateSweep_StopsWhenTheCursorCannotAdvance(t *testing.T) {
	s, _ := gateReconcileFixture(t, gatingInputs(), &listingGateClient{})
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	stuck := &fakeGateSweepLister{}
	for i := 0; i < gateSweepBatch; i++ {
		// Zero UpdatedAt: nothing to advance the cursor with.
		stuck.refs = append(stuck.refs, mongostore.NotifiableRunRef{ID: "no-timestamp"})
	}

	s.sweepGates(context.Background(), stuck, now)

	if stuck.calls != 1 {
		t.Fatalf("scanned %d times, want 1 — a stalled cursor re-scanned the same rows", stuck.calls)
	}
}

// A scan that fails must not take the ticker down with it: the next pass is
// the recovery, and a sweeper that exits on one Mongo hiccup silently stops
// being a net at all.
func TestGateSweep_SurvivesAScanError(t *testing.T) {
	s, _ := gateReconcileFixture(t, gatingInputs(), &listingGateClient{})
	s.sweepGates(context.Background(), &fakeGateSweepLister{err: errors.New("mongo down")}, time.Now().UTC())
}

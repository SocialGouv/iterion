package server

import (
	"context"
	"testing"
	"time"

	mongostore "github.com/SocialGouv/iterion/pkg/store/mongo"
)

// backlogLister serves a backlog the way the Mongo query does: newest-first,
// bounded below by `since` and strictly above by `before`. Unlike pagingLister
// it honours BOTH bounds, which is what makes the horizon window meaningful
// here — the whole point is a backlog wider than one pass can walk.
type backlogLister struct {
	all   []mongostore.NotifiableRunRef // newest first
	calls int
}

func (b *backlogLister) ListNotifiableRuns(_ context.Context, since, before time.Time, limit int) ([]mongostore.NotifiableRunRef, error) {
	b.calls++
	out := []mongostore.NotifiableRunRef{}
	for _, r := range b.all {
		if len(out) >= limit {
			break
		}
		if r.UpdatedAt.Before(before) && !r.UpdatedAt.Before(since) {
			out = append(out, r)
		}
	}
	return out, nil
}

// A backlog of `fillers` rows that gate nothing, with the real gating
// candidate sitting at the very oldest end — the row a truncated pass drops.
func gateBacklog(runID string, now time.Time, fillers int) *backlogLister {
	l := &backlogLister{}
	for i := 0; i < fillers; i++ {
		l.all = append(l.all, mongostore.NotifiableRunRef{
			ID:        "filler",
			UpdatedAt: now.Add(-gateSweepGrace - time.Duration(i+1)*time.Minute),
		})
	}
	l.all = append(l.all, mongostore.NotifiableRunRef{
		ID:        runID,
		UpdatedAt: now.Add(-gateSweepGrace - time.Duration(fillers+1)*time.Minute),
	})
	return l
}

// The horizon is only as real as the number of rows a pass can walk to reach
// it. One pass is capped at gateSweepMaxPages × gateSweepBatch, rows arrive
// updated_at-descending, so a backlog past that cap is truncated at its OLDEST
// end — exactly the batch-death runs a multi-day horizon exists for. Restarting
// the cursor at the head every pass drops the SAME rows every pass: the reach
// is not slow, it is zero, and the deployment cannot tell because the pass
// warns "not examined this pass" as though a later one would get there.
//
// Both halves are asserted here, because only the contrast shows the fix is
// the resumption and not the window.
func TestTheDeepPassWalksABacklogWiderThanOnePass(t *testing.T) {
	gc := &listingGateClient{fakeGateClient: fakeGateClient{headSHA: "deadbeef"}}
	s, runID := gateReconcileFixture(t, gatingInputs(), gc)
	now := time.Date(2026, 9, 15, 6, 0, 0, 0, time.UTC)
	budget := gateSweepMaxPages * gateSweepBatch

	t.Run("restarting at the head never reaches it", func(t *testing.T) {
		lister := gateBacklog(runID, now, budget)
		for pass := 0; pass < 5; pass++ {
			s.sweepGates(context.Background(), lister, now, gateSweepHorizon, time.Time{})
		}
		if gc.setCalls != 0 {
			t.Fatalf("the candidate was reached without a resuming cursor (%d posts) — then this test is not exercising a backlog wider than one pass", gc.setCalls)
		}
	})

	t.Run("resuming reaches it", func(t *testing.T) {
		lister := gateBacklog(runID, now, budget)
		var resume time.Time
		reached := 0
		// ceil(P/budget) passes is the arithmetic the constant block states;
		// allow one more for the page that finds the window exhausted.
		for pass := 0; pass < 4; pass++ {
			resume = s.sweepGates(context.Background(), lister, now, gateSweepHorizon, resume)
			if gc.setCalls > 0 {
				reached = pass + 1
				break
			}
		}
		if reached == 0 {
			t.Fatalf("a gating run at the oldest end of a %d-row backlog was never offered to the reconciler — its required check stays pending forever and the horizon is inert", len(lister.all))
		}
		if reached > 2 {
			t.Errorf("it took %d deep passes to walk %d rows at %d per pass — the traversal is not advancing a full budget per pass", reached, len(lister.all), budget)
		}
	})
}

// A completed traversal must start over. The cursor is progress within ONE
// walk of the horizon, not a permanent watermark: runs keep dying, and a
// cursor that stuck at the oldest row would leave every newer candidate
// unreachable by the deep pass from then on.
func TestAFinishedDeepTraversalStartsOverAtTheHead(t *testing.T) {
	gc := &listingGateClient{fakeGateClient: fakeGateClient{headSHA: "deadbeef"}}
	s, runID := gateReconcileFixture(t, gatingInputs(), gc)
	now := time.Date(2026, 9, 15, 6, 0, 0, 0, time.UTC)

	lister := gateBacklog(runID, now, 3)
	if resume := s.sweepGates(context.Background(), lister, now, gateSweepHorizon, time.Time{}); !resume.IsZero() {
		t.Errorf("a pass that exhausted its window handed back the cursor %s — the next one would resume mid-window and never look at anything newer", resume)
	}
}

// A resume point that has aged out below the window is not a cursor any more:
// the rows it pointed into have left the horizon. Handing it back unchanged
// would make every remaining deep pass scan an empty range.
func TestADeepPassStartsOverWhenItsCursorAgedOutOfTheWindow(t *testing.T) {
	gc := &listingGateClient{fakeGateClient: fakeGateClient{headSHA: "deadbeef"}}
	s, runID := gateReconcileFixture(t, gatingInputs(), gc)
	now := time.Date(2026, 9, 15, 6, 0, 0, 0, time.UTC)

	lister := gateBacklog(runID, now, 3)
	stale := now.Add(-gateSweepHorizon).Add(-time.Hour)
	s.sweepGates(context.Background(), lister, now, gateSweepHorizon, stale)

	if gc.setCalls == 0 {
		t.Error("a stale cursor left the pass scanning below the window — nothing inside it was examined at all")
	}
}

// The fast pass has the opposite contract: answer a dropped outcome event
// within the minute. It must always start at the head, and it must never
// inherit the deep pass's position — a fast pass resuming mid-backlog would
// stop being the low-latency net it exists to be.
func TestTheFastPassAlwaysStartsAtTheHead(t *testing.T) {
	s, _ := gateReconcileFixture(t, gatingInputs(), &listingGateClient{})
	now := time.Date(2026, 9, 15, 6, 0, 0, 0, time.UTC)

	lister := &fakeGateSweepLister{}
	s.sweepGates(context.Background(), lister, now, gateSweepLookback, time.Time{})
	if got, want := lister.before, now.Add(-gateSweepGrace); !got.Equal(want) {
		t.Errorf("the fast pass started at %s, want the head %s", got, want)
	}
}

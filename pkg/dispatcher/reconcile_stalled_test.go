package dispatcher

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
)

// TestReconcileStalled_ForceReapsCtxIgnoringWorker covers the failure
// mode the dogfood-2026-05-20 run surfaced: a delegate that blocks on
// network I/O without honoring ctx pinned a dispatcher slot for 7+
// hours after stall fired the first ctx cancel. reconcileStalled MUST
// plant a tombstone + finishRun once the grace expires so the slot is
// released even when the worker swallows cancellation.
func TestReconcileStalled_ForceReapsCtxIgnoringWorker(t *testing.T) {
	t.Setenv("ITERION_DISPATCHER_STALL_REAP_GRACE", "100ms")

	ft := newFakeTracker()
	ft.add(tracker.Issue{
		ID: "fake:stall", Identifier: "fake#stall",
		Title: "hang", WorkflowState: "ready",
		Assignee: "feature_dev",
	})

	// The worker ignores ctx.Done() — the dispatcher must reap the
	// slot via the force-reap path rather than waiting for the worker
	// to exit on its own.
	// The start signal is a NON-BLOCKING send: under the tombstone-drop
	// mutation this very test re-dispatches the issue, and a second
	// worker blocking on a full channel would wedge c.Stop() in the
	// deferred cleanup instead of letting the claimCalls assertion fail
	// fast — a 180 s timeout red instead of a diagnostic.
	unblock := make(chan struct{})
	dispatchStarted := make(chan struct{}, 1)
	runner := &StubRunner{Handler: func(_ context.Context, _ DispatchSpec) error {
		select {
		case dispatchStarted <- struct{}{}:
		default:
		}
		<-unblock // never returns until the test releases it
		return nil
	}}

	c := newTestDispatcher(t, runner, ft, 50*time.Millisecond)
	// newTestDispatcher leaves Stall disabled (TimeoutMS=0). Reload
	// with a tight stall window so the force-reap path fires fast.
	stalledCfg := *c.cfg.Load()
	stalledCfg.Polling = PollingConfig{IntervalMS: 50}
	stalledCfg.Stall = StallConfig{TimeoutMS: 100}
	stalledCfg.applyDefaults()
	c.Reload(&stalledCfg)

	ctx, cancel := context.WithCancel(context.Background())
	// Cleanup order matters: unblock the runner BEFORE Stop() so the
	// dispatcher's WaitGroup can drain. Stop() blocks on c.workersWG.
	defer func() {
		close(unblock)
		c.Stop()
		cancel()
	}()
	c.Start(ctx)

	select {
	case <-dispatchStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("dispatch never started")
	}

	// Wait for the dispatch to be VISIBLE in a published snapshot before
	// polling for the reap: dispatchStarted fires from the WORKER
	// goroutine, which is unordered with the actor's fireSnapshot for the
	// dispatch — polling for Running==0 straight away can read the initial
	// EMPTY snapshot and misread it as "already reaped" (the 0.06s CI
	// flake), then find the entry in the follow-up check and report a
	// phantom missing tombstone.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if len(c.Snapshot().Running) == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(c.Snapshot().Running) != 1 {
		t.Fatalf("dispatched entry never appeared in a snapshot; snapshot=%+v", c.Snapshot())
	}

	// Expect: stall fires (>100ms after dispatch with no events),
	// grace expires (>100ms after first cancel), force-reap runs.
	// Total budget: ~500ms; allow 3s for CI slop.
	for time.Now().Before(deadline) {
		snap := c.Snapshot()
		if len(snap.Running) == 0 {
			// Slot reaped. Verify the tombstone was planted (issue
			// can't be re-dispatched until the worker actually exits).
			//
			// Mechanism observed — TWO advances of ft.listCalls under
			// serial Refreshes, then a check that tracker.Claim was
			// NOT called for the tombstoned issue during that scan.
			//
			// Per ADR-028, tick() only LAUNCHES an off-actor discovery
			// goroutine (launchDiscovery) and returns; the candidate
			// scan itself runs in cmdCandidates.apply, which fires on
			// the actor AFTER discovery's ListCandidates returns AND
			// posts back. One listCalls advance therefore only proves
			// the tracker was called; it does NOT prove
			// cmdCandidates.apply ran. A second launchDiscovery cannot
			// fire until the first cmdCandidates.apply cleared
			// state.discoveryInFlight, so a second listCalls advance
			// guarantees the first apply completed. The positive
			// mechanism ASSERTED is then that tracker.Claim was not
			// called again for the tombstoned issue during that scan —
			// dropping the tombstone in production would send the
			// dispatch path through Claim (fakeTracker.Claim
			// increments claimCallsPerID), reddening the count check.
			// The verification loop uses its OWN budget rather than
			// the outer `deadline`: on a contended runner the reap can
			// land with < 100 ms of the outer budget left, which would
			// let the inner loop exit at once and misfire "actor is
			// wedged". #1471.
			//
			// PRECONDITION observed first: the revert+Release that return
			// fake:stall to the candidate set run on the off-actor finish
			// worker AFTER finishRun published the freed-slot snapshot
			// this loop polled — Running==0 does not yet mean the issue
			// is claimable. ListCandidates skips claimed issues, so a
			// scan run before the Release cannot contain fake:stall, and
			// the tombstone assertion over such a scan is vacuously
			// green. Wait until the Release is observed on the tracker,
			// THEN run the scan witness over a set that provably
			// contains the issue.
			innerDeadline := time.Now().Add(10 * time.Second)
			for ft.isClaimedOnTracker("fake:stall") && time.Now().Before(innerDeadline) {
				time.Sleep(5 * time.Millisecond)
			}
			if ft.isClaimedOnTracker("fake:stall") {
				t.Fatal("the tracker still holds the claim on fake:stall after the reap — the finish worker never released; the tombstone precondition cannot be observed")
			}
			claimsBefore := ft.claimCallsFor("fake:stall")
			listBefore := ft.listCalls.Load()
			c.Refresh()
			for ft.listCalls.Load() < listBefore+2 && time.Now().Before(innerDeadline) {
				// Nudge: a mid-flight discovery makes an early Refresh
				// short-circuit through the discoveryInFlight guard, so
				// we re-Refresh until the second round-trip lands. Each
				// Refresh is idempotent — a full-queue send drops via
				// the select default in dispatcher.Refresh.
				c.Refresh()
				time.Sleep(5 * time.Millisecond)
			}
			if got := ft.listCalls.Load(); got < listBefore+2 {
				t.Fatalf("only %d/2 discovery round-trips landed within the verification deadline — actor is wedged, cannot decide the tombstone assertion", got-listBefore)
			}
			if got := ft.claimCallsFor("fake:stall"); got > claimsBefore {
				t.Fatalf("tombstone missing — tracker.Claim(fake:stall) was called %d additional time(s) after the reap (was %d, now %d)", got-claimsBefore, claimsBefore, got)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("reconcileStalled did not force-reap the stalled slot; snapshot=%+v", c.Snapshot())
}

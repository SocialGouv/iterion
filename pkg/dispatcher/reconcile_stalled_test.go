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
	unblock := make(chan struct{})
	dispatchStarted := make(chan struct{}, 1)
	runner := &StubRunner{Handler: func(_ context.Context, _ DispatchSpec) error {
		dispatchStarted <- struct{}{}
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
	case <-time.After(2 * time.Second):
		t.Fatal("dispatch never started")
	}

	// Expect: stall fires (>100ms after dispatch with no events),
	// grace expires (>100ms after first cancel), force-reap runs.
	// Total budget: ~500ms; allow 3s for CI slop.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		snap := c.Snapshot()
		if len(snap.Running) == 0 {
			// Slot reaped. Verify the tombstone was planted (issue
			// can't be re-dispatched until worker actually exits).
			c.Refresh()
			time.Sleep(60 * time.Millisecond)
			again := c.Snapshot()
			if len(again.Running) != 0 {
				t.Fatalf("tombstone missing — issue re-dispatched while worker still alive: %+v", again)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("reconcileStalled did not force-reap the stalled slot; snapshot=%+v", c.Snapshot())
}

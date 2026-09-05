package dispatcher

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// blockingReaperTracker is a lease-capable tracker whose expired-claim
// listing parks until released, recording the context the pass runs on.
type blockingReaperTracker struct {
	*native.Adapter
	entered chan context.Context
	release chan struct{}
	once    sync.Once
}

func (b *blockingReaperTracker) ListExpiredClaimCandidates(ctx context.Context, _ time.Time, _ int) ([]tracker.ExpiredClaim, error) {
	b.once.Do(func() { b.entered <- ctx })
	<-b.release
	return nil, nil
}

// TestClaimReaper_StopInterruptsAndDrainsThePass: Stop() promises that
// nothing of this dispatcher keeps writing once it returns — Manager.Stop
// tears the EngineRunner down right after, and the studio rebuilds the
// dispatcher on a project switch. The reaper pass ran on
// context.Background(), outside every WaitGroup Stop waits on, so a pass
// mid-flight kept transferring claims, filing cards and writing run
// statuses under a config the operator had already replaced. The pass
// must be interruptible (its context dies with c.stop) and awaited.
func TestClaimReaper_StopInterruptsAndDrainsThePass(t *testing.T) {
	board, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	adapter := native.NewAdapter(board)
	trk := &blockingReaperTracker{Adapter: adapter, entered: make(chan context.Context, 1), release: make(chan struct{})}
	c := &Dispatcher{
		tracker: trk, leaser: adapter, logger: iterlog.Nop(), hostMarker: "host-1",
		state: newState(), stop: make(chan struct{}), done: make(chan struct{}),
	}
	c.cfg.Store(&Config{Agent: AgentConfig{RunningState: native.StateInProgress}})
	// No actor runs in this harness: Stop's <-c.done wait is satisfied up
	// front, so what it measures below is ONLY the reaper's drain.
	close(c.done)

	t.Setenv(claimReaperEnv, "on")
	prevTick := claimReaperTick
	claimReaperTick = time.Millisecond
	t.Cleanup(func() { claimReaperTick = prevTick })

	c.startClaimReaper()

	var passCtx context.Context
	select {
	case passCtx = <-trk.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the reaper never ticked")
	}

	stopped := make(chan struct{})
	go func() {
		c.Stop()
		close(stopped)
	}()

	// 1. Interruptible: the pass's context dies with the stop.
	select {
	case <-passCtx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("REPRODUCED: Stop() did not cancel the reaper pass — it runs on context.Background(), so a pass " +
			"mid-flight keeps transferring claims and filing cards after the dispatcher is declared stopped")
	}
	// 2. Awaited: Stop() must not return while the pass is still running.
	select {
	case <-stopped:
		t.Fatal("REPRODUCED: Stop() returned while a reaper pass was still in flight — the pass is not tracked by " +
			"any WaitGroup Stop waits on")
	case <-time.After(100 * time.Millisecond):
	}
	close(trk.release)
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop() did not return once the pass ended")
	}
}

package runner

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
)

// countingLease is a run lock whose refreshes are counted, and fail with
// fail when it is set.
type countingLease struct {
	mu        sync.Mutex
	refreshes int
	fail      error
}

func (l *countingLease) Refresh(context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.refreshes++
	return l.fail
}

func (l *countingLease) Unlock() error { return nil }

func (l *countingLease) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.refreshes
}

type nopProgress struct{}

func (nopProgress) InProgress() error { return nil }

// TestLeaseHeartbeat_holdsTheLeaseThroughACancelledRunsTeardown: a cancelled
// run keeps refreshing its lease while its engine unwinds — the workspace
// export and the scratch bank, minutes long — so a sibling that received the
// redelivery stays on the lock; the refreshes stop once the engine returned.
func TestLeaseHeartbeat_holdsTheLeaseThroughACancelledRunsTeardown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		lease := &countingLease{}
		r := &Runner{cfg: Config{HeartbeatInterval: 20 * time.Second, Logger: iterlog.Nop()}}
		runCtx, runCancel := context.WithCancelCause(context.Background())
		defer runCancel(nil)
		stop := r.startLeaseHeartbeat(runCtx, runCancel, "run-1", lease, nopProgress{})
		time.Sleep(30 * time.Second)
		runCancel(runtime.ErrRunInterrupted)
		atCancel := lease.count()
		time.Sleep(5 * time.Minute)
		synctest.Wait()
		during := lease.count()
		stop()
		time.Sleep(time.Minute)
		synctest.Wait()
		if during-atCancel < 14 {
			t.Fatalf("%d refresh(es) in the 5 minutes after the run's cancellation, want one per 20s tick: the lease lapses during the teardown", during-atCancel)
		}
		if after := lease.count(); after != during {
			t.Fatalf("%d refresh(es) after the engine returned, want none", after-during)
		}
	})
}

// TestLeaseHeartbeat_aFailedRefreshInterruptsTheRun: a lease that cannot be
// refreshed cancels the run as interrupted, so the engine unwinds to
// failed_resumable before another pod takes the run.
func TestLeaseHeartbeat_aFailedRefreshInterruptsTheRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		lease := &countingLease{fail: errors.New("nats: wrong last sequence")}
		r := &Runner{cfg: Config{HeartbeatInterval: 20 * time.Second, Logger: iterlog.Nop()}}
		runCtx, runCancel := context.WithCancelCause(context.Background())
		defer runCancel(nil)
		stop := r.startLeaseHeartbeat(runCtx, runCancel, "run-1", lease, nopProgress{})
		defer stop()
		time.Sleep(30 * time.Second)
		synctest.Wait()
		if !errors.Is(context.Cause(runCtx), runtime.ErrRunInterrupted) {
			t.Fatalf("after a failed refresh the run's cause is %v, want interrupted", context.Cause(runCtx))
		}
	})
}

// TestLeaseHeartbeat_aStuckUnwindReleasesTheLeaseAtItsCeiling: an engine
// that has not returned long after its run's cancellation — past its
// sandbox teardown's own budget — no longer holds the run: the lease lapses
// for a sibling instead of lasting until the pod is killed.
func TestLeaseHeartbeat_aStuckUnwindReleasesTheLeaseAtItsCeiling(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		lease := &countingLease{}
		r := &Runner{cfg: Config{HeartbeatInterval: 20 * time.Second, Logger: iterlog.Nop()}}
		runCtx, runCancel := context.WithCancelCause(context.Background())
		defer runCancel(nil)
		stop := r.startLeaseHeartbeat(runCtx, runCancel, "run-1", lease, nopProgress{})
		defer stop()
		runCancel(runtime.ErrRunInterrupted)
		time.Sleep(unwindLeaseCeiling + time.Minute)
		synctest.Wait()
		atCeiling := lease.count()
		time.Sleep(10 * time.Minute)
		synctest.Wait()
		if atCeiling == 0 {
			t.Fatal("no refresh during the unwind — this proves nothing")
		}
		if after := lease.count(); after != atCeiling {
			t.Fatalf("%d refresh(es) past the ceiling of an unwind that never returns, want none", after-atCeiling)
		}
	})
}

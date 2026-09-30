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
// redelivery stays on the lock; the refreshes stop once the hold ends.
func TestLeaseHeartbeat_holdsTheLeaseThroughACancelledRunsTeardown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		lease := &countingLease{}
		r := &Runner{cfg: Config{HeartbeatInterval: 20 * time.Second, Logger: iterlog.Nop()}}
		runCtx, runCancel := context.WithCancelCause(context.Background())
		defer runCancel(nil)
		hold := r.startLeaseHeartbeat(runCtx, runCancel, "run-1", lease, nopProgress{})
		time.Sleep(30 * time.Second)
		runCancel(runtime.ErrRunInterrupted)
		atCancel := lease.count()
		time.Sleep(5 * time.Minute)
		synctest.Wait()
		during := lease.count()
		hold.stop()
		time.Sleep(time.Minute)
		synctest.Wait()
		if during-atCancel < 14 {
			t.Fatalf("%d refresh(es) in the 5 minutes after the run's cancellation, want one per 20s tick: the lease lapses during the teardown", during-atCancel)
		}
		if after := lease.count(); after != during {
			t.Fatalf("%d refresh(es) after the hold ended, want none", after-during)
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
		hold := r.startLeaseHeartbeat(runCtx, runCancel, "run-1", lease, nopProgress{})
		defer hold.stop()
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
		hold := r.startLeaseHeartbeat(runCtx, runCancel, "run-1", lease, nopProgress{})
		defer hold.stop()
		runCancel(runtime.ErrRunInterrupted)
		time.Sleep(engineUnwindCeiling + time.Minute)
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

// TestLeaseHeartbeat_anEngineReturnWithoutCancellationStartsTheCeiling: a
// park — a human gate, a failed_resumable death — returns from the engine
// without the run ctx ever being cancelled. From that return the lease is
// held through the runner's post-engine steps, then let go at
// postEngineCeiling as when a refresh fails: no longer refreshed, and the run
// ctx interrupted so what still works under it stops.
func TestLeaseHeartbeat_anEngineReturnWithoutCancellationStartsTheCeiling(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		lease := &countingLease{}
		r := &Runner{cfg: Config{HeartbeatInterval: 20 * time.Second, Logger: iterlog.Nop()}}
		runCtx, runCancel := context.WithCancelCause(context.Background())
		defer runCancel(nil)
		hold := r.startLeaseHeartbeat(runCtx, runCancel, "run-1", lease, nopProgress{})
		defer hold.stop()
		time.Sleep(30 * time.Second)
		hold.engineReturned(runtime.ErrRunPaused)
		time.Sleep(postEngineCeiling - time.Minute)
		synctest.Wait()
		beforeCeiling := lease.count()
		time.Sleep(30 * time.Second)
		synctest.Wait()
		held := lease.count() - beforeCeiling
		time.Sleep(time.Minute)
		synctest.Wait()
		atCeiling := lease.count()
		time.Sleep(10 * time.Minute)
		synctest.Wait()
		if held == 0 {
			t.Fatalf("no refresh in the minute before the post-engine ceiling (%s): the lease is let go while the post-engine steps may still run", postEngineCeiling)
		}
		if after := lease.count(); after != atCeiling {
			t.Fatalf("%d refresh(es) past the post-engine ceiling of a run that was never cancelled, want none", after-atCeiling)
		}
		if !errors.Is(context.Cause(runCtx), runtime.ErrRunInterrupted) {
			t.Fatalf("at the post-engine ceiling the run's cause is %v, want interrupted: what still works under the lease must stop", context.Cause(runCtx))
		}
	})
}

// TestLeaseHeartbeat_theEngineReturnHandsTheHoldToThePostEngineCeiling: once
// a cancelled run's engine returned, the unwind's ceiling no longer applies —
// the post-engine steps are held to their own, from the return — or a slow
// teardown would leave them no lease at all.
func TestLeaseHeartbeat_theEngineReturnHandsTheHoldToThePostEngineCeiling(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		lease := &countingLease{}
		r := &Runner{cfg: Config{HeartbeatInterval: 20 * time.Second, Logger: iterlog.Nop()}}
		runCtx, runCancel := context.WithCancelCause(context.Background())
		defer runCancel(nil)
		hold := r.startLeaseHeartbeat(runCtx, runCancel, "run-1", lease, nopProgress{})
		defer hold.stop()
		runCancel(runtime.ErrRunInterrupted)
		time.Sleep(engineUnwindCeiling - time.Minute)
		hold.engineReturned(runtime.ErrRunInterrupted)
		time.Sleep(5 * time.Minute)
		synctest.Wait()
		pastUnwind := lease.count()
		time.Sleep(time.Minute)
		synctest.Wait()
		if lease.count() == pastUnwind {
			t.Fatalf("no refresh %s after the cancellation of a run whose engine returned at %s: the unwind's ceiling cut the post-engine steps' hold", engineUnwindCeiling+5*time.Minute, engineUnwindCeiling-time.Minute)
		}
		time.Sleep(postEngineCeiling)
		synctest.Wait()
		atCeiling := lease.count()
		time.Sleep(10 * time.Minute)
		synctest.Wait()
		if after := lease.count(); after != atCeiling {
			t.Fatalf("%d refresh(es) past the post-engine ceiling, want none", after-atCeiling)
		}
	})
}

// TestLeaseHeartbeat_aFinishedRunKeepsItsLeaseThroughItsPostEngineSteps: a run
// whose engine returned without error is waited on by no resume. Its
// post-engine steps — the bank that makes it landable first — are not cut at
// postEngineCeiling: the lease is still refreshed past it, and the run's
// context is not interrupted.
func TestLeaseHeartbeat_aFinishedRunKeepsItsLeaseThroughItsPostEngineSteps(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		lease := &countingLease{}
		r := &Runner{cfg: Config{HeartbeatInterval: 20 * time.Second, Logger: iterlog.Nop()}}
		runCtx, runCancel := context.WithCancelCause(context.Background())
		defer runCancel(nil)
		hold := r.startLeaseHeartbeat(runCtx, runCancel, "run-1", lease, nopProgress{})
		defer hold.stop()
		time.Sleep(30 * time.Second)
		hold.engineReturned(nil)
		time.Sleep(postEngineCeiling + time.Minute)
		synctest.Wait()
		pastCeiling := lease.count()
		time.Sleep(time.Minute)
		synctest.Wait()
		if lease.count() == pastCeiling {
			t.Fatalf("no refresh past the post-engine ceiling (%s) of a finished run: its bank would be cut", postEngineCeiling)
		}
		if cause := context.Cause(runCtx); cause != nil {
			t.Fatalf("a finished run's context was cancelled (%v) past the post-engine ceiling: its bank would stop", cause)
		}
	})
}

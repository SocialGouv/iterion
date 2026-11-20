package runview

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/runview/runstream"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A Service stop ends every bound subscription it owns on the native
// filesystem store — open, still being set up, or still waiting for its alert
// registration or its admission — with runstream.ErrSourceClosed, never a
// clean end, and joins the waiting ones before it returns. A store other than
// the native one keeps its admission retry outside the Service.

// alertWaiters counts the alert-registration waits parked on runID.
func alertWaiters(svc *Service, runID string) int {
	svc.alertRegistry.mu.Lock()
	defer svc.alertRegistry.mu.Unlock()
	return len(svc.alertRegistry.waiters[runID])
}

// awaitCondition polls cond until it holds; the test fails past d.
func awaitCondition(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not reached within %s", what, d)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// endedLogPipe reads what a log subscription already holds, without waiting:
// ended is true only once its producer finished it (both channels closed).
// Callers read it after a join, which orders the producer's Finish first.
func endedLogPipe(sub runstream.LogSubscription) (errs []error, ended bool) {
	chunks, errc := sub.Chunks(), sub.Errors()
	for chunks != nil || errc != nil {
		select {
		case _, ok := <-chunks:
			if !ok {
				chunks = nil
			}
		case err, ok := <-errc:
			if !ok {
				errc = nil
				continue
			}
			errs = append(errs, err)
		default:
			return errs, false
		}
	}
	return errs, true
}

// stopBackgroundWithin runs StopBackground under a d deadline.
func stopBackgroundWithin(svc *Service, d time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	svc.StopBackground(ctx)
}

// W8a: an event subscription parked in the alert-registration wait (an Active
// run nobody registered yet) is registered before it waits: StopBackground
// cancels it with ErrSourceClosed and joins it, so the subscribe returns
// before the wait's 2 s bound and leaves nothing registered.
func TestStopEndsAPendingEventSubscription(t *testing.T) {
	svc, native, _ := newLiveEventsService(t, WithAlerts(AlertSettings{StallTimeout: time.Hour}))
	ctx := t.Context()
	if err := seedOriginRun(ctx, native, "r", "A", store.LegacyValueFormat, store.RunStatusRunning, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.manager.Register(ctx, "r"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.manager.Deregister("r") })
	bound := boundOriginCtx(t, svc, "r")

	type result struct {
		err error
		at  time.Time
	}
	opened := make(chan result, 1)
	started := time.Now()
	go func() {
		sub, err := runstream.SubscribeEventsCursor(bound, svc.StreamSource(), "r", runstream.EventCursor{Seq: 0, After: true})
		if sub != nil {
			_ = sub.Close()
		}
		opened <- result{err: err, at: time.Now()}
	}()
	awaitCondition(t, 2*time.Second, "the subscribe parked in the registration wait", func() bool { return alertWaiters(svc, "r") == 1 })

	stopBackgroundWithin(svc, 5*time.Second)
	var r result
	select {
	case r = <-opened:
	case <-time.After(5 * time.Second):
		t.Fatal("the subscribe never returned after the stop")
	}
	if !errors.Is(r.err, runstream.ErrSourceClosed) {
		t.Fatalf("subscribe returned %v, want ErrSourceClosed", r.err)
	}
	// The wait's 2 s bound starts after started: a subscribe the stop did not
	// end returns at the bound or later.
	elapsed := r.at.Sub(started)
	t.Logf("the subscribe returned %s after it started (wait bound 2s)", elapsed)
	if elapsed >= 2*time.Second {
		t.Fatalf("the subscribe returned %s after it started, at or past its wait bound: the stop did not end it", elapsed)
	}
	requireReleased(t, svc, "r", "after the stop")
}

// W8b: a bound log subscription still retrying its admission belongs to the
// Service. StopBackground cancels it with ErrSourceClosed and joins it: the
// pipe has ended, typed, when the stop returns — whether the stop lands while
// it retries or while an attempt is being admitted — and no attempt follows.
func TestStopEndsAPendingLogAdmission(t *testing.T) {
	t.Run("retrying", func(t *testing.T) {
		svc, native := newLogLaneService(t)
		seedLogLaneRun(t, native, "r", store.RunStatusRunning)
		var calls atomic.Int32
		retrying := make(chan struct{})
		svc.logAdmissionHook = func(context.Context, string) error {
			if calls.Add(1) == 2 {
				close(retrying)
			}
			return os.ErrDeadlineExceeded // transient: retried, never refused
		}
		sub := subscribeBoundLogs(t, svc, "r")
		select {
		case <-retrying:
		case <-time.After(5 * time.Second):
			t.Fatal("the admission was never retried")
		}

		stopBackgroundWithin(svc, 2*time.Second)
		atStop := calls.Load()
		errs, ended := endedLogPipe(sub)
		if !ended {
			t.Fatalf("the pending log admission outlived StopBackground; errors so far %v", errs)
		}
		if !hasError(errs, runstream.ErrSourceClosed) {
			t.Fatalf("errors = %v, want the typed source closure", errs)
		}
		// The joined retry cannot run again; its next attempt was due 500 ms
		// after the second one.
		time.Sleep(time.Second)
		if n := calls.Load(); n != atStop {
			t.Fatalf("admission attempts went %d -> %d after the stop", atStop, n)
		}
	})

	t.Run("admitted-as-the-stop-lands", func(t *testing.T) {
		svc, native := newLogLaneService(t)
		seedLogLaneRun(t, native, "r", store.RunStatusRunning)
		var calls atomic.Int32
		svc.logAdmissionHook = func(context.Context, string) error {
			if calls.Add(1) == 1 {
				return os.ErrDeadlineExceeded
			}
			return nil
		}
		// The stop lands once the retry was admitted and before its
		// subscription is served: the merge would be set up under a stopped
		// context. (A bound store read refuses a cancelled context, so a stop
		// during the attempt itself never admits.)
		stopped := make(chan struct{})
		svc.liveEvents.afterLogAdmission = func(ctx context.Context) {
			go func() {
				defer close(stopped)
				stopBackgroundWithin(svc, 5*time.Second)
			}()
			select {
			case <-ctx.Done():
			case <-time.After(3 * time.Second):
			}
		}
		sub := subscribeBoundLogs(t, svc, "r")
		select {
		case <-stopped:
		case <-time.After(10 * time.Second):
			t.Fatal("StopBackground never returned")
		}
		errs, ended := endedLogPipe(sub)
		if !ended {
			t.Fatalf("the admission admitted under the stop outlived StopBackground; errors so far %v", errs)
		}
		if !hasError(errs, runstream.ErrSourceClosed) {
			t.Fatalf("errors = %v, want the typed source closure", errs)
		}
	})
}

// W8c: a bound log subscription stopped while its merge is still being set up
// — parked in the first-registration wait of an Active run with no lane —
// ends with ErrSourceClosed, never with the setup's own cancellation.
func TestStopEndsABoundLogSubscriptionInItsSetup(t *testing.T) {
	svc, native := newLogLaneService(t)
	seedLogLaneRun(t, native, "r", store.RunStatusRunning)
	// Active with no lane and a wait bound far past the test: only the stop
	// can end the setup.
	tightenLogLaneWait(t, time.Minute)
	if _, err := svc.manager.Register(t.Context(), "r"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.manager.Deregister("r") })
	sub := subscribeBoundLogs(t, svc, "r")
	sink := newLogSink(sub)
	awaitCondition(t, 5*time.Second, "the bound log merge registered", func() bool { return svc.liveEvents.logStreams() == 1 })

	stopBackgroundWithin(svc, 2*time.Second)
	if n := svc.liveEvents.logStreams(); n != 0 {
		t.Fatalf("logActive = %d after the stop, want 0", n)
	}
	text, errs := sink.awaitEnd(t, 2*time.Second)
	if !hasError(errs, runstream.ErrSourceClosed) {
		t.Fatalf("text %q errors %v, want the typed source closure", text, errs)
	}
}

// The legacy path keeps its untracked admission retry: on a wrapper of the
// native store StopBackground neither ends nor joins it, and it retries on.
func TestStopLeavesALegacyLogAdmissionRetrying(t *testing.T) {
	dir := t.TempDir()
	native, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	svc := newWrappedStreamService(t, dir, &transientAdmissionStore{FilesystemRunStore: native})
	if svc.liveEvents != nil {
		t.Fatal("a wrapper of the native store took the native live path")
	}
	if err := seedOriginRun(t.Context(), native, "r", "A", store.LegacyValueFormat, store.RunStatusRunning, 0); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	svc.logAdmissionHook = func(context.Context, string) error {
		calls.Add(1)
		return os.ErrDeadlineExceeded
	}
	sub, err := svc.StreamSource().SubscribeLogs(boundOriginCtx(t, svc, "r"), "r", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sub.Close() })
	awaitCondition(t, 5*time.Second, "the legacy admission retried", func() bool { return calls.Load() >= 2 })

	stopBackgroundWithin(svc, 2*time.Second)
	atStop := calls.Load()
	if errs, ended := endedLogPipe(sub); ended {
		t.Fatalf("StopBackground ended a legacy pending admission; errors %v", errs)
	}
	awaitCondition(t, 10*time.Second, "the legacy admission retried after the stop", func() bool { return calls.Load() > atStop })
}

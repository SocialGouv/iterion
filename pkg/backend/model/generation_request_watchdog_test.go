package model

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"
	"github.com/SocialGouv/iterion/pkg/errtrack"
)

type watchdogAmbiguousStart struct{}

func (watchdogAmbiguousStart) Error() string         { return "provider start ambiguous" }
func (watchdogAmbiguousStart) AmbiguousEffect() bool { return true }

// watchdogStartClient runs before (if set) inside StreamResponse, then fails
// the start with err, or blocks until the request context ends when block is
// set.
type watchdogStartClient struct {
	before func()
	err    error
	block  bool
}

func (c *watchdogStartClient) StreamResponse(ctx context.Context, _ api.CreateMessageRequest) (<-chan api.StreamEvent, error) {
	if c.before != nil {
		c.before()
	}
	if c.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return nil, c.err
}

// startForTest mirrors callAndAggregate's wiring: the request context is a
// child of the parent the watchdog also watches.
func startForTest(parent context.Context, client api.APIClient, coldTimeout time.Duration) (<-chan api.StreamEvent, error) {
	requestCtx, cancelRequest := context.WithCancel(parent)
	defer cancelRequest()
	return startStreamWithColdWatchdog(parent, requestCtx, client, api.CreateMessageRequest{}, coldTimeout)
}

// bothReadyParent is a run context whose first Done call — the watchdog's
// select — cancels it, then waits until the client has returned its failure
// and the watchdog goroutine has had time to deliver it. The select then
// finds both its result arm and its cancellation arm ready, the order CI
// hits under load, which a synchronous stub almost never reproduces.
type bothReadyParent struct {
	context.Context
	cancel   context.CancelFunc
	returned chan struct{}
	once     sync.Once
}

func newBothReadyParent() *bothReadyParent {
	ctx, cancel := context.WithCancel(context.Background())
	return &bothReadyParent{Context: ctx, cancel: cancel, returned: make(chan struct{})}
}

func (p *bothReadyParent) Done() <-chan struct{} {
	p.once.Do(func() {
		p.cancel()
		<-p.returned
		time.Sleep(time.Millisecond)
	})
	return p.Context.Done()
}

// A stream start that fails after the run's context ended reports the
// cancellation, whatever goroutine the scheduler runs first: the provider's
// error (here an ambiguous effect) never decides the terminal verdict of a
// cancelled run, with or without the cold watchdog.
func TestStartStreamColdWatchdogRacedCancelIsTheVerdict(t *testing.T) {
	t.Parallel()
	assertCancelVerdict := func(t *testing.T, i int, stream <-chan api.StreamEvent, err error) {
		t.Helper()
		if stream != nil {
			t.Fatalf("iteration %d: a failed start returned a stream", i)
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("iteration %d: err=%v, want the run's cancellation", i, err)
		}
		if errors.As(err, new(watchdogAmbiguousStart)) || errtrack.IsAmbiguousEffect(err) {
			t.Fatalf("iteration %d: err=%v still classifies as the provider's ambiguous effect", i, err)
		}
	}

	t.Run("failed_start_racing_parent_cancel/watchdog", func(t *testing.T) {
		for i := range 200 {
			parent := newBothReadyParent()
			// The request context is detached from parent so only the
			// watchdog's select calls parent.Done.
			requestCtx, stopRequest := context.WithCancel(context.Background())
			client := &watchdogStartClient{before: func() {
				<-parent.Context.Done()
				close(parent.returned)
			}, err: watchdogAmbiguousStart{}}
			stream, err := startStreamWithColdWatchdog(parent, requestCtx, client, api.CreateMessageRequest{}, time.Minute)
			stopRequest()
			assertCancelVerdict(t, i, stream, err)
		}
	})

	t.Run("failed_start_after_parent_cancel/no_watchdog", func(t *testing.T) {
		for i := range 20 {
			parent, cancel := context.WithCancel(context.Background())
			client := &watchdogStartClient{before: cancel, err: watchdogAmbiguousStart{}}
			stream, err := startForTest(parent, client, 0)
			cancel()
			assertCancelVerdict(t, i, stream, err)
			// Without the watchdog the start's result is the only one
			// observed, so its diagnostic always reaches the text.
			if !strings.Contains(err.Error(), "provider start ambiguous") {
				t.Fatalf("iteration %d: err=%q drops the provider's diagnostic", i, err)
			}
		}
	})

	t.Run("failed_start_surfaces_without_cancel", func(t *testing.T) {
		sentinel := watchdogAmbiguousStart{}
		for _, cold := range []time.Duration{time.Minute, 0} {
			_, err := startForTest(context.Background(), &watchdogStartClient{err: sentinel}, cold)
			if !errors.Is(err, sentinel) || errors.Is(err, context.Canceled) {
				t.Fatalf("cold=%s: err=%v, want the provider's error verbatim", cold, err)
			}
		}
	})

	t.Run("blocked_client_regains_control_on_cancel", func(t *testing.T) {
		parent, cancel := context.WithCancel(context.Background())
		time.AfterFunc(10*time.Millisecond, cancel)
		// The request context is detached from the parent: only the watchdog's
		// own parent arm can hand control back.
		requestCtx, stopRequest := context.WithCancel(context.Background())
		t.Cleanup(stopRequest)
		done := make(chan error, 1)
		go func() {
			_, err := startStreamWithColdWatchdog(parent, requestCtx, &watchdogStartClient{block: true}, api.CreateMessageRequest{}, time.Minute)
			done <- err
		}()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err=%v, want the run's cancellation", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("a client blocked on a request context that never ends kept the watchdog past the run's cancellation")
		}
	})
}

// expiredColdParent is a run context whose first Done call cancels it and
// then outlasts the cold allowance, so the cancellation and the expired cold
// window are both facts by the time anyone reads the verdict. Later Done
// calls wait (bounded) until the client returned, so the watchdog's select
// also finds its result arm ready.
type expiredColdParent struct {
	context.Context
	cancel   context.CancelFunc
	outlast  time.Duration
	returned chan struct{}
	mu       sync.Mutex
	calls    int
}

func newExpiredColdParent(outlast time.Duration) *expiredColdParent {
	ctx, cancel := context.WithCancel(context.Background())
	return &expiredColdParent{Context: ctx, cancel: cancel, outlast: outlast, returned: make(chan struct{})}
}

func (p *expiredColdParent) Done() <-chan struct{} {
	p.mu.Lock()
	p.calls++
	first := p.calls == 1
	p.mu.Unlock()
	if first {
		p.cancel()
		time.Sleep(p.outlast)
	} else {
		select {
		case <-p.returned:
			time.Sleep(time.Millisecond)
		case <-time.After(time.Second):
		}
	}
	return p.Context.Done()
}

// When the cold window has expired AND the run is cancelled, the verdict is
// the cancellation: an expired cold allowance on a cancelled run is not a
// stalled provider, and a StreamIdleError there would be booked as an
// abandoned, retryable call. callAndAggregate's twin lives in
// generation_test.go (it sets the cold timeout through the environment).
func TestColdWatchdogExpiredOnCancelledRunIsTheCancellation(t *testing.T) {
	t.Parallel()
	t.Run("timer_and_parent_arms_both_ready", func(t *testing.T) {
		for i := range 200 {
			parent := newExpiredColdParent(5 * time.Millisecond)
			requestCtx, stopRequest := context.WithCancel(context.Background())
			_, err := startStreamWithColdWatchdog(parent, requestCtx, &watchdogStartClient{block: true}, api.CreateMessageRequest{}, time.Millisecond)
			stopRequest()
			if !errors.Is(err, context.Canceled) || errors.As(err, new(*StreamIdleError)) {
				t.Fatalf("iteration %d: err=%v, want the run's cancellation", i, err)
			}
		}
	})

}

// The provider's message after a cancellation rides the text only: typed
// classifiers see the cancellation, text matchers still see the message,
// and extraction accounting books the start as an unknown bill.
func TestStartFailureAfterCancelReachesTextMatchersAndAccounting(t *testing.T) {
	t.Parallel()
	t.Run("text_matcher_sees_provider_message", func(t *testing.T) {
		parent, cancel := context.WithCancel(context.Background())
		defer cancel()
		client := &watchdogStartClient{before: cancel, err: errors.New("400 invalid_request_error: prompt is too long: 250000 tokens > 200000 maximum")}
		_, err := startForTest(parent, client, 0)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v, want the run's cancellation", err)
		}
		if !isContextWindowError(err) {
			t.Fatalf("err=%v: isContextWindowError no longer reads the provider's text", err)
		}
	})

	t.Run("extraction_books_definite_refusal_as_unknown_bill", func(t *testing.T) {
		parent, cancel := context.WithCancel(context.Background())
		defer cancel()
		obs := &extractionClient{inner: &watchdogStartClient{before: cancel, err: errors.New("401 authentication_error: invalid x-api-key")}}
		_, err := startForTest(parent, obs, 0)
		obs.observeResponse(ResponseInfo{Error: err})
		if got := obs.settleUsage(Usage{}).UnreportedCalls; got != 1 {
			t.Fatalf("err=%v unreported=%d, want the deliberate over-count of 1", err, got)
		}
	})
}

// The aggregation watchdog's timer arm honours the same rule: a stream whose
// silence window expires on a cancelled run ends on the cancellation.
func TestAggregateWatchdogExpiredOnCancelledRunIsTheCancellation(t *testing.T) {
	t.Parallel()
	for i := range 200 {
		parent := newExpiredColdParent(5 * time.Millisecond)
		close(parent.returned)
		res := aggregateStreamWithIdleWatchdog(parent, make(chan api.StreamEvent), time.Millisecond, time.Minute)
		if !errors.Is(res.err, context.Canceled) || errors.As(res.err, new(*StreamIdleError)) {
			t.Fatalf("iteration %d: err=%v, want the run's cancellation", i, res.err)
		}
	}
}

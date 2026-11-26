package model

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"

	"github.com/SocialGouv/iterion/pkg/errtrack"
)

// unobservedCancel is a context whose cancellation no select can observe:
// Done never fires, Err reports the real context's state. It drives the
// order in which a provider outcome is selected while the cancellation has
// already happened — the order the scheduler picks when both arms are
// ready, made deterministic.
type unobservedCancel struct{ context.Context }

func (unobservedCancel) Done() <-chan struct{} { return nil }

type cancelVerdictAmbiguous struct{}

func (cancelVerdictAmbiguous) Error() string         { return "provider transport ambiguous" }
func (cancelVerdictAmbiguous) AmbiguousEffect() bool { return true }

// cancellingClient cancels the run inside StreamResponse, then returns a
// start error (startErr) or a stream fed by feed. With hold, a start error is
// returned, and a stream fed, only once the test releases it.
type cancellingClient struct {
	cancel   context.CancelFunc
	hold     bool
	startErr error
	feed     func(chan<- api.StreamEvent)
	release  chan struct{}
	once     sync.Once
}

func (c *cancellingClient) StreamResponse(context.Context, api.CreateMessageRequest) (<-chan api.StreamEvent, error) {
	c.cancel()
	if c.startErr != nil {
		if c.hold {
			<-c.release
		}
		return nil, c.startErr
	}
	ch := make(chan api.StreamEvent, 1)
	if c.hold {
		// The stream opens, and speaks only once the test released it, after
		// the call returned: aggregation observes the cancellation first, and
		// its drainer consumes the late frames.
		go func() {
			<-c.release
			c.feed(ch)
		}()
	} else {
		c.feed(ch)
	}
	return ch, nil
}

func (c *cancellingClient) releaseHeld() { c.once.Do(func() { close(c.release) }) }

// TestCallAndAggregateCancelledRunVerdictIsOrderIndependent drives every
// failing exit of a provider call on a run its parent cancelled, in both
// orders: the provider's outcome selected first, and the cancellation
// selected first. The verdict is the cancellation in both, never the
// provider's echo (an ambiguous effect, a retryable transport fault, a cold
// stall). The booking is order-independent on the exits driven here: the
// unwatched opened streams (booked) and the unopened starts (not booked).
// Under the watched start, a stream that opens exactly as the run is
// cancelled is booked 1 or 0 by which arm of startStreamWithColdWatchdog's
// select wins; that race is not covered here.
func TestCallAndAggregateCancelledRunVerdictIsOrderIndependent(t *testing.T) {
	errorFrame := func(ch chan<- api.StreamEvent) {
		ch <- api.StreamEvent{Type: api.EventError, ErrorMessage: "connection reset by peer"}
		close(ch)
	}
	closedEarly := func(ch chan<- api.StreamEvent) { close(ch) }
	exits := []struct {
		name     string
		cold     string
		startErr error
		feed     func(chan<- api.StreamEvent)
		// silent holds the provider past the cold window in the
		// provider-first order: the watchdog's timer is the exit.
		silent bool
		// opened is whether the call reaches aggregation and is booked
		// unreported.
		opened bool
	}{
		{name: "start error", cold: "1m", startErr: cancelVerdictAmbiguous{}},
		{name: "start error without watchdog", cold: "0", startErr: cancelVerdictAmbiguous{}},
		{name: "cold window", cold: "20ms", startErr: cancelVerdictAmbiguous{}, silent: true},
		// The stream exits start without the watchdog: a synchronous start
		// always hands aggregation the opened stream, whatever the order.
		{name: "error frame", cold: "0", feed: errorFrame, opened: true},
		{name: "stream closed early", cold: "0", feed: closedEarly, opened: true},
	}
	for _, exit := range exits {
		for _, order := range []string{"provider first", "cancellation first"} {
			if exit.cold == "0" && exit.startErr != nil && order == "cancellation first" {
				// Without the watchdog the start is synchronous: no select
				// can observe the cancellation before the provider returns.
				continue
			}
			t.Run(exit.name+"/"+order, func(t *testing.T) {
				t.Setenv("ITERION_CLAW_STREAM_COLD_TIMEOUT", exit.cold)
				t.Setenv("ITERION_CLAW_STREAM_IDLE_TIMEOUT", "0")
				run, cancel := context.WithCancel(context.Background())
				defer cancel()
				client := &cancellingClient{
					cancel:   cancel,
					hold:     order == "cancellation first" || exit.silent,
					startErr: exit.startErr,
					feed:     exit.feed,
					release:  make(chan struct{}),
				}
				t.Cleanup(client.releaseHeld)
				ctx := run
				if order == "provider first" {
					ctx = unobservedCancel{run}
				}
				var mu sync.Mutex
				var responses []error
				agg, err := callAndAggregate(ctx, client, api.CreateMessageRequest{Model: "test-model"}, GenerationOptions{
					Model: "test-model",
					OnResponse: func(info ResponseInfo) {
						mu.Lock()
						defer mu.Unlock()
						responses = append(responses, info.Error)
					},
				})
				client.releaseHeld()

				verdict := err
				if verdict == nil && agg != nil {
					verdict = agg.err
				}
				if !errors.Is(verdict, context.Canceled) || errtrack.IsAmbiguousEffect(verdict) ||
					errors.As(verdict, new(*APIError)) || errors.As(verdict, new(*StreamIdleError)) {
					t.Fatalf("verdict = %v, want the run's cancellation alone", verdict)
				}
				if exit.opened {
					if err != nil || agg == nil || agg.usage.UnreportedCalls != 1 {
						t.Fatalf("opened call: err=%v agg=%+v, want an aggregate booking one unreported call", err, agg)
					}
				} else if agg != nil {
					t.Fatalf("unopened call: agg=%+v, want none (an unopened call is not booked)", agg)
				}
				mu.Lock()
				defer mu.Unlock()
				if len(responses) != 1 || !errors.Is(responses[0], context.Canceled) || errtrack.IsAmbiguousEffect(responses[0]) {
					t.Fatalf("OnResponse errors = %v, want one cancellation", responses)
				}
			})
		}
	}
}

// TestStreamVerdictKeepsALiveRunsOutcome pins the other half of the
// chokepoint: on a live run the provider's outcome is the verdict, and a
// completed response stays a success on a cancelled run.
func TestStreamVerdictKeepsALiveRunsOutcome(t *testing.T) {
	ambiguous := cancelVerdictAmbiguous{}
	if got := streamVerdict(context.Background(), ambiguous); got != error(ambiguous) {
		t.Fatalf("live run: verdict = %v, want the provider's error unchanged", got)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if got := streamVerdict(cancelled, nil); got != nil {
		t.Fatalf("completed response on a cancelled run: verdict = %v, want nil", got)
	}
	if got := streamVerdict(cancelled, context.Canceled); got != context.Canceled { //nolint:errorlint // identity: the context's own error passes unwrapped
		t.Fatalf("context error: verdict = %v, want it unchanged", got)
	}
	deadline, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer stop()
	got := streamVerdict(deadline, ambiguous)
	if !errors.Is(got, context.DeadlineExceeded) || errtrack.IsAmbiguousEffect(got) {
		t.Fatalf("deadline: verdict = %v, want the deadline alone", got)
	}
	var echo *streamCancelledError
	if !errors.As(got, &echo) || echo.Observed != error(ambiguous) {
		t.Fatalf("deadline: verdict = %#v, want the provider's outcome kept as Observed", got)
	}
}

// lateOpeningClient cancels the run, keeps the CPU past the cold window, then
// opens a stream that never speaks: the start returns an opened stream once
// both the cold window and the run are over.
type lateOpeningClient struct {
	cancel context.CancelFunc
	spin   time.Duration
}

func (c *lateOpeningClient) StreamResponse(context.Context, api.CreateMessageRequest) (<-chan api.StreamEvent, error) {
	c.cancel()
	for t0 := time.Now(); time.Since(t0) < c.spin; {
	}
	return make(chan api.StreamEvent), nil
}

// TestStructuredExtractionBooksALateOpenedCancelledCallOnce drives the
// structured-extraction path (extractionClient observing direct generation)
// through a stream that opens after its cold window on a cancelled run. The
// call is booked once: the abandoned call's booking rides the OnResponse
// usage, so the observer's cancellation adjustment does not add a second.
// Which exit a start takes depends on scheduling, so the loop also requires
// that the late-opened exit was reached at least once.
func TestStructuredExtractionBooksALateOpenedCancelledCallOnce(t *testing.T) {
	// The test pins the process to one P, so it stays serial (see serialFamily).
	prev := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(prev)
	t.Setenv("ITERION_CLAW_STREAM_COLD_TIMEOUT", "50us")
	t.Setenv("ITERION_CLAW_STREAM_IDLE_TIMEOUT", "0")
	reached := 0
	for i := 0; i < 200; i++ {
		run, cancel := context.WithCancel(context.Background())
		observed := &extractionClient{inner: &lateOpeningClient{cancel: cancel, spin: 300 * time.Microsecond}}
		var responses []error
		opts := GenerationOptions{
			Model:          "test-model",
			ExplicitSchema: json.RawMessage(`{"type":"object","properties":{"a":{"type":"string"}}}`),
			Messages:       []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "x"}}}},
			OnResponse: func(info ResponseInfo) {
				responses = append(responses, info.Error)
				observed.observeResponse(info)
			},
		}
		obj, _ := GenerateObjectDirect[map[string]any](unobservedCancel{run}, observed, opts)
		partial := objectUsage(obj).UnreportedCalls
		settled := observed.settleUsage(objectUsage(obj)).UnreportedCalls
		cancel()
		observed.mu.Lock()
		entered := observed.entered
		observed.mu.Unlock()
		if settled > entered {
			t.Fatalf("iteration %d: settled UnreportedCalls = %d for %d entered call(s) (aggregate %d, OnResponse errors %v), want at most one booking per call",
				i, settled, entered, partial, responses)
		}
		if len(responses) == 1 && errors.As(responses[0], new(*streamCancelledError)) && partial == 1 {
			reached++
		}
	}
	if reached == 0 {
		t.Fatal("no iteration reached the late-opened exit (a cancelled verdict with an aggregate booking one call): the test proved nothing")
	}
	t.Logf("late-opened exit reached in %d/200 iterations", reached)
}

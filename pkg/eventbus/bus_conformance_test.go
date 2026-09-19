package eventbus

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// The Bus contract (bus.go): cancel signals the handler's context, then waits
// for an in-flight delivery within a bounded budget; a handler ignoring its
// context is named in a warning rather than waited out.
//
// #1343: the two implementations disagreed on this — InProcBus enforced it
// (unbounded) and NATSBus enforced NEITHER half. The table below exercises
// both against the same properties so a bus that skips one fails on its own
// row. Adding a new Bus implementation is one entry here away from the
// property being enforced on it too.

type busCase struct {
	name string
	// make returns a Bus wired to buf (for warning-log assertions) and a
	// cleanup callback.
	make func(t *testing.T, logger *iterlog.Logger, budget time.Duration) (Bus, func())
}

var busCases = []busCase{
	{
		name: "inproc",
		make: func(t *testing.T, logger *iterlog.Logger, budget time.Duration) (Bus, func()) {
			bus := NewInProcBus(logger)
			if budget > 0 {
				bus.cancelBudget = budget
			}
			return bus, func() {}
		},
	},
	{
		name: "nats",
		make: func(t *testing.T, logger *iterlog.Logger, budget time.Duration) (Bus, func()) {
			broker := newFakeBroker()
			bus, err := newNATSBus(broker, NATSOptions{Logger: logger})
			if err != nil {
				t.Fatalf("newNATSBus: %v", err)
			}
			if budget > 0 {
				bus.cancelBudget = budget
			}
			return bus, func() {}
		},
	},
	{
		// nats-async delivers callbacks on independent goroutines, mirroring
		// the transport patterns nats.go supports (parallel per-message
		// dispatch, future queue-group fan-out). The synchronous fakeBroker
		// alone would hide a WaitGroup race in NATSBus.Subscribe — the
		// adversarial round on #1343 reproduced exactly that panic against
		// an async broker. This row keeps the property honest.
		name: "nats-async",
		make: func(t *testing.T, logger *iterlog.Logger, budget time.Duration) (Bus, func()) {
			broker := newAsyncFakeBroker()
			bus, err := newNATSBus(broker, NATSOptions{Logger: logger})
			if err != nil {
				t.Fatalf("newNATSBus: %v", err)
			}
			if budget > 0 {
				bus.cancelBudget = budget
			}
			return bus, broker.stop
		},
	},
}

// newBufLogger returns a logger writing to buf at Warn+ (which is what the
// cancel-overrun paths emit).
func newBufLogger(buf *bytes.Buffer) *iterlog.Logger {
	return iterlog.New(iterlog.LevelWarn, buf)
}

// TestBusSubscribeCancelSignalsHandler asserts that a handler respecting its
// context observes the cancel signal AND cancel returns promptly (the wait
// short-circuits when the handler returns before the budget is spent).
func TestBusSubscribeCancelSignalsHandler(t *testing.T) {
	for _, c := range busCases {
		t.Run(c.name, func(t *testing.T) {
			bus, cleanup := c.make(t, nil, 0)
			defer cleanup()

			entered := make(chan struct{})
			handlerErr := make(chan error, 1)
			cancel, err := bus.Subscribe("worker", trigger.Matcher{}, func(ctx context.Context, _ trigger.Event) error {
				close(entered)
				<-ctx.Done() // a well-behaved handler observes its ctx
				handlerErr <- ctx.Err()
				return ctx.Err()
			})
			if err != nil {
				t.Fatalf("Subscribe: %v", err)
			}
			// Publish on a separate goroutine: the fake NATS broker delivers
			// synchronously, and InProc's worker owns the delivery goroutine.
			// Either way, Publish must not race the cancel.
			go func() { _ = bus.Publish(context.Background(), trigger.Event{Source: trigger.SourceBoard}) }()
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("handler never entered — Publish did not reach the subscriber")
			}

			done := make(chan struct{})
			started := time.Now()
			go func() { cancel(context.Background()); close(done) }()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("cancel(context.Background()) hung — the in-flight handler never observed context cancellation")
			}
			// Well behaved: the wait should short-circuit as soon as the
			// handler returned, well below the 500ms budget.
			if elapsed := time.Since(started); elapsed > 200*time.Millisecond {
				t.Errorf("cancel took %v; expected sub-budget short-circuit", elapsed)
			}
			select {
			case gotErr := <-handlerErr:
				if gotErr != context.Canceled {
					t.Errorf("handler ctx.Err() = %v; want context.Canceled", gotErr)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("handler never reported its ctx.Err()")
			}
			// Idempotent.
			cancel(context.Background())
			cancel(context.Background())
		})
	}
}

// TestBusSubscribeCancelBoundedForRunawayHandler asserts that a handler
// ignoring its context cannot hold cancel past the bounded budget; a warning
// naming the subscriber is emitted so the operator sees the overrun. This is
// what #1343 called "a handler that ignores cancel is named as an overrun
// rather than hanging" — pre-fix, NATSBus.cancel returned instantly (no wait)
// and InProcBus.cancel hung forever.
func TestBusSubscribeCancelBoundedForRunawayHandler(t *testing.T) {
	for _, c := range busCases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			// A tight budget makes the test deterministic on shared CI:
			// the assertion is that cancel returned WITHIN [budget, budget+slack],
			// which never depends on wall clock ratios (see grep-la-classe:
			// count/order, not duration ratios).
			budget := 80 * time.Millisecond
			bus, cleanup := c.make(t, newBufLogger(&buf), budget)
			defer cleanup()

			release := make(chan struct{})
			defer close(release)
			entered := make(chan struct{})
			cancel, err := bus.Subscribe("runaway", trigger.Matcher{}, func(_ context.Context, _ trigger.Event) error {
				close(entered)
				<-release // never observe ctx
				return nil
			})
			if err != nil {
				t.Fatalf("Subscribe: %v", err)
			}
			go func() { _ = bus.Publish(context.Background(), trigger.Event{Source: trigger.SourceBoard}) }()
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("handler never entered")
			}

			done := make(chan struct{})
			started := time.Now()
			go func() { cancel(context.Background()); close(done) }()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("cancel(context.Background()) hung past the bounded budget")
			}
			elapsed := time.Since(started)
			// The wait is BOUNDED — cancel MUST have taken at least the budget
			// (otherwise it didn't wait) and no more than a generous slack for
			// scheduler jitter on shared machines. Mutation the test defends
			// against: removing the wait on NATSBus makes elapsed < budget/2.
			if elapsed < budget/2 {
				t.Errorf("cancel returned in %v — the bus did not wait for the in-flight handler; #1343 regressed", elapsed)
			}
			if elapsed > 2*time.Second {
				t.Errorf("cancel took %v; expected bounded by %s+slack", elapsed, budget)
			}
			// A warning must name the subscriber so the operator can attribute
			// the cut. Mutation: dropping the log line makes this line red.
			if s := buf.String(); !strings.Contains(s, "runaway") {
				t.Errorf("expected warning naming the runaway subscriber; got %q", s)
			}
		})
	}
}

// TestBusSubscribeCancelIdempotent asserts that a second cancel is a no-op
// (never panics, never re-waits). Both buses use sync.Once; the property is
// worth pinning because a caller that stores the cancel through several fields
// (Server.gateReconcileCancel + Server.assistantWatchCancel wrap it) may call
// it twice on shutdown.
func TestBusSubscribeCancelIdempotent(t *testing.T) {
	for _, c := range busCases {
		t.Run(c.name, func(t *testing.T) {
			bus, cleanup := c.make(t, nil, 0)
			defer cleanup()
			cancel, err := bus.Subscribe("idem", trigger.Matcher{}, func(context.Context, trigger.Event) error { return nil })
			if err != nil {
				t.Fatalf("Subscribe: %v", err)
			}
			cancel(context.Background())
			cancel(context.Background())
			cancel(context.Background())
		})
	}
}

// TestBusSubscribeCancelWaitsForStoreWrite is the ticket's specific scenario:
// a handler doing store I/O when SIGTERM lands must complete or observe
// cancellation before cancel returns. Modelled as a handler that increments
// a counter under a mutex — if the wait is skipped, cancel returns while the
// increment is racing (visible under `-race`).
func TestBusSubscribeCancelWaitsForStoreWrite(t *testing.T) {
	for _, c := range busCases {
		t.Run(c.name, func(t *testing.T) {
			// A generous budget so the well-behaved handler always finishes
			// inside it and the assertion is about waiting, not overrunning.
			bus, cleanup := c.make(t, nil, 200*time.Millisecond)
			defer cleanup()

			var mu sync.Mutex
			var writes int
			var completed atomic.Bool
			release := make(chan struct{})
			entered := make(chan struct{})
			cancel, err := bus.Subscribe("store", trigger.Matcher{}, func(ctx context.Context, _ trigger.Event) error {
				close(entered)
				// Wait for the test to signal release OR the ctx to cancel;
				// either way finish the "store write" before returning.
				select {
				case <-release:
				case <-ctx.Done():
				}
				mu.Lock()
				writes++
				mu.Unlock()
				completed.Store(true)
				return nil
			})
			if err != nil {
				t.Fatalf("Subscribe: %v", err)
			}
			go func() { _ = bus.Publish(context.Background(), trigger.Event{Source: trigger.SourceBoard}) }()
			<-entered
			// Release the handler and cancel concurrently: the cancel arm
			// must not race the write. A bus that skips the wait returns
			// while the increment is in flight — TestRace sees the write.
			go func() { close(release) }()
			cancel(context.Background())
			if !completed.Load() {
				t.Fatal("cancel returned before the handler completed its store write — the wait was skipped (#1343)")
			}
			mu.Lock()
			if writes != 1 {
				t.Errorf("writes=%d; want 1", writes)
			}
			mu.Unlock()
		})
	}
}

// TestNATSBus_SubscribeCancelConcurrentPublishNoWGRace stresses the specific
// race the adversarial round on this PR caught: a callback dispatched by the
// transport in parallel with cancel, so `cb: WG.Add` runs racing with
// `cancel: WG.Wait`. Without the mu/closed gate in natsSub, Go's WaitGroup
// panics with "reused before previous Wait has returned"; with the gate the
// callback returns without Add-ing.
//
// The property is NATS-specific (InProc's per-sub worker owns dispatch and
// cannot race itself), so this test does not go through the busCases table.
func TestNATSBus_SubscribeCancelConcurrentPublishNoWGRace(t *testing.T) {
	broker := newAsyncFakeBroker()
	t.Cleanup(broker.stop)
	bus, err := newNATSBus(broker, NATSOptions{})
	if err != nil {
		t.Fatalf("newNATSBus: %v", err)
	}

	var callbackCount atomic.Int64
	cancel, err := bus.Subscribe("racer", trigger.Matcher{}, func(context.Context, trigger.Event) error {
		callbackCount.Add(1)
		return nil
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// Publish a burst; each hops onto its own goroutine. Immediately cancel
	// concurrently — some callbacks land before cancel closes the gate,
	// others after. Without the gate, the "after" callbacks race Wait().
	var pubWG sync.WaitGroup
	for i := 0; i < 200; i++ {
		pubWG.Add(1)
		go func() {
			defer pubWG.Done()
			_ = bus.Publish(context.Background(), trigger.Event{Source: trigger.SourceBoard})
		}()
	}
	// Interleave cancel with the burst so some Publish→cb goroutines land
	// after ns.closed=true.
	cancel(context.Background())
	pubWG.Wait()
	// The count is not asserted (the race is what matters); a run that
	// panics is the failure this test catches under -race.
	_ = callbackCount.Load()
}

// TestBusSubscribeCancelSharedBudget is the #1477 medium property: N slow
// subscribers cancelled under ONE joinCtx cost ONE budget total, not N ×
// budget. Under the pre-fix per-subscription budget, seven slow-Mongo
// handlers ate up to 3.5s of the grace period serially — the very shape
// #1257 rejects for loops, arriving from the bus side.
//
// Only runs on buses that dispatch callbacks in parallel — the sync
// fakeBroker delivers messages serially through a single goroutine, so a
// blocked handler blocks the whole broker and the setup itself deadlocks.
// The nats-async row is the honest test of the property; InProc's per-sub
// worker also matches (each sub's worker is its own goroutine). Mutation:
// revert to per-subscription budget → N × budget → red.
func TestBusSubscribeCancelSharedBudget(t *testing.T) {
	for _, c := range busCases {
		if c.name == "nats" {
			// Sync fakeBroker delivers to subs in a serial for-loop, so a
			// slow-unwind handler on sub 0 blocks the delivery to sub 1..N.
			// Property proven on inproc + nats-async.
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			// Per-subscription budget is 80ms; N=5 so serial N× would be 400ms.
			// The shared joinCtx caps total at 120ms.
			const N = 5
			perSubBudget := 80 * time.Millisecond
			bus, cleanup := c.make(t, nil, perSubBudget)
			defer cleanup()

			entered := make([]chan struct{}, N)
			enteredOnce := make([]sync.Once, N)
			cancels := make([]func(context.Context), N)
			// The unwind sleep is longer than the joinCtx budget so a
			// per-subscription wait would have to expire, not short-circuit
			// on handler exit — that's what makes N × budget observable.
			unwindDelay := 3 * perSubBudget
			for i := 0; i < N; i++ {
				entered[i] = make(chan struct{})
				idx := i
				cancel, err := bus.Subscribe("slow-"+string(rune('A'+idx)), trigger.Matcher{}, func(ctx context.Context, _ trigger.Event) error {
					enteredOnce[idx].Do(func() { close(entered[idx]) })
					<-ctx.Done() // cooperate on cancel signal
					// Simulate a slow store-write unwind: the handler
					// keeps running past ctx.Done for longer than the
					// per-sub budget, so cancel's wait has to time out.
					time.Sleep(unwindDelay)
					return nil
				})
				if err != nil {
					t.Fatalf("Subscribe %d: %v", i, err)
				}
				cancels[i] = cancel
			}

			// One publish reaches every subscriber (distinct queue-group
			// names → each sub is its own group of 1 member → each group
			// gets the delivery).
			go func() { _ = bus.Publish(context.Background(), trigger.Event{Source: trigger.SourceBoard}) }()
			for i := 0; i < N; i++ {
				select {
				case <-entered[i]:
				case <-time.After(2 * time.Second):
					t.Fatalf("subscriber %d never entered", i)
				}
			}

			// ONE joinCtx shared across the N cancels — the shape
			// pkg/server.Shutdown threads under this PR (#1477).
			joinBudget := perSubBudget + 40*time.Millisecond
			joinCtx, joinCancel := context.WithTimeout(context.Background(), joinBudget)
			defer joinCancel()

			started := time.Now()
			for _, cancel := range cancels {
				cancel(joinCtx)
			}
			elapsed := time.Since(started)
			// Serial per-sub budgets would give N × perSubBudget = 400ms;
			// the assertion below reddens the pre-#1477 shape by any margin.
			if elapsed >= time.Duration(N-1)*perSubBudget {
				t.Errorf("%s: N=%d cancels shared joinCtx took %v; per-subscription composition would give ~%v — the shared budget is not being honoured", c.name, N, elapsed, time.Duration(N)*perSubBudget)
			}
			ceiling := joinBudget + 200*time.Millisecond
			if elapsed > ceiling {
				t.Errorf("%s: N=%d cancels took %v; expected ≤ %v (budget %v + slack)", c.name, N, elapsed, ceiling, joinBudget)
			}
			// Wait for the slow handlers to unwind so the cleanup can
			// drain the async broker's dispatch goroutines cleanly.
			time.Sleep(unwindDelay + 100*time.Millisecond)
		})
	}
}

// A silent no-op consumer of io.Writer to prove the logger construction path
// in tests works without buf allocations (paranoia against the "logger nil
// panics on Warn" defect this package's tests would otherwise not catch).
var _ io.Writer = (*bytes.Buffer)(nil)

// asyncFakeBroker is fakeBroker with parallel callback dispatch: every
// delivery hops onto its own goroutine, mirroring the transport patterns
// nats.go supports today (multiple readers per subscription) and expects to
// support tomorrow (queue-group fan-out workers). The synchronous
// fakeBroker cannot detect a WaitGroup race in NATSBus.Subscribe because
// the callback returns before Publish returns and cancel never observes an
// in-flight Add. This broker does.
type asyncFakeBroker struct {
	mu     sync.Mutex
	subs   []*asyncFakeSub
	closed bool
	wg     sync.WaitGroup // tracks in-flight dispatch goroutines
}

func newAsyncFakeBroker() *asyncFakeBroker { return &asyncFakeBroker{} }

type asyncFakeSub struct {
	broker  *asyncFakeBroker
	subject string
	queue   string
	cb      nats.MsgHandler
	active  bool
}

func (s *asyncFakeSub) Unsubscribe() error {
	s.broker.mu.Lock()
	s.active = false
	s.broker.mu.Unlock()
	return nil
}

func (b *asyncFakeBroker) Publish(subject string, data []byte) error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	var targets []*asyncFakeSub
	for _, s := range b.subs {
		if s.active && subjectMatches(s.subject, subject) {
			targets = append(targets, s)
		}
	}
	b.mu.Unlock()
	for _, s := range targets {
		b.wg.Add(1)
		go func(s *asyncFakeSub, subj string, d []byte) {
			defer b.wg.Done()
			s.cb(&nats.Msg{Subject: subj, Data: d})
		}(s, subject, data)
	}
	return nil
}

func (b *asyncFakeBroker) QueueSubscribe(subject, queue string, cb nats.MsgHandler) (natsSubscription, error) {
	s := &asyncFakeSub{
		broker:  b,
		subject: subject,
		queue:   queue,
		cb:      cb,
		active:  true,
	}
	b.mu.Lock()
	b.subs = append(b.subs, s)
	b.mu.Unlock()
	return s, nil
}

// stop drains the in-flight dispatch goroutines so a test teardown does not
// leak them past the test's own goroutine leak checker.
func (b *asyncFakeBroker) stop() {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
	b.wg.Wait()
}

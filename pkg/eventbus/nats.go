package eventbus

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// DefaultSubjectPrefix is the NATS subject namespace the bus publishes under.
// It is deliberately distinct from the run WORK queue's stream
// (ITERION_RUNS): the notification bus and the work queue have different
// delivery semantics, so they never share a subject tree. Override via
// NATSOptions.SubjectPrefix.
const DefaultSubjectPrefix = "iterion.events"

// natsConn / natsSubscription are the minimal slice of the NATS client the bus
// needs. Narrowing to interfaces lets the bus logic (subject computation,
// envelope encode/decode, client-side Matcher filtering, queue-group fan-out)
// be unit-tested against a fake broker without vendoring a NATS server. A thin
// adapter (realNATSConn) makes *nats.Conn satisfy natsConn — the interface
// can't name *nats.Conn's concrete *nats.Subscription return directly (Go
// requires exact return types for interface satisfaction), so the adapter
// widens it to natsSubscription.
type natsConn interface {
	Publish(subject string, data []byte) error
	QueueSubscribe(subject, queue string, cb nats.MsgHandler) (natsSubscription, error)
}

type natsSubscription interface {
	Unsubscribe() error
}

type realNATSConn struct{ nc *nats.Conn }

func (r realNATSConn) Publish(subject string, data []byte) error {
	return r.nc.Publish(subject, data)
}

func (r realNATSConn) QueueSubscribe(subject, queue string, cb nats.MsgHandler) (natsSubscription, error) {
	return r.nc.QueueSubscribe(subject, queue, cb)
}

// NATSBus is the cloud multi-replica Bus: a Core NATS (not JetStream) subject
// fan-out. Every trigger.Event is published to <prefix>.<source> and delivered
// to subscribers via a QUEUE GROUP keyed on the subscriber name, so exactly
// one replica's evaluator handles each event — the multi-host equivalent of
// InProcBus's single-worker-per-subscriber semantics. Whichever replica holds
// the (identical, store-backed) subscription set reacts; the others don't
// double-launch.
//
// Core NATS, not JetStream, is deliberate: this is the LOSSY notification bus
// (bus.go), at-most-once with the producer's reconciliation path (dispatcher
// poll) as the backstop — not the exactly-once locked work queue (pkg/queue,
// which is the JetStream one). A dropped notification is recovered by the
// poll, so persistence + acks would be cost without benefit here.
//
// Filtering mirrors InProcBus exactly: a subscriber consumes the whole event
// subject tree (<prefix>.>) and applies trigger.Matcher in-process, so the bus
// needs no per-field subject encoding and a subscription's filter can be
// arbitrarily rich.
type NATSBus struct {
	nc     natsConn
	prefix string
	logger *iterlog.Logger

	mu   sync.Mutex
	subs map[*natsSub]struct{}

	cancelBudget time.Duration // 0 → DefaultSubscribeCancelBudget; test seam only.
}

type natsSub struct {
	sub natsSubscription
	// ctx is cancelled by the subscriber's cancel func; an in-flight handler
	// observing it exits. The transport keeps invoking the callback on its own
	// goroutine until Unsubscribe returns, so a per-subscription context is the
	// only way to signal them.
	ctx    context.Context
	cancel context.CancelFunc
	// mu/closed/inFlight together implement the "safe WaitGroup" pattern:
	// cancel sets closed=true under mu, then Wait; a callback dispatched
	// AFTER cancel sees closed and returns without touching inFlight. Without
	// this gate a transport dispatching callbacks in parallel (async nats.go
	// subs, a queue-group fan-out worker, an async broker in tests) can race
	// the cancel — cb starts, cancel calls Wait() (counter zero, returns),
	// cb then calls Add(1) → "WaitGroup is reused before previous Wait has
	// returned" panic. The adversarial round on this PR reproduced it 1/100
	// on an async broker; see bus_conformance_test.go's asyncBroker row.
	mu       sync.Mutex
	closed   bool
	inFlight sync.WaitGroup
}

// NATSOptions configures a NATSBus.
type NATSOptions struct {
	// SubjectPrefix overrides DefaultSubjectPrefix. Trailing dots are trimmed.
	SubjectPrefix string
	// Logger receives dropped-event / decode-error warnings. May be nil.
	Logger *iterlog.Logger
}

// subscribeCancelBudget bounds the wait for in-flight NATS callbacks to
// return after cancel is called; a handler that ignores its context cannot
// hold shutdown past this window. Paired with pkg/server's background join
// budget (#1257) — see DefaultSubscribeCancelBudget.
func (b *NATSBus) subscribeCancelBudget() time.Duration {
	if b.cancelBudget > 0 {
		return b.cancelBudget
	}
	return DefaultSubscribeCancelBudget
}

// NewNATSBus builds a NATSBus over an established NATS connection. The caller
// owns the connection lifecycle (the bus never closes it); callers typically
// pass the same low-level *nats.Conn the work queue uses (natsq.Conn.NATS()),
// since the bus and the queue address disjoint subject trees on one link.
func NewNATSBus(nc *nats.Conn, opts NATSOptions) (*NATSBus, error) {
	if nc == nil {
		return nil, fmt.Errorf("eventbus: NewNATSBus: nil connection")
	}
	return newNATSBus(realNATSConn{nc: nc}, opts)
}

// newNATSBus is the internal constructor over the natsConn seam (real adapter
// in production, fake broker in tests).
func newNATSBus(nc natsConn, opts NATSOptions) (*NATSBus, error) {
	if nc == nil {
		return nil, fmt.Errorf("eventbus: newNATSBus: nil connection")
	}
	prefix := strings.TrimRight(strings.TrimSpace(opts.SubjectPrefix), ".")
	if prefix == "" {
		prefix = DefaultSubjectPrefix
	}
	return &NATSBus{
		nc:     nc,
		prefix: prefix,
		logger: opts.Logger,
		subs:   make(map[*natsSub]struct{}),
	}, nil
}

// subjectFor returns the publish subject for an event. Events carry a Source;
// scoping the subject by source keeps the tree legible in `nats sub` and lets
// a future subscriber narrow at the broker if needed, while today's
// subscribers consume the whole tree and filter in-process. A source with a
// subject-special character (there are none in the closed Source set, but be
// defensive) collapses to a safe leaf so the subject stays two-token.
func (b *NATSBus) subjectFor(ev trigger.Event) string {
	src := string(ev.Source)
	if src == "" || strings.ContainsAny(src, ". *>") {
		src = "unknown"
	}
	return b.prefix + "." + src
}

// Publish encodes ev as JSON and fires it to <prefix>.<source>. Core NATS
// publish is non-blocking and never waits on subscribers — the lossy-fan-out
// contract. A publish to a subject with no subscribers is a no-op, not an
// error.
func (b *NATSBus) Publish(_ context.Context, ev trigger.Event) error {
	data, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("eventbus: marshal event %s/%s: %w", ev.Source, ev.Kind, err)
	}
	if err := b.nc.Publish(b.subjectFor(ev), data); err != nil {
		return fmt.Errorf("eventbus: publish %s/%s: %w", ev.Source, ev.Kind, err)
	}
	return nil
}

// Subscribe registers h under a NATS queue group named `name`, listening on
// the whole event subject tree (<prefix>.>). The queue group makes NATS
// deliver each event to exactly one member across all replicas sharing the
// name — so N server pods with the same evaluator subscription process each
// event once, not N times. Events are decoded and passed through filter before
// h, matching InProcBus.
//
// The returned cancel first signals the handler through the per-subscription
// context, then unsubscribes so the transport delivers no further messages,
// and finally WAITS for any in-flight callback to return within
// DefaultSubscribeCancelBudget — a handler ignoring its ctx is left behind
// with a warning naming the subscriber, exactly as InProcBus does. Without
// that wait a pod's SIGTERM would cut a callback mid-store-write (#1343).
// See Bus.Subscribe for the ctx contract on the returned cancel.
// Idempotent.
func (b *NATSBus) Subscribe(name string, filter trigger.Matcher, h Handler) (func(context.Context), error) {
	if name == "" {
		return nil, fmt.Errorf("eventbus: NATSBus.Subscribe: empty name (used as the queue group)")
	}
	subCtx, subCancel := context.WithCancel(context.Background())
	ns := &natsSub{ctx: subCtx, cancel: subCancel}
	cb := func(msg *nats.Msg) {
		// Gate WaitGroup.Add under mu / closed so a callback dispatched by
		// the transport AFTER cancel called Wait cannot race the WG's
		// counter — see natsSub.mu comment. Once closed, we return without
		// touching the store; the cancelled context alone is not enough
		// because sync.WaitGroup.Add after a returned Wait panics under
		// -race, and a "no I/O when closed" fast path here plus a Wait()
		// that sees only Add()s made under mu is the pattern the standard
		// library documents.
		ns.mu.Lock()
		if ns.closed {
			ns.mu.Unlock()
			return
		}
		ns.inFlight.Add(1)
		ns.mu.Unlock()
		defer ns.inFlight.Done()
		var ev trigger.Event
		if err := json.Unmarshal(msg.Data, &ev); err != nil {
			if b.logger != nil {
				b.logger.Warn("eventbus: subscriber %q dropping undecodable event on %s: %v", name, msg.Subject, err)
			}
			return
		}
		if !filter.Match(ev) {
			return
		}
		// The per-subscription context is the handler's cancel signal, so a
		// handler doing store/LLM I/O observes shutdown teardown; a handler
		// managing its own deadline layers WithTimeout on top. Errors are
		// logged and swallowed (the reconciliation poll is the safety net),
		// mirroring InProcBus.worker.
		if err := deliver(ns.ctx, h, ev); err != nil && b.logger != nil {
			b.logger.Warn("eventbus: subscriber %q handler error on %s/%s: %v", name, ev.Source, ev.Kind, err)
		}
	}
	sub, err := b.nc.QueueSubscribe(b.prefix+".>", name, cb)
	if err != nil {
		subCancel()
		return nil, fmt.Errorf("eventbus: NATSBus.Subscribe %q: %w", name, err)
	}
	ns.sub = sub
	b.mu.Lock()
	b.subs[ns] = struct{}{}
	b.mu.Unlock()

	var once sync.Once
	cancel := func(ctx context.Context) {
		once.Do(func() {
			// One deadline covers Unsubscribe AND the in-flight wait.
			// Unsubscribe on a real nats.Conn typically returns fast, but
			// its "typically" is not a guarantee — a wire flush during a
			// broker outage can sit here — so we bound it under the same
			// budget as Wait rather than pinning "never blocks" with a
			// separate assumption (#1477's Q2).
			waitCtx, waitCancel := b.waitCtx(ctx)
			defer waitCancel()

			// Close the WaitGroup gate BEFORE Unsubscribe: past this point
			// any newly-dispatched callback returns without Add-ing, so
			// Wait() below observes only Adds that happened before the
			// gate closed. The gate is what makes the WG safe under a
			// transport that dispatches callbacks in parallel.
			ns.mu.Lock()
			ns.closed = true
			ns.mu.Unlock()
			// Signal the handler through its context BEFORE unsubscribing
			// so an in-flight callback observes teardown; Unsubscribing
			// first would let a running callback finish its store write
			// with no signal at all.
			ns.cancel()
			// Unsubscribe on its own goroutine so a slow transport wire
			// doesn't eat the whole budget before Wait even starts. If
			// unsubscribe hangs past waitCtx, we log and move on — the
			// handler-context cancel already stopped delivery from being
			// consumed, and the transport tears down when the connection
			// closes.
			unsubDone := make(chan struct{})
			go func() {
				defer close(unsubDone)
				if ns.sub != nil {
					_ = ns.sub.Unsubscribe()
				}
			}()
			waitDone := make(chan struct{})
			go func() {
				defer close(waitDone)
				ns.inFlight.Wait()
			}()
			// One select over BOTH the in-flight WG AND unsubscribe: the
			// budget is shared. A handler unwinding fast + unsub blocking
			// is the same wall clock as a fast unsub + slow handler.
			for pending := 2; pending > 0; {
				select {
				case <-waitDone:
					pending--
					waitDone = nil
				case <-unsubDone:
					pending--
					unsubDone = nil
				case <-waitCtx.Done():
					pending = 0
				}
			}
			// The overrun is defined by "did the operation actually
			// complete", not by "which select arm won". When the shared
			// budget is already spent (subscribers 2..N under one
			// joinCtx), the ctx-arm and a channel-close arm can be ready
			// on the same tick and Go's select picks a random ready arm
			// — a clean shutdown would be named as an overrun ~half the
			// time. Give each still-open channel one more short window:
			// enough for the spawning goroutine to run when there is
			// nothing in flight, small enough not to compose meaningfully
			// across N subscribers. Only warn when the channel is
			// genuinely still open. #1477 R6aac0e.
			overran := false
			if waitDone != nil {
				select {
				case <-waitDone:
				case <-time.After(overrunPostCheckWindow):
					overran = true
				}
			}
			if !overran && unsubDone != nil {
				select {
				case <-unsubDone:
				case <-time.After(overrunPostCheckWindow):
					overran = true
				}
			}
			if overran && b.logger != nil {
				b.logger.Warn("eventbus: subscriber %q did not settle within cancel budget; its last write falls outside the grace period", name)
			}
			b.mu.Lock()
			delete(b.subs, ns)
			b.mu.Unlock()
		})
	}
	return cancel, nil
}

// waitCtx derives the ctx that bounds Unsubscribe + WG.Wait on cancel.
// When the caller passes a joinCtx (shared deadline) we honour it verbatim
// so N subscriptions share ONE budget. Otherwise fall back to a fresh
// WithTimeout on the default budget so a standalone cancel stays bounded.
func (b *NATSBus) waitCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		return context.WithTimeout(context.Background(), b.subscribeCancelBudget())
	}
	if _, ok := ctx.Deadline(); ok {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, b.subscribeCancelBudget())
}

var _ Bus = (*NATSBus)(nil)

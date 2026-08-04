package eventbus

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

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
}

type natsSub struct {
	sub natsSubscription
}

// NATSOptions configures a NATSBus.
type NATSOptions struct {
	// SubjectPrefix overrides DefaultSubjectPrefix. Trailing dots are trimmed.
	SubjectPrefix string
	// Logger receives dropped-event / decode-error warnings. May be nil.
	Logger *iterlog.Logger
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
// h, matching InProcBus. The returned cancel unsubscribes (idempotent).
func (b *NATSBus) Subscribe(name string, filter trigger.Matcher, h Handler) (func(), error) {
	if name == "" {
		return nil, fmt.Errorf("eventbus: NATSBus.Subscribe: empty name (used as the queue group)")
	}
	cb := func(msg *nats.Msg) {
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
		// A fresh context per delivery: the bus has no run-scoped ctx, and a
		// handler doing store/LLM I/O manages its own deadline. Errors are
		// logged and swallowed (the reconciliation poll is the safety net),
		// mirroring InProcBus.worker.
		if err := deliver(context.Background(), h, ev); err != nil && b.logger != nil {
			b.logger.Warn("eventbus: subscriber %q handler error on %s/%s: %v", name, ev.Source, ev.Kind, err)
		}
	}
	sub, err := b.nc.QueueSubscribe(b.prefix+".>", name, cb)
	if err != nil {
		return nil, fmt.Errorf("eventbus: NATSBus.Subscribe %q: %w", name, err)
	}
	ns := &natsSub{sub: sub}
	b.mu.Lock()
	b.subs[ns] = struct{}{}
	b.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			if ns.sub != nil {
				_ = ns.sub.Unsubscribe()
			}
			b.mu.Lock()
			delete(b.subs, ns)
			b.mu.Unlock()
		})
	}
	return cancel, nil
}

var _ Bus = (*NATSBus)(nil)

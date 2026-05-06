// Package nats wraps the NATS / JetStream / KV layer for iterion's
// cloud queue. It owns three concrete responsibilities:
//
//  1. Publisher — ensure the JetStream stream exists, then publish a
//     queue.RunMessage onto `iterion.queue.runs` with `Nats-Msg-Id =
//     run_id` so JetStream itself dedups republishes.
//  2. Consumer  — subscribe to the durable `iterion-runners`
//     pull-consumer with AckWait=5min and MaxAckPending=1 so a single
//     runner pod can only have one in-flight run at a time.
//  3. KV        — distributed lease bucket `iterion-run-locks` keyed
//     on run_id with TTL=60s; the runner refreshes the lease via the
//     CAS write while it owns the run (T-26 bridges this to
//     MongoRunStore.LockRun).
//
// Subjects + retention policies are pinned in cloud-ready plan §C.2;
// every constant in this file mirrors the table verbatim so changes
// are obvious in `git diff`.
package nats

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
)

// Plan §C.2 — every named subject / stream / bucket lives here.
const (
	StreamRuns       = "ITERION_RUNS"
	StreamRunsDLQ    = "ITERION_RUNS_DLQ"
	SubjectRuns      = "iterion.queue.runs"
	SubjectRunsDLQ   = "iterion.queue.runs.dlq"
	SubjectCancelFmt = "iterion.cancel.%s" // %s = run_id
	SubjectHeartFmt  = "iterion.heartbeat.%s"
	KVRunLocks       = "iterion-run-locks"
	ConsumerRunners  = "iterion-runners"
)

// Default retention values from plan §C.2.
const (
	DefaultStreamMaxAge   = 24 * time.Hour
	DefaultStreamMaxRetry = 3
	DefaultDLQMaxAge      = 7 * 24 * time.Hour
	DefaultLockTTL        = 60 * time.Second
	DefaultAckWait        = 5 * time.Minute
)

// Config carries the connection settings for the cloud queue.
type Config struct {
	URL        string        // nats://host:port — required
	StreamName string        // default StreamRuns
	DLQStream  string        // default StreamRunsDLQ
	KVBucket   string        // default KVRunLocks
	MaxAge     time.Duration // default 24h
	DLQMaxAge  time.Duration // default 7d
	MaxDeliver int           // default 3
	AckWait    time.Duration // default 5min
	LockTTL    time.Duration // default 60s
	Logger     *iterlog.Logger
}

// Conn is the wired NATS layer. The publisher + consumer both consume
// it; the runner takes a single Conn at boot and shares it between
// the consumer goroutine and any cancel-subject subscribers.
type Conn struct {
	nc     *nats.Conn
	js     jetstream.JetStream
	kv     jetstream.KeyValue
	cfg    Config
	logger *iterlog.Logger
}

// Connect opens the NATS connection, pins the stream + DLQ + KV
// bucket idempotently, and returns the live wrapper. EnsureSchema is
// called automatically — callers don't need a separate bootstrap step.
func Connect(ctx context.Context, cfg Config) (*Conn, error) {
	if cfg.URL == "" {
		return nil, fmt.Errorf("queue/nats: URL is required")
	}
	cfg = applyDefaults(cfg)

	nc, err := nats.Connect(cfg.URL,
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2*time.Second),
		nats.Name("iterion"),
	)
	if err != nil {
		return nil, fmt.Errorf("queue/nats: connect: %w", err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("queue/nats: jetstream: %w", err)
	}

	c := &Conn{nc: nc, js: js, cfg: cfg, logger: cfg.Logger}
	if err := c.EnsureSchema(ctx); err != nil {
		nc.Close()
		return nil, err
	}
	return c, nil
}

// Close releases the NATS connection. Safe to call multiple times.
func (c *Conn) Close() {
	if c == nil || c.nc == nil {
		return
	}
	c.nc.Close()
}

// NATS exposes the underlying connection for callers that need raw
// pub/sub on the cancel + heartbeat subjects (the runner subscribes
// to `iterion.cancel.<run_id>` directly via Core NATS — no JetStream
// needed for transient signalling).
func (c *Conn) NATS() *nats.Conn { return c.nc }

// JetStream exposes the JetStream interface for advanced consumers
// (paginated lookups, custom consumer geometry).
func (c *Conn) JetStream() jetstream.JetStream { return c.js }

// KV exposes the run-lock KV bucket so MongoRunStore.LockRun (T-26)
// can layer a CAS lease on top of it without re-resolving the bucket.
func (c *Conn) KV() jetstream.KeyValue { return c.kv }

// EnsureSchema creates the JetStream streams + KV bucket idempotently.
// Designed to run on every server / runner boot so the topology is
// self-healing — if an operator deletes a stream by mistake the next
// pod start brings it back.
func (c *Conn) EnsureSchema(ctx context.Context) error {
	if _, err := c.js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:       c.cfg.StreamName,
		Subjects:   []string{SubjectRuns},
		Retention:  jetstream.WorkQueuePolicy,
		MaxAge:     c.cfg.MaxAge,
		Storage:    jetstream.FileStorage,
		Duplicates: 5 * time.Minute, // window for Nats-Msg-Id dedup
	}); err != nil {
		return fmt.Errorf("queue/nats: stream %s: %w", c.cfg.StreamName, err)
	}

	if _, err := c.js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:      c.cfg.DLQStream,
		Subjects:  []string{SubjectRunsDLQ},
		Retention: jetstream.LimitsPolicy,
		MaxAge:    c.cfg.DLQMaxAge,
		Storage:   jetstream.FileStorage,
	}); err != nil {
		return fmt.Errorf("queue/nats: stream %s: %w", c.cfg.DLQStream, err)
	}

	kv, err := c.js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:  c.cfg.KVBucket,
		TTL:     c.cfg.LockTTL,
		History: 1,
	})
	if err != nil {
		return fmt.Errorf("queue/nats: kv %s: %w", c.cfg.KVBucket, err)
	}
	c.kv = kv

	return nil
}

// PublishRun submits a RunMessage onto the iterion.queue.runs subject.
// The Nats-Msg-Id header is set to RunID so that re-publishes within
// the dedup window are silently absorbed by JetStream — that is what
// makes the editor-side launch handler safely retryable.
func (c *Conn) PublishRun(ctx context.Context, msg *queue.RunMessage) (*jetstream.PubAck, error) {
	if err := msg.Validate(); err != nil {
		return nil, fmt.Errorf("queue/nats: invalid RunMessage: %w", err)
	}
	if msg.PublishedAtRFC == "" {
		msg.PublishedAtRFC = time.Now().UTC().Format(time.RFC3339Nano)
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("queue/nats: marshal RunMessage: %w", err)
	}

	headers := nats.Header{}
	headers.Set("Nats-Msg-Id", msg.RunID)
	headers.Set("iterion-schema-version", fmt.Sprintf("%d", msg.V))

	// Plan §F (T-41): inject W3C traceparent + tracestate from the
	// caller's ctx so the runner-side span inherits the parent. The
	// queue.TraceContext mirror in the body is also populated for
	// callers who decode the payload before the headers (defence in
	// depth). EnsureDefaultPropagator wires propagation.TraceContext
	// when nothing else has set a global propagator yet.
	EnsureDefaultPropagator()
	injectTrace(ctx, msg, headers)
	// Convenience aliases — runners that don't want to drag in OTel
	// can still read these directly. Kept after Inject so the W3C
	// header takes precedence on conflict.
	if msg.Trace.TraceID != "" {
		headers.Set("iterion-trace-id", msg.Trace.TraceID)
		headers.Set("iterion-span-id", msg.Trace.SpanID)
	}

	return c.js.PublishMsg(ctx, &nats.Msg{
		Subject: SubjectRuns,
		Data:    body,
		Header:  headers,
	})
}

// CancelRun fires the transient `iterion.cancel.<run_id>` Core NATS
// subject. The runner subscribes to its in-flight run's subject for
// the duration of execution; an unsubscribed cancel is a silent
// no-op, which matches the expectation that a queued (not yet
// picked up) run is cancelled by deleting the stream message instead.
func (c *Conn) CancelRun(runID string) error {
	if runID == "" {
		return fmt.Errorf("queue/nats: cancel requires runID")
	}
	return c.nc.Publish(fmt.Sprintf(SubjectCancelFmt, runID), nil)
}

// SubscribeCancel installs a one-shot Core NATS subscriber on
// `iterion.cancel.<run_id>` and invokes onCancel when a message
// arrives. The runner uses this for the duration of a single run;
// the returned subscription is valid until ctx is cancelled.
func (c *Conn) SubscribeCancel(ctx context.Context, runID string, onCancel func()) (*nats.Subscription, error) {
	sub, err := c.nc.Subscribe(fmt.Sprintf(SubjectCancelFmt, runID), func(_ *nats.Msg) {
		onCancel()
	})
	if err != nil {
		return nil, fmt.Errorf("queue/nats: subscribe cancel %s: %w", runID, err)
	}
	go func() {
		<-ctx.Done()
		_ = sub.Unsubscribe()
	}()
	return sub, nil
}

// Consumer wraps the durable JetStream pull consumer the runner uses
// to drain the queue. It is created lazily so a process that only
// publishes (the server) doesn't pay the cost of consumer setup.
type Consumer struct {
	cons   jetstream.Consumer
	cfg    Config
	logger *iterlog.Logger
}

// NewConsumer creates / updates the durable consumer on the runs
// stream. AckWait + MaxAckPending=1 enforce one-in-flight-per-runner
// without coordinating outside JetStream itself; OptStartPolicy=All
// means a fresh consumer replays from the earliest pending message
// (matters when a stale pod is replaced).
func (c *Conn) NewConsumer(ctx context.Context) (*Consumer, error) {
	cons, err := c.js.CreateOrUpdateConsumer(ctx, c.cfg.StreamName, jetstream.ConsumerConfig{
		Durable:       ConsumerRunners,
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       c.cfg.AckWait,
		MaxAckPending: 1,
		MaxDeliver:    c.cfg.MaxDeliver,
		DeliverPolicy: jetstream.DeliverAllPolicy,
		FilterSubject: SubjectRuns,
	})
	if err != nil {
		return nil, fmt.Errorf("queue/nats: consumer: %w", err)
	}
	return &Consumer{cons: cons, cfg: c.cfg, logger: c.logger}, nil
}

// Fetch pulls a single ready message, blocking up to wait. Returns
// (nil, ErrNoMessage) when the wait elapses without a delivery.
func (cons *Consumer) Fetch(ctx context.Context, wait time.Duration) (*Delivery, error) {
	batch, err := cons.cons.FetchNoWait(1)
	if err != nil {
		return nil, fmt.Errorf("queue/nats: fetch: %w", err)
	}
	for msg := range batch.Messages() {
		return wrap(msg), nil
	}
	if err := batch.Error(); err != nil {
		return nil, fmt.Errorf("queue/nats: fetch error: %w", err)
	}

	// Nothing was immediately ready — fall through to a blocking
	// fetch with the caller's wait so we don't busy-loop.
	if wait <= 0 {
		return nil, ErrNoMessage
	}
	timeoutCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	batch2, err := cons.cons.Fetch(1, jetstream.FetchMaxWait(wait))
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(timeoutCtx.Err(), context.DeadlineExceeded) {
			return nil, ErrNoMessage
		}
		return nil, fmt.Errorf("queue/nats: fetch wait: %w", err)
	}
	for msg := range batch2.Messages() {
		return wrap(msg), nil
	}
	return nil, ErrNoMessage
}

// ErrNoMessage signals that Fetch elapsed its wait without a message.
// Callers loop on this to keep polling without treating it as a
// failure (the runner does).
var ErrNoMessage = errors.New("queue/nats: no message ready")

// Delivery bundles a JetStream message with helpers to ack / nak /
// term and to decode the body into a queue.RunMessage. Wrapping the
// raw jetstream.Msg keeps the consumer-facing surface narrow.
type Delivery struct {
	raw jetstream.Msg
}

func wrap(m jetstream.Msg) *Delivery { return &Delivery{raw: m} }

// Decode unmarshals the body and validates it.
func (d *Delivery) Decode() (*queue.RunMessage, error) {
	var msg queue.RunMessage
	if err := json.Unmarshal(d.raw.Data(), &msg); err != nil {
		return nil, fmt.Errorf("queue/nats: decode: %w", err)
	}
	if err := msg.Validate(); err != nil {
		return nil, err
	}
	return &msg, nil
}

// Ack marks the delivery as successfully processed. Stops redelivery.
func (d *Delivery) Ack() error { return d.raw.Ack() }

// Nak schedules a redelivery (after AckWait expires or sooner).
func (d *Delivery) Nak() error { return d.raw.Nak() }

// Term tells JetStream to permanently drop the message — used after
// MaxDeliver attempts when the runner publishes a DLQ copy itself.
func (d *Delivery) Term() error { return d.raw.Term() }

// InProgress tells JetStream we're still working — extends AckWait
// so a long-running run isn't redelivered to a sibling runner.
func (d *Delivery) InProgress() error { return d.raw.InProgress() }

// Subject returns the original delivery subject.
func (d *Delivery) Subject() string { return d.raw.Subject() }

// Headers returns the message headers.
func (d *Delivery) Headers() nats.Header { return d.raw.Headers() }

// PropagateTraceTo extracts the W3C traceparent header from this
// delivery and returns a child context so the consumer's runtime
// span inherits the publisher's trace. When no header is present
// (legacy publisher, local-mode tests) the input ctx is returned
// unchanged. Plan §F (T-41).
func (d *Delivery) PropagateTraceTo(ctx context.Context) context.Context {
	EnsureDefaultPropagator()
	return extractTrace(ctx, d.Headers())
}

func applyDefaults(c Config) Config {
	if c.StreamName == "" {
		c.StreamName = StreamRuns
	}
	if c.DLQStream == "" {
		c.DLQStream = StreamRunsDLQ
	}
	if c.KVBucket == "" {
		c.KVBucket = KVRunLocks
	}
	if c.MaxAge == 0 {
		c.MaxAge = DefaultStreamMaxAge
	}
	if c.DLQMaxAge == 0 {
		c.DLQMaxAge = DefaultDLQMaxAge
	}
	if c.MaxDeliver == 0 {
		c.MaxDeliver = DefaultStreamMaxRetry
	}
	if c.AckWait == 0 {
		c.AckWait = DefaultAckWait
	}
	if c.LockTTL == 0 {
		c.LockTTL = DefaultLockTTL
	}
	if c.Logger == nil {
		c.Logger = iterlog.New(iterlog.LevelInfo, nil)
	}
	return c
}

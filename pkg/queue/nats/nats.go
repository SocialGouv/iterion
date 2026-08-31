// Package nats wraps the NATS / JetStream / KV layer for iterion's
// cloud queue. It owns three concrete responsibilities:
//
//  1. Publisher — ensure the JetStream stream exists, then publish a
//     queue.RunMessage onto `iterion.queue.runs` with a stable
//     `Nats-Msg-Id`: run_id for launches, and a per-attempt salt for resumes.
//  2. Consumer  — subscribe to the SHARED durable `iterion-runners`
//     pull-consumer with AckWait=10min and MaxAckPending=DefaultMaxAckPending
//     as a fleet-wide in-flight ceiling; each pod holds one in-flight run
//     via its serial fetch loop, so pod count (KEDA) is the real capacity.
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

	// Trigger event bus (pkg/eventbus NATSBus). A fan-out NOTIFICATION
	// stream, kept deliberately separate from the run WORK queue above:
	// events are at-least-once + lossy under back-pressure, runs are
	// exactly-once + KV-locked. SubjectEventsFmt is "%s = source, %s =
	// tenant" so a consumer can filter by either with a subject wildcard.
	StreamEvents     = "ITERION_EVENTS"
	SubjectEventsAll = "iterion.events.>"
	SubjectEventsFmt = "iterion.events.%s.%s" // source, tenant
)

// Default stream topology and retention values from plan §C.2.
const (
	DefaultStreamMaxAge   = 24 * time.Hour
	DefaultStreamReplicas = 1
	// DefaultStreamMaxRetry is the consumer's MaxDeliver — how many times a
	// message is redelivered before it parks in the DLQ. A run in flight
	// renews its ack every HeartbeatInterval (InProgress), so this budget is
	// only consumed by a run that CANNOT make progress: a genuine crash, OR a
	// message that waited un-claimed in a deep queue past AckWait because every
	// runner was busy (the burst case — 5 runs onto 3 static pods). 8 (× the
	// 10-minute AckWait ≈ 80 min) absorbs a bursty backlog without giving up,
	// while still parking a truly stuck run. The real capacity lever is KEDA
	// autoscaling on queue depth (charts runner.keda); this is the resilience
	// floor for when the pool is momentarily saturated.
	DefaultStreamMaxRetry = 8
	DefaultDLQMaxAge      = 7 * 24 * time.Hour
	DefaultLockTTL        = 60 * time.Second
	// DefaultAckWait is the per-delivery ack deadline. Longer than a single
	// heartbeat interval so one missed heartbeat doesn't trigger a spurious
	// redelivery; 10 min also covers a cold devbox/Nix first-run before the
	// first heartbeat lands.
	DefaultAckWait = 10 * time.Minute
	// DefaultMaxAckPending caps how many runs can be in-flight (delivered but
	// unacked) at once across the whole fleet on the shared durable pull
	// consumer. It MUST be ≥ the max runner-pod count KEDA scales to, or it
	// silently re-caps global parallelism below the pool size (the historic
	// value of 1 pinned the entire fleet to a single concurrent run — every
	// scaled-up pod sat idle). Set generously: actual parallelism is bounded
	// by the number of pods calling Fetch (each holds one in-flight run via
	// its serial loop), so a high ceiling just removes the artificial cap and
	// lets KEDA autoscaling be the real capacity lever, as intended.
	DefaultMaxAckPending = 256

	// SchemaMismatchNakDelay is the redelivery delay a runner applies when it
	// rejects a message whose schema version it does not recognise (rolling
	// upgrade in flight). A bare Nak is immediate: with MaxDeliver=8, a single
	// stale pod can burn the whole redelivery budget in seconds — long before
	// an upgraded runner is scheduled to take the message — and JetStream
	// then drops it (issue #481). 30s stretches the 8-delivery budget over
	// ~4 minutes of wall clock, which covers a rolling restart of the runner
	// deployment; if the fleet still hasn't caught up, the exhausted message
	// is parked on the DLQ with an actionable run status (recoverable via
	// /api/admin/dlq) rather than silently dropped.
	SchemaMismatchNakDelay = 30 * time.Second
)

// Config carries the connection settings for the cloud queue.
type Config struct {
	URL            string        // nats://host:port — required
	StreamName     string        // default StreamRuns
	DLQStream      string        // default StreamRunsDLQ
	KVBucket       string        // default KVRunLocks
	StreamReplicas int           // default 1
	ConsumerName   string        // default ConsumerRunners
	MaxAge         time.Duration // default 24h
	DLQMaxAge      time.Duration // default 7d
	MaxDeliver     int           // default DefaultStreamMaxRetry (8)
	AckWait        time.Duration // default DefaultAckWait (10m)
	// SchemaMismatchDelay is the delayed-Nak interval used by runners during
	// mixed-schema rollouts. It also contributes to RedeliveryWindow so the
	// orphan sweeper never races a legitimately bouncing message.
	SchemaMismatchDelay time.Duration // default SchemaMismatchNakDelay (30s)
	// MaxAckPending caps fleet-wide in-flight (delivered-unacked) runs on the
	// shared consumer; 0 → DefaultMaxAckPending. Keep ≥ max runner pods.
	MaxAckPending int
	LockTTL       time.Duration // default 60s
	MaxPayload    int           // default 0 → use server's negotiated MaxPayload
	Logger        *iterlog.Logger
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

// RedeliveryWindow is the worst-case time a healthy queued message can
// spend bouncing through redeliveries before parking in the DLQ. Ordinary
// retries are bounded by AckWait; schema mismatches use an explicit delayed
// Nak, which an operator may configure above AckWait. The server's orphan
// sweeper derives its queued-staleness cutoff from the larger interval so it
// never flips a message that is still legitimately waiting for redelivery.
func (c *Conn) RedeliveryWindow() time.Duration {
	interval := c.cfg.AckWait
	if c.cfg.SchemaMismatchDelay > interval {
		interval = c.cfg.SchemaMismatchDelay
	}
	return time.Duration(c.cfg.MaxDeliver) * interval
}

// Connect opens the NATS connection, pins the stream + DLQ + KV
// bucket idempotently, and returns the live wrapper. EnsureSchema is
// called automatically — callers don't need a separate bootstrap step.
func Connect(ctx context.Context, cfg Config) (*Conn, error) {
	if cfg.URL == "" {
		return nil, fmt.Errorf("queue/nats: URL is required")
	}
	cfg = applyDefaults(cfg)
	if cfg.StreamReplicas < 1 {
		return nil, fmt.Errorf("queue/nats: stream replicas %d invalid (want >= 1)", cfg.StreamReplicas)
	}

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
	// Install the W3C TraceContext propagator once per process so
	// PublishRun + Delivery.PropagateTraceTo can stay in the hot path
	// (plan §F T-41).
	EnsureDefaultPropagator()
	return c, nil
}

// Close releases the NATS connection. Safe to call multiple times.
func (c *Conn) Close() {
	if c == nil || c.nc == nil {
		return
	}
	c.nc.Close()
}

// Ping issues a round-trip ping on the NATS connection. Used by the
// server's /readyz handler. Returns the wrapped client error on failure
// (typically a timeout when the broker is unreachable).
func (c *Conn) Ping(ctx context.Context) error {
	if c == nil || c.nc == nil {
		return fmt.Errorf("queue/nats: connection not initialised")
	}
	if ctx == nil {
		return fmt.Errorf("queue/nats: ping: nil context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !c.nc.IsConnected() {
		return fmt.Errorf("queue/nats: not connected (status=%s)", c.nc.Status())
	}
	// FlushWithContext is the context-aware form of a NATS ping/flush
	// round-trip. Preserve the previous RTT() upper bound (10s) when the
	// caller supplies an undeadlined context, but honour shorter caller
	// deadlines/cancellation instead of always blocking for the library
	// default.
	flushCtx := ctx
	var cancel context.CancelFunc
	if _, ok := flushCtx.Deadline(); !ok {
		flushCtx, cancel = context.WithTimeout(ctx, 10*time.Second)
	} else {
		cancel = func() {}
	}
	defer cancel()
	if err := c.nc.FlushWithContext(flushCtx); err != nil {
		return fmt.Errorf("queue/nats: ping: %w", err)
	}
	return nil
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

// MaxPayload returns the server-negotiated maximum message size (bytes)
// this connection will accept, or 0 when unknown / not connected. The
// cloud publisher reads it to decide when a RunMessage's compiled IR must
// be offloaded out-of-band via the IRRef fallback (T-42) instead of being
// inlined on the wire.
func (c *Conn) MaxPayload() int64 { return c.nc.MaxPayload() }

// EnsureSchema creates the JetStream streams + KV bucket idempotently.
// Designed to run on every server / runner boot so the topology is
// self-healing — if an operator deletes a stream by mistake the next
// pod start brings it back.
func (c *Conn) EnsureSchema(ctx context.Context) error {
	kv, err := ensureSchema(ctx, c.js, c.cfg)
	if err != nil {
		return err
	}
	c.kv = kv

	return nil
}

type schemaManager interface {
	CreateOrUpdateStream(context.Context, jetstream.StreamConfig) (jetstream.Stream, error)
	CreateOrUpdateKeyValue(context.Context, jetstream.KeyValueConfig) (jetstream.KeyValue, error)
}

func ensureSchema(ctx context.Context, js schemaManager, cfg Config) (jetstream.KeyValue, error) {
	if _, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:       cfg.StreamName,
		Subjects:   []string{SubjectRuns},
		Retention:  jetstream.WorkQueuePolicy,
		MaxAge:     cfg.MaxAge,
		Storage:    jetstream.FileStorage,
		Replicas:   cfg.StreamReplicas,
		Duplicates: 5 * time.Minute, // window for Nats-Msg-Id dedup
	}); err != nil {
		return nil, fmt.Errorf("queue/nats: stream %s: %w", cfg.StreamName, err)
	}

	if _, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:      cfg.DLQStream,
		Subjects:  []string{SubjectRunsDLQ},
		Retention: jetstream.LimitsPolicy,
		MaxAge:    cfg.DLQMaxAge,
		Storage:   jetstream.FileStorage,
		Replicas:  cfg.StreamReplicas,
	}); err != nil {
		return nil, fmt.Errorf("queue/nats: stream %s: %w", cfg.DLQStream, err)
	}

	kv, err := js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:   cfg.KVBucket,
		TTL:      cfg.LockTTL,
		History:  1,
		Replicas: cfg.StreamReplicas,
	})
	if err != nil {
		return nil, fmt.Errorf("queue/nats: kv %s: %w", cfg.KVBucket, err)
	}

	return kv, nil
}

// PublishRun submits a RunMessage onto the iterion.queue.runs subject.
// Launches use RunID as Nats-Msg-Id so retries inside JetStream's dedup window
// are absorbed. Resumes must use a distinct id: a schema-mismatch park can
// happen before that window closes, and reusing bare RunID would make an
// operator's successful resume response enqueue nothing. PublishedAtRFC is
// stable when the same message object is retried, yet unique per resume
// attempt created by SubmitResume.
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

	// NATS rejects messages larger than the server-negotiated
	// max_payload (default 1 MiB). Catch the limit ourselves so the
	// caller gets a clean, actionable error instead of an opaque
	// runtime ErrMaxPayload after the IR has been built. Oversized IR is
	// meant to be offloaded out-of-band by the publisher (IRRef fallback,
	// T-42) BEFORE reaching here; a message that still exceeds the limit
	// at publish means the offload path was skipped (e.g. a store without
	// the IRBlobStore seam), so surface it explicitly.
	if maxPayload := c.nc.MaxPayload(); maxPayload > 0 && int64(len(body)) > maxPayload {
		return nil, fmt.Errorf("queue/nats: RunMessage size %d exceeds NATS max_payload %d for run %s — compiled IR was not offloaded via the IRRef fallback (store lacks out-of-band IR blob support?)", len(body), maxPayload, msg.RunID)
	}

	headers := nats.Header{}
	headers.Set("Nats-Msg-Id", runMessageID(msg))
	headers.Set("iterion-schema-version", fmt.Sprintf("%d", msg.V))

	// Plan §F (T-41): inject W3C traceparent + tracestate from the
	// caller's ctx so the runner-side span inherits the parent. The
	// queue.TraceContext mirror in the body is also populated for
	// callers who decode the payload before the headers (defence in
	// depth). The propagator is installed once at Connect time so
	// this stays out of the per-publish hot path.
	injectTrace(ctx, msg, headers)
	// Convenience aliases — runners that don't want to drag in OTel
	// can still read these directly. Kept after Inject so the W3C
	// header takes precedence on conflict.
	if msg.Trace.TraceID != "" {
		headers.Set("iterion-trace-id", msg.Trace.TraceID)
		headers.Set("iterion-span-id", msg.Trace.SpanID)
	}

	// Per-publish timeout: the NATS client is configured with
	// MaxReconnects(-1) so a downed broker leaves PublishMsg blocked
	// indefinitely waiting for ack — that froze studio handlers
	// (Launch run) under broker outage. 5s is long enough for normal
	// hiccups and short enough that the studio surfaces "queue down"
	// to the user instead of hanging.
	pubCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return c.js.PublishMsg(pubCtx, &nats.Msg{
		Subject: SubjectRuns,
		Data:    body,
		Header:  headers,
	})
}

func runMessageID(msg *queue.RunMessage) string {
	if msg.Resume != nil {
		return fmt.Sprintf("%s|resume-%s", msg.RunID, msg.PublishedAtRFC)
	}
	return msg.RunID
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
	if c == nil || c.nc == nil {
		return fmt.Errorf("queue/nats: connection not initialised")
	}
	if err := c.nc.Publish(fmt.Sprintf(SubjectCancelFmt, runID), nil); err != nil {
		return fmt.Errorf("queue/nats: publish cancel %s: %w", runID, err)
	}
	// Core NATS Publish only queues to the client's flusher; force a
	// bounded round-trip so API callers learn when a reconnect/partition
	// prevented the transient cancel signal from reaching the broker.
	if err := c.nc.FlushTimeout(5 * time.Second); err != nil {
		return fmt.Errorf("queue/nats: flush cancel %s: %w", runID, err)
	}
	return nil
}

// SubscribeCancel installs a one-shot Core NATS subscriber on
// `iterion.cancel.<run_id>` and invokes onCancel when a message
// arrives. The runner uses this for the duration of a single run;
// the returned subscription is valid until ctx is cancelled.
func (c *Conn) SubscribeCancel(ctx context.Context, runID string, onCancel func()) (*nats.Subscription, error) {
	if c == nil || c.nc == nil {
		return nil, fmt.Errorf("queue/nats: connection not initialised")
	}
	if ctx == nil {
		return nil, fmt.Errorf("queue/nats: subscribe cancel %s: nil context", runID)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sub, err := c.nc.Subscribe(fmt.Sprintf(SubjectCancelFmt, runID), func(_ *nats.Msg) {
		onCancel()
	})
	if err != nil {
		return nil, fmt.Errorf("queue/nats: subscribe cancel %s: %w", runID, err)
	}
	// Subscribe is asynchronous: without a flush, a cancel published just
	// after SubscribeCancel returns can race ahead of the SUB protocol and
	// be lost. Bound the round-trip with the caller's context (or 5s when
	// it has no deadline) and tear the subscription back down on failure.
	flushCtx := ctx
	var cancel context.CancelFunc
	if _, ok := flushCtx.Deadline(); !ok {
		flushCtx, cancel = context.WithTimeout(ctx, 5*time.Second)
	} else {
		cancel = func() {}
	}
	flushErr := c.nc.FlushWithContext(flushCtx)
	cancel()
	if flushErr != nil {
		if unsubErr := sub.Unsubscribe(); unsubErr != nil {
			return nil, fmt.Errorf("queue/nats: subscribe cancel %s flush: %w (unsubscribe: %v)", runID, flushErr, unsubErr)
		}
		return nil, fmt.Errorf("queue/nats: subscribe cancel %s flush: %w", runID, flushErr)
	}
	go func() {
		<-ctx.Done()
		if err := sub.Unsubscribe(); err != nil && c.logger != nil {
			c.logger.Warn("queue/nats: unsubscribe cancel %s: %v", runID, err)
		}
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
// stream. Every runner pod binds the SAME durable (shared pull consumer),
// so MaxAckPending is a FLEET-WIDE in-flight cap, not per-pod: it must stay
// ≥ the pod count or it re-caps global parallelism (it was pinned to 1,
// which serialised the whole fleet to one concurrent run). Actual
// parallelism = pods each holding one in-flight run via Fetch, elastic
// under KEDA; MaxAckPending is only the ceiling. DeliverAllPolicy means a
// fresh consumer replays from the earliest pending message (matters when a
// stale pod is replaced). CreateOrUpdate applies the current MaxAckPending
// to the existing durable, so a deploy lifts the cap on the live consumer.
func (c *Conn) NewConsumer(ctx context.Context) (*Consumer, error) {
	cons, err := c.js.CreateOrUpdateConsumer(ctx, c.cfg.StreamName, jetstream.ConsumerConfig{
		Durable:       c.cfg.ConsumerName,
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       c.cfg.AckWait,
		MaxAckPending: c.cfg.MaxAckPending,
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
	// Respect caller cancellation before either phase. FetchNoWait
	// takes no context (SDK shape) and can stall on a partitioned
	// NATS for the connection RTT even though loopCtx may already
	// have been cancelled by Shutdown — that eroded the shutdown
	// grace contract. Short-circuit cleanly here.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
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
	batch2, err := cons.cons.Fetch(1, jetstream.FetchContext(timeoutCtx))
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(timeoutCtx.Err(), context.DeadlineExceeded) {
			return nil, ErrNoMessage
		}
		return nil, fmt.Errorf("queue/nats: fetch wait: %w", err)
	}
	for msg := range batch2.Messages() {
		return wrap(msg), nil
	}
	// A transport error during the blocking fetch must not masquerade as
	// "no message ready": that would skip the runner loop's back-off and
	// spin tightly against a partitioned broker. A bare wait timeout is
	// the expected empty-poll case and stays ErrNoMessage.
	if err := batch2.Error(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, ErrNoMessage
		}
		return nil, fmt.Errorf("queue/nats: fetch wait error: %w", err)
	}
	return nil, ErrNoMessage
}

// ErrNoMessage signals that Fetch elapsed its wait without a message.
// Callers loop on this to keep polling without treating it as a
// failure (the runner does).
var ErrNoMessage = errors.New("queue/nats: no message ready")

// Pending returns the number of messages currently waiting on the
// durable consumer. Used by the runner's metrics goroutine to publish
// the iterion_nats_pending_messages gauge — the same value KEDA pulls
// via the JetStream scaler.
func (cons *Consumer) Pending(ctx context.Context) (uint64, error) {
	info, err := cons.cons.Info(ctx)
	if err != nil {
		return 0, fmt.Errorf("queue/nats: consumer info: %w", err)
	}
	return info.NumPending, nil
}

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

// Envelope extracts the stable identity fields (v, run_id, tenant_id,
// owner_id) WITHOUT validating the schema version. This is the decode path
// for messages this build rejects: a version-mismatched payload must still be
// identifiable so it can be parked on the DLQ and its run document flipped to
// an actionable status (issue #481).
func (d *Delivery) Envelope() (queue.Envelope, error) {
	return queue.PeekEnvelope(d.raw.Data())
}

// Ack marks the delivery as successfully processed. Stops redelivery.
func (d *Delivery) Ack() error { return d.raw.Ack() }

// Nak schedules a redelivery (after AckWait expires or sooner).
func (d *Delivery) Nak() error { return d.raw.Nak() }

// NakWithDelay schedules a redelivery no sooner than delay. Use it instead of
// Nak whenever the redelivery is meant to wait for an EXTERNAL condition —
// e.g. a schema-version mismatch waiting for the runner fleet to finish
// rolling. A bare Nak is immediate: MaxDeliver attempts can be burned in
// seconds, long before the condition had a chance to change (issue #481).
func (d *Delivery) NakWithDelay(delay time.Duration) error { return d.raw.NakWithDelay(delay) }

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
// unchanged. The propagator is installed at Connect time so this
// stays out of the per-message hot path. Plan §F (T-41).
func (d *Delivery) PropagateTraceTo(ctx context.Context) context.Context {
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
	if c.StreamReplicas == 0 {
		c.StreamReplicas = DefaultStreamReplicas
	}
	if c.ConsumerName == "" {
		c.ConsumerName = ConsumerRunners
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
	if c.SchemaMismatchDelay == 0 {
		c.SchemaMismatchDelay = SchemaMismatchNakDelay
	}
	if c.MaxAckPending == 0 {
		c.MaxAckPending = DefaultMaxAckPending
	}
	if c.LockTTL == 0 {
		c.LockTTL = DefaultLockTTL
	}
	if c.Logger == nil {
		c.Logger = iterlog.New(iterlog.LevelInfo, nil)
	}
	return c
}

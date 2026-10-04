// Sovereign runner pools (#2029, plan v2.1 D2'/D4'): one stream + one DLQ
// stream + one consumer PER POOL, next to the shared default pair. A pool
// name is grammar-checked at the wire (queue.ValidPoolName), so every name
// here is a safe subject/stream/consumer suffix. Per-pool topology gives
// retention, lifecycle and ACL isolation by construction, and a pre-pool
// binary never rewrites a stream it does not know — the mixed-fleet
// topology hazard shrinks to the shared pair the old code already owned.
package nats

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	// StreamRunsPoolPrefix + <pool> names a pool's run stream; the shared
	// default stream keeps its exact subject and never carries pool runs.
	StreamRunsPoolPrefix = "ITERION_RUNS_POOL_"
	// SubjectRunsPoolPrefix + <pool> is the exact subject a pool's run
	// stream covers. Not a wildcard: the default stream's exact subject
	// never matches it, so publishing a pool run cannot land on the shared
	// stream, and a stale default-consumer cannot claim it.
	SubjectRunsPoolPrefix = "iterion.queue.runs.pool."
	// ConsumerRunsPoolPrefix + <pool> is the pool's durable consumer.
	ConsumerRunsPoolPrefix = "iterion-runners-pool-"
)

// PoolStreamName is the JetStream run-stream name for pool.
func PoolStreamName(pool string) string { return StreamRunsPoolPrefix + pool }

// PoolDLQStreamName is the DLQ stream name for pool.
func PoolDLQStreamName(pool string) string { return StreamRunsPoolPrefix + pool + "_DLQ" }

// PoolSubject is the exact run subject for pool.
func PoolSubject(pool string) string { return SubjectRunsPoolPrefix + pool }

// PoolDLQSubject is the DLQ subject for pool.
func PoolDLQSubject(pool string) string { return SubjectRunsPoolPrefix + pool + ".dlq" }

// PoolConsumerName is the durable consumer name for pool.
func PoolConsumerName(pool string) string { return ConsumerRunsPoolPrefix + pool }

// PoolFromStreamName inverts PoolStreamName for a pool stream, reporting
// whether the name belongs to a pool stream at all.
func PoolFromStreamName(name string) (string, bool) {
	rest, ok := strings.CutPrefix(name, StreamRunsPoolPrefix)
	if !ok || rest == "" || strings.HasSuffix(rest, "_DLQ") {
		return "", false
	}
	return rest, true
}

// EnsurePoolSchema creates the pool's run stream + DLQ stream
// idempotently — the same self-healing contract as EnsureSchema, scoped to
// one pool. A WorkQueue run stream with its exact subject, a Limits DLQ
// with the ".dlq" subject: the shared pair's shape, one pool over.
func (c *Conn) EnsurePoolSchema(ctx context.Context, pool string) error {
	return ensurePoolSchema(ctx, c.js, c.cfg, pool)
}

// ensurePoolSchema is the function form, testable against a recording
// schema manager without a broker (the ensureSchema pattern).
func ensurePoolSchema(ctx context.Context, js schemaManager, cfg Config, pool string) error {
	if !queue.ValidPoolName(pool) {
		return fmt.Errorf("queue/nats: pool %q invalid (want 1–31 chars [a-z0-9-], starting alphanumeric)", pool)
	}
	if _, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:       PoolStreamName(pool),
		Subjects:   []string{PoolSubject(pool)},
		Retention:  jetstream.WorkQueuePolicy,
		MaxAge:     cfg.MaxAge,
		Storage:    jetstream.FileStorage,
		Replicas:   cfg.StreamReplicas,
		Duplicates: 5 * time.Minute, // window for Nats-Msg-Id dedup
	}); err != nil {
		return fmt.Errorf("queue/nats: pool stream %s: %w", PoolStreamName(pool), err)
	}
	if _, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:      PoolDLQStreamName(pool),
		Subjects:  []string{PoolDLQSubject(pool)},
		Retention: jetstream.LimitsPolicy,
		MaxAge:    cfg.DLQMaxAge,
		Storage:   jetstream.FileStorage,
		Replicas:  cfg.StreamReplicas,
	}); err != nil {
		return fmt.Errorf("queue/nats: pool dlq stream %s: %w", PoolDLQStreamName(pool), err)
	}
	return nil
}

// PreparePoolConsumer creates/updates the pool's durable consumer on the
// pool's stream — the SERVER owns this (KEDA needs the consumer to exist,
// and a pool runner attaches an existing consumer rather than creating
// one, plan D4'). Same knobs as the shared consumer, exact pool subject.
func (c *Conn) PreparePoolConsumer(ctx context.Context, pool string) (*Consumer, error) {
	if c == nil || c.js == nil {
		return nil, fmt.Errorf("queue/nats: connection not initialised")
	}
	if !queue.ValidPoolName(pool) {
		return nil, fmt.Errorf("queue/nats: pool %q invalid (want 1–31 chars [a-z0-9-], starting alphanumeric)", pool)
	}
	cons, err := c.js.CreateOrUpdateConsumer(ctx, PoolStreamName(pool), poolConsumerConfig(pool, c.cfg))
	if err != nil {
		return nil, fmt.Errorf("queue/nats: pool consumer %s: %w", PoolConsumerName(pool), err)
	}
	return &Consumer{cons: cons, owner: c, cfg: c.cfg, logger: c.logger}, nil
}

// poolConsumerConfig is the pool consumer's shape, extracted so the naming
// and the knobs are testable without a broker.
func poolConsumerConfig(pool string, cfg Config) jetstream.ConsumerConfig {
	return jetstream.ConsumerConfig{
		Durable:       PoolConsumerName(pool),
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       cfg.AckWait,
		MaxAckPending: cfg.MaxAckPending,
		MaxDeliver:    cfg.MaxDeliver,
		DeliverPolicy: jetstream.DeliverAllPolicy,
		FilterSubject: PoolSubject(pool),
	}
}

// PublishRunPool routes a pool-stamped message onto its pool's stream —
// JetStream picks the stream by subject, so the exact pool subject cannot
// land on the shared stream. A message without a pool keeps PublishRun's
// shared subject; the two never mix.
func (c *Conn) publishSubject(msg *queue.RunMessage) string {
	if msg.RunnerPool != "" {
		return PoolSubject(msg.RunnerPool)
	}
	return SubjectRuns
}

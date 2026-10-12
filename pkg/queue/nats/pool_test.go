package nats

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/nats-io/nats.go/jetstream"
)

// The per-pool topology: one run stream + one DLQ stream per pool, with
// EXACT subjects the shared stream never covers — a pool run cannot land
// on the shared stream and a stale default consumer cannot claim it. Red
// when the subjects overlap, the retention drifts from the shared pair, or
// the pool grammar is not enforced.
func TestEnsurePoolSchema_TopologyPerPool(t *testing.T) {
	rec := &recordingSchemaManager{}
	cfg := Config{MaxAge: time.Hour, DLQMaxAge: 7 * 24 * time.Hour, StreamReplicas: 3}
	if err := ensurePoolSchema(context.Background(), rec, cfg, "honorabilite"); err != nil {
		t.Fatalf("EnsurePoolSchema: %v", err)
	}
	if err := ensurePoolSchema(context.Background(), rec, cfg, "../escape"); err == nil ||
		!strings.Contains(err.Error(), "invalid") {
		t.Fatalf("a path-shaped pool name must be refused by the grammar, got: %v", err)
	}
	if len(rec.streams) != 2 {
		t.Fatalf("the refused pool must not have created streams: %+v", rec.streams)
	}
	if len(rec.streams) != 2 {
		t.Fatalf("want exactly 2 streams (run + dlq), got %+v", rec.streams)
	}
	run, dlq := rec.streams[0], rec.streams[1]
	if run.Name != "ITERION_RUNS_POOL_honorabilite" || dlq.Name != "ITERION_RUNS_POOL_honorabilite_DLQ" {
		t.Fatalf("stream names: %+v", rec.streams)
	}
	if len(run.Subjects) != 1 || run.Subjects[0] != "iterion.queue.runs.pool.honorabilite" {
		t.Fatalf("run subjects: %+v", run.Subjects)
	}
	if len(dlq.Subjects) != 1 || dlq.Subjects[0] != "iterion.queue.runs.pool.honorabilite.dlq" {
		t.Fatalf("dlq subjects: %+v", dlq.Subjects)
	}
	if run.Retention != jetstream.WorkQueuePolicy || dlq.Retention != jetstream.LimitsPolicy {
		t.Fatalf("retention must mirror the shared pair: run=%v dlq=%v", run.Retention, dlq.Retention)
	}
	if run.MaxAge != time.Hour || dlq.MaxAge != 7*24*time.Hour || run.Replicas != 3 {
		t.Fatalf("knobs must come from the shared config: %+v %+v", run, dlq)
	}
}

// The pool consumer: durable per pool, filter subject EXACT, same knobs as
// the shared consumer. Red when the naming or the filter drifts.
func TestPoolConsumerConfig(t *testing.T) {
	cfg := Config{AckWait: 11 * time.Second, MaxAckPending: 42, MaxDeliver: 7}
	got := poolConsumerConfig("honorabilite", cfg)
	if got.Durable != "iterion-runners-pool-honorabilite" {
		t.Fatalf("durable = %q", got.Durable)
	}
	if got.FilterSubject != "iterion.queue.runs.pool.honorabilite" {
		t.Fatalf("filter subject = %q, want the pool's exact subject", got.FilterSubject)
	}
	if got.AckWait != 11*time.Second || got.MaxAckPending != 42 || got.MaxDeliver != 7 {
		t.Fatalf("knobs must mirror the shared consumer: %+v", got)
	}
}

// The routing rule: a pool-stamped message publishes on the pool's subject,
// everything else on the shared subject — the two never mix. Red when
// publishSubject stops deriving from the frozen stamp.
func TestPublishSubjectDerivesFromThePool(t *testing.T) {
	if got := (&Conn{}).publishSubject(&queue.RunMessage{RunnerPool: "honorabilite"}); got != "iterion.queue.runs.pool.honorabilite" {
		t.Fatalf("pool subject = %q", got)
	}
	if got := (&Conn{}).publishSubject(&queue.RunMessage{}); got != SubjectRuns {
		t.Fatalf("default subject = %q, want %q", got, SubjectRuns)
	}
}

// The stream-name inversion: reconcilers and operators read pool names back
// from stream names; a DLQ stream is NOT a pool run stream.
func TestPoolFromStreamName(t *testing.T) {
	if p, ok := PoolFromStreamName("ITERION_RUNS_POOL_honorabilite"); !ok || p != "honorabilite" {
		t.Fatalf("pool = %q ok=%v", p, ok)
	}
	if _, ok := PoolFromStreamName("ITERION_RUNS_POOL_honorabilite_DLQ"); ok {
		t.Fatal("a DLQ stream must not read as a pool run stream")
	}
	if _, ok := PoolFromStreamName("ITERION_RUNS"); ok {
		t.Fatal("the shared stream must not read as a pool stream")
	}
}

// The attach's fail-closed refusals, one row per leg (rva Re3adb6: the
// checks can be weakened with zero red if unwitnessed). The MaxDeliver leg
// is the rva R33cc26 skew: the runner budgets on the POD's config while
// the broker enforces the CONSUMER's — equality or refusal.
func TestVerifyPoolConsumerAttachment(t *testing.T) {
	base := jetstream.ConsumerConfig{
		Durable:       "iterion-runners-pool-honorabilite",
		FilterSubject: "iterion.queue.runs.pool.honorabilite",
		AckPolicy:     jetstream.AckExplicitPolicy,
		MaxDeliver:    8,
	}
	if err := verifyPoolConsumerAttachment(base, "honorabilite", 8); err != nil {
		t.Fatalf("a correctly pointed, aligned consumer verifies: %v", err)
	}
	durable := base
	durable.Durable = "iterion-runners"
	if err := verifyPoolConsumerAttachment(durable, "honorabilite", 8); err == nil || !strings.Contains(err.Error(), "durable") {
		t.Fatalf("a shared durable must be refused: %v", err)
	}
	filter := base
	filter.FilterSubject = "iterion.queue.runs"
	if err := verifyPoolConsumerAttachment(filter, "honorabilite", 8); err == nil || !strings.Contains(err.Error(), "filter") {
		t.Fatalf("a shared filter must be refused: %v", err)
	}
	ack := base
	ack.AckPolicy = jetstream.AckNonePolicy
	if err := verifyPoolConsumerAttachment(ack, "honorabilite", 8); err == nil || !strings.Contains(err.Error(), "auto-acks") {
		t.Fatalf("an auto-acking consumer must be refused: %v", err)
	}
	skew := base
	skew.MaxDeliver = 3
	if err := verifyPoolConsumerAttachment(skew, "honorabilite", 8); err == nil || !strings.Contains(err.Error(), "MaxDeliver 3 does not match") {
		t.Fatalf("a MaxDeliver skew must be refused: %v", err)
	}
}

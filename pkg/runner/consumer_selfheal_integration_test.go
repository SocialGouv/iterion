package runner

// Integration test for the stall self-heal (issue #2890): the Oct 30
// wedge froze the durable consumer's raft state — deliveries stopped
// while pull requests kept waiting — and never self-resolved; only
// deleting the consumer cleared it. This walks the real broker path:
// an external deletion puts the fetch in the consumer-gone shape every
// pod sees, the fetch-path recreate rebuilds the handle, and a message
// published after the recreation is claimed through the fresh consumer.
// Gated on ITERION_TEST_NATS_URI (CI: nats-conformance job), mirror of
// the schema-rollout harness beside it.

import (
	"context"
	"fmt"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	natsq "github.com/SocialGouv/iterion/pkg/queue/nats"
)

// selfHealConn builds a fully-namespaced Conn (stream, DLQ, KV buckets,
// consumer — all test-suffixed) with the names exposed: the self-heal
// tests delete and re-create the consumer through the same manager the
// production heal and a hand-rolled `consumer rm` use.
func selfHealConn(t *testing.T, uri string) (conn *natsq.Conn, stream, consumer string) {
	t.Helper()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	stream = "ITERION_RUNS_TEST_" + suffix
	dlq := "ITERION_RUNS_DLQ_TEST_" + suffix
	kv := "test-run-locks-" + suffix
	rolloutKV := "test-runner-rollout-" + suffix
	consumer = "test-runners-" + suffix
	c, err := natsq.Connect(context.Background(), natsq.Config{
		URL:             uri,
		StreamName:      stream,
		DLQStream:       dlq,
		KVBucket:        kv,
		RolloutKVBucket: rolloutKV,
		ConsumerName:    consumer,
		MaxDeliver:      2,
		AckWait:         2 * time.Second,
		MaxAge:          time.Hour,
		Logger:          iterlog.Nop(),
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := c.ClaimRunnerEpoch(context.Background()); err != nil {
		c.Close()
		t.Fatalf("claim rollout epoch: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = c.JetStream().DeleteStream(ctx, stream)
		_ = c.JetStream().DeleteStream(ctx, dlq)
		_ = c.JetStream().DeleteKeyValue(ctx, kv)
		_ = c.JetStream().DeleteKeyValue(ctx, rolloutKV)
		c.Close()
	})
	return c, stream, consumer
}

// TestConsumerRecreateRecoversFromExternalDeletion walks the wedge's own
// shape: the consumer is deleted under the pod (the self-heal's gesture),
// the fetch fails consumer-gone, Recreate rebuilds, and a message
// published after the recreation is claimed through the fresh consumer.
func TestConsumerRecreateRecoversFromExternalDeletion(t *testing.T) {
	conn, stream, consumer := selfHealConn(t, schemaRolloutNATSURI(t))
	ctx := context.Background()

	cons, err := conn.PrepareConsumer(ctx)
	if err != nil {
		t.Fatalf("prepare consumer: %v", err)
	}

	// The sibling pod's stall self-heal deletes the consumer under us.
	if err := conn.JetStream().DeleteConsumer(ctx, stream, consumer); err != nil {
		t.Fatalf("external delete: %v", err)
	}

	fetchCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, fetchErr := cons.Fetch(fetchCtx, time.Second)
	if fetchErr == nil {
		t.Fatal("fetch against a deleted consumer: want an error, got a delivery")
	}

	// The fetch-path recovery: the deleted consumer's pull request meets
	// no responder (not a clean sentinel) — RecoverIfGone verifies
	// against the broker and recreates.
	if !cons.RecoverIfGone(ctx, fetchErr) {
		t.Fatalf("RecoverIfGone did not recover from %v", fetchErr)
	}

	// The fresh consumer claims again.
	if _, err := conn.JetStream().Publish(ctx, natsq.SubjectRuns, []byte(`{}`)); err != nil {
		t.Fatalf("publish probe: %v", err)
	}
	fetchCtx2, cancel2 := context.WithTimeout(ctx, 5*time.Second)
	defer cancel2()
	delivery, err := cons.Fetch(fetchCtx2, 2*time.Second)
	if err != nil {
		t.Fatalf("fetch after recreate: %v", err)
	}
	if err := delivery.Nak(); err != nil {
		t.Fatalf("nak the probe: %v", err)
	}
}

// Recreate is safe against a racing pair of healers: the second caller's
// delete lands on an already-deleted (or just-recreated) consumer and
// must not error the heal away — both pods converge on one live consumer.
func TestConsumerRecreateIsIdempotentUnderRacingSiblings(t *testing.T) {
	conn, stream, consumer := selfHealConn(t, schemaRolloutNATSURI(t))
	ctx := context.Background()

	cons, err := conn.PrepareConsumer(ctx)
	if err != nil {
		t.Fatalf("prepare consumer: %v", err)
	}
	if err := cons.Recreate(ctx); err != nil {
		t.Fatalf("first recreate: %v", err)
	}
	// A racing sibling deletes the fresh consumer between our samples.
	if err := conn.JetStream().DeleteConsumer(ctx, stream, consumer); err != nil {
		t.Fatalf("sibling delete: %v", err)
	}
	if err := cons.Recreate(ctx); err != nil {
		t.Fatalf("racing recreate: %v", err)
	}
	if _, err := cons.Sample(ctx); err != nil {
		t.Fatalf("sample after racing recreates: %v", err)
	}
}

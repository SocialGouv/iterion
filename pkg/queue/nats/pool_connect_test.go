package nats

// A pool-serving Connect brings up the POOL's stream pair with the
// pool's exact subjects and leaves the shared pair alone. Measured in
// prod on the first pool boot (2026-10-07): a pool boot through the
// shared EnsureSchema created a shared-subject stream under the pool's
// name, and the install's shared stream then refused the connection
// with subjects-overlap (API 10065). This witness replays the prod
// order: the reconciler's pool streams exist FIRST, no shared stream
// does, and the pod's Connect must not create one.

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

func TestPoolConnectBringsUpPoolTopology(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_NATS_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_NATS_URI unset — skipping pool connect test (CI: nats-conformance job)")
	}
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	pool := "testpool" + suffix[len(suffix)-4:]
	kvBucket := "test-pool-kv-" + suffix
	rolloutBucket := "test-pool-rkv-" + suffix

	// Reconciler state: ONLY the pool's stream pair exists. A bare
	// JetStream session creates exactly that — no shared streams.
	nc, err := natsgo.Connect(uri, natsgo.MaxReconnects(-1), natsgo.ReconnectWait(2*time.Second))
	if err != nil {
		t.Fatalf("nats connect: %v", err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		t.Fatalf("jetstream: %v", err)
	}
	if err := ensurePoolSchema(ctx, js, Config{MaxAge: time.Hour, DLQMaxAge: time.Hour, StreamReplicas: 1, Logger: iterlog.Nop()}, pool); err != nil {
		nc.Close()
		t.Fatalf("reconciler pool schema: %v", err)
	}
	nc.Close()

	// The pod's Connect: pool topology, shared KVs, and NOT the shared
	// stream pair.
	pod, err := Connect(ctx, Config{
		URL:             uri,
		Pool:            pool,
		KVBucket:        kvBucket,
		RolloutKVBucket: rolloutBucket,
	})
	if err != nil {
		t.Fatalf("pool connect over the reconciler's streams: %v", err)
	}
	defer pod.Close()

	info, err := pod.JetStream().Stream(ctx, PoolStreamName(pool))
	if err != nil {
		t.Fatalf("pool stream missing after a pool connect: %v", err)
	}
	info2, err := info.Info(ctx)
	if err != nil {
		t.Fatalf("pool stream info: %v", err)
	}
	if len(info2.Config.Subjects) != 1 || info2.Config.Subjects[0] != PoolSubject(pool) {
		t.Fatalf("pool stream subjects %v, want exactly [%s]", info2.Config.Subjects, PoolSubject(pool))
	}
	if _, err := pod.JetStream().Stream(ctx, StreamRuns); err == nil {
		t.Fatal("the pool pod created the SHARED run stream — a pool boot must not touch the shared topology")
	}
	if pod.kv == nil || pod.rolloutKV == nil {
		t.Fatal("the shared KV buckets did not come up with a pool connect (the lease and the epoch need them)")
	}

	// The pod template stamps the derived names: accepted (the deployed
	// pool pods carry them — merged alone, this branch must keep them
	// booting), and they land on the same pool topology.
	stamped, err := Connect(ctx, Config{
		URL:             uri,
		Pool:            pool,
		StreamName:      PoolStreamName(pool),
		DLQStream:       PoolDLQStreamName(pool),
		KVBucket:        kvBucket + "b",
		RolloutKVBucket: rolloutBucket + "b",
	})
	if err != nil {
		t.Fatalf("pool connect with the chart's derived stream names: %v", err)
	}
	stamped.Close()

	// ANY other stream name points the pool's deliveries elsewhere: refused.
	if _, err := Connect(ctx, Config{
		URL:             uri,
		Pool:            pool,
		StreamName:      "ITERION_RUNS_EXPLICIT",
		KVBucket:        kvBucket + "c",
		RolloutKVBucket: rolloutBucket + "c",
	}); err == nil || !contains(err.Error(), PoolStreamName(pool)) {
		t.Fatalf("Pool with an arbitrary StreamName: err = %v, want the derived-name refusal", err)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle || len(needle) == 0 || indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

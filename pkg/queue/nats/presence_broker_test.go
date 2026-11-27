package nats

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/SocialGouv/iterion/pkg/internal/appinfo"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
)

// The fleet registry against a real JetStream: the bucket Connect creates,
// the stream-info listing, the draining mark, a deregistration and the
// bucket TTL expiring a record nobody refreshes. The unit tests drive the
// same code over doubles; this is where the broker's own answers are read.
func TestFleetRegistryOnARealBroker(t *testing.T) {
	uri := os.Getenv("ITERION_TEST_NATS_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_NATS_URI unset — skipping the fleet registry broker test (CI: nats-conformance job)")
	}
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	// A pool topology of its own, so the test shares no stream, subject or
	// bucket with any other suite on the broker: a custom lock bucket
	// isolates the rollout and presence buckets too (applyDefaults).
	pool := "fleet" + suffix[len(suffix)-6:]
	kvBucket := "test-fleet-kv-" + suffix

	nc, err := natsgo.Connect(uri)
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
		t.Fatalf("pool schema: %v", err)
	}
	t.Cleanup(func() {
		for _, bucket := range []string{kvBucket, kvBucket + "-rollout", kvBucket + "-presence"} {
			_ = js.DeleteKeyValue(context.Background(), bucket)
		}
		_ = js.DeleteStream(context.Background(), PoolStreamName(pool))
		_ = js.DeleteStream(context.Background(), PoolDLQStreamName(pool))
		nc.Close()
	})

	const ttl = 2 * time.Second
	conn, err := Connect(ctx, Config{URL: uri, Pool: pool, KVBucket: kvBucket, RunnerEpoch: 7, PresenceTTL: ttl, Logger: iterlog.Nop()})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close()

	server, err := conn.NewPresence(queue.FleetRoleServer, "srv", "")
	if err != nil {
		t.Fatal(err)
	}
	runner, err := conn.NewPresence(queue.FleetRoleRunner, "run", pool)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []*Presence{server, runner} {
		if err := p.Register(ctx); err != nil {
			t.Fatalf("register: %v", err)
		}
	}

	members := fleetOf(t, conn)
	if len(members) != 2 {
		t.Fatalf("fleet = %+v, want the server and the runner", members)
	}
	byRole := map[string]queue.FleetMember{}
	for _, m := range members {
		byRole[m.Role] = m
	}
	if r := byRole[queue.FleetRoleRunner]; r.Build != appinfo.FullVersion() || r.Epoch != 7 || r.Pool != pool || r.Draining || r.SeenAt.IsZero() {
		t.Fatalf("runner record = %+v, want this build, epoch 7, the pool, not draining, a broker timestamp", r)
	}

	if err := runner.MarkDraining(ctx); err != nil {
		t.Fatalf("mark draining: %v", err)
	}
	if err := server.Deregister(ctx); err != nil {
		t.Fatalf("deregister: %v", err)
	}
	members = fleetOf(t, conn)
	if len(members) != 1 || members[0].Role != queue.FleetRoleRunner || !members[0].Draining {
		t.Fatalf("fleet after the mark and the deregistration = %+v, want the draining runner alone", members)
	}

	// Nobody refreshes the runner's record: the bucket TTL removes it.
	deadline := time.Now().Add(10 * ttl)
	for len(members) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the unrefreshed record outlived %s: %+v", 10*ttl, members)
		}
		time.Sleep(200 * time.Millisecond)
		members = fleetOf(t, conn)
	}
}

func fleetOf(t *testing.T, conn *Conn) []queue.FleetMember {
	t.Helper()
	members, err := conn.FleetMembers(context.Background())
	if err != nil {
		t.Fatalf("read the fleet: %v", err)
	}
	return members
}

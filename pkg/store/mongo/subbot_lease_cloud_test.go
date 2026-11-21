package mongo

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	natsqueue "github.com/SocialGouv/iterion/pkg/queue/nats"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// TestSubbotLeaseMongoNATS exercises the real lease-backed Mongo seam.
// Requires BOTH integration endpoints, a Mongo replica set, and JetStream.
// CI must run it in the dedicated dual-service step of mongo-conformance;
// neither the Mongo-only nor NATS-only job proves this row. Blob bodies use
// this package's existing inMemoryBlob; this does not claim S3 coverage.
func TestSubbotLeaseMongoNATS(t *testing.T) {
	mongoURI, natsURI := os.Getenv("ITERION_TEST_MONGO_URI"), os.Getenv("ITERION_TEST_NATS_URI")
	if mongoURI == "" || natsURI == "" {
		t.Skip("requires ITERION_TEST_MONGO_URI and ITERION_TEST_NATS_URI; run mongo-conformance dual-service witness")
	}
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()
	nonce := bsonNonce(t)
	pool := "lease-" + nonce
	cfg := natsqueue.Config{
		URL: natsURI, Pool: pool, KVBucket: "lease_" + nonce,
		RolloutKVBucket: "rollout_" + nonce,
		LockTTL:         2 * time.Second, Logger: iterlog.Nop(),
	}
	// Own a distinct pool as well as buckets: changing stream names alone
	// would still claim the shared subjects and collide with another package.
	admin, err := natsgo.Connect(natsURI)
	if err != nil {
		t.Fatal(err)
	}
	js, err := jetstream.New(admin)
	if err != nil {
		admin.Close()
		t.Fatal(err)
	}
	// Registered before Connect, so partially created topology is reclaimed too.
	t.Cleanup(func() {
		cleanup, stop := mongotest.TeardownCtx()
		defer stop()
		defer admin.Close()
		for _, bucket := range []string{cfg.KVBucket, cfg.RolloutKVBucket} {
			if err := js.DeleteKeyValue(cleanup, bucket); err != nil && !errors.Is(err, jetstream.ErrBucketNotFound) && !errors.Is(err, jetstream.ErrStreamNotFound) {
				t.Errorf("drop owned bucket %s: %v", bucket, err)
			}
		}
		for _, stream := range []string{natsqueue.PoolStreamName(pool), natsqueue.PoolDLQStreamName(pool)} {
			if err := js.DeleteStream(cleanup, stream); err != nil && !errors.Is(err, jetstream.ErrStreamNotFound) {
				t.Errorf("drop owned stream %s: %v", stream, err)
			}
		}
	})
	conn, err := natsqueue.Connect(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(conn.Close)
	s, err := New(ctx, Config{URI: mongoURI, Database: "subbot_lease_" + nonce, Blob: newInMemoryBlob(), LockProvider: natsqueue.NewLockProvider(conn, "lease-owner-"+nonce), Logger: iterlog.Nop()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := mongotest.TeardownCtx()
		defer stop()
		if err := s.db.Drop(cleanup); err != nil {
			t.Errorf("drop owned database: %v", err)
		}
		if err := s.Close(cleanup); err != nil {
			t.Errorf("close Mongo: %v", err)
		}
	})
	parentCtx := store.WithLeaseIdentity(store.WithTenant(ctx, "tenant-"+nonce), "tenant-"+nonce, pool)
	parentCtx, err = store.WithRunValueFormat(parentCtx, store.RunValueFormat{RunID: "parent", Format: store.LegacyValueFormat})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateRun(parentCtx, "parent", "wf", nil); err != nil {
		t.Fatal(err)
	}
	lease, err := runview.AcquireSubbotLease(parentCtx, s, "parent", "child", runview.SubbotLeaseOptions{HeartbeatInterval: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := lease.Release(); err != nil {
			t.Errorf("release owned child lease: %v", err)
		}
	})
	if _, err = s.LoadRun(parentCtx, "child"); !errors.Is(err, store.ErrRunNotFound) {
		t.Fatalf("child exists before birth: %v", err)
	}
	if peer, err := conn.AcquireLock(parentCtx, "child", "prebirth-peer"); !errors.Is(err, natsqueue.ErrLockHeld) {
		if peer != nil {
			_ = peer.Unlock()
		}
		t.Fatalf("prebirth lease not exclusive: %v", err)
	}
	if _, err = lease.PrepareInvocation(lease.Context, s, store.RunCreateSpec{ID: "child", WorkflowName: "wf", Status: store.RunStatusRunning}); err != nil {
		t.Fatal(err)
	}
	row, err := s.LoadRun(parentCtx, "child")
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != store.RunStatusRunning || row.ParentRunID != "parent" {
		t.Fatalf("wrong birth: %+v", row)
	}
	// Cross two full TTL windows. This wait is the assertion's time dimension,
	// not a sleep guessing whether an asynchronous setup operation completed.
	timer := time.NewTimer(2*cfg.LockTTL + 200*time.Millisecond)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-lease.Context.Done():
		t.Fatalf("lease lost during preparation: %v", context.Cause(lease.Context))
	}
	if peer, err := conn.AcquireLock(parentCtx, "child", "live-peer"); !errors.Is(err, natsqueue.ErrLockHeld) {
		if peer != nil {
			_ = peer.Unlock()
		}
		t.Fatalf("peer acquired refreshed child: %v", err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	peer, err := conn.AcquireLock(parentCtx, "child", "successor")
	if err != nil {
		t.Fatal(err)
	}
	if err := peer.Unlock(); err != nil {
		t.Fatal(err)
	}
	drift := cfg
	drift.LockTTL = 2 * cfg.LockTTL
	if other, err := natsqueue.Connect(ctx, drift); err == nil {
		other.Close()
		t.Fatal("connection changed active bucket TTL")
	} else if !strings.Contains(err.Error(), "drain holders and align bucket configuration") {
		t.Fatalf("missing TTL migration diagnostic: %v", err)
	}
	after, err := conn.AcquireLock(parentCtx, "child", "after-refusal")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := after.Unlock(); err != nil {
			t.Errorf("release final lease: %v", err)
		}
	}()
	if after.LeaseTTL() != cfg.LockTTL {
		t.Fatalf("bucket TTL changed: %s", after.LeaseTTL())
	}
}

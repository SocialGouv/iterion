package mongo

import (
	"context"
	"errors"
	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	natsqueue "github.com/SocialGouv/iterion/pkg/queue/nats"
	"github.com/SocialGouv/iterion/pkg/store"
	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func birthMongoNATS(t *testing.T) (*Store, context.Context, *natsqueue.Conn) {
	t.Helper()
	mongoURI, natsURI := os.Getenv("ITERION_TEST_MONGO_URI"), os.Getenv("ITERION_TEST_NATS_URI")
	if mongoURI == "" || natsURI == "" {
		t.Skip("requires ITERION_TEST_MONGO_URI and ITERION_TEST_NATS_URI; run mongo-conformance dual-service witness")
	}
	ctx, cancel := mongotest.Ctx(t)
	t.Cleanup(cancel)
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
	s, err := New(ctx, Config{URI: mongoURI, Database: "subbot_lease_" + nonce, Blob: newInMemoryBlob(), RunFilesScratchDir: filepath.Join(t.TempDir(), "scratch"), LockProvider: natsqueue.NewLockProvider(conn, "lease-owner-"+nonce), Logger: iterlog.Nop()})
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
	return s, store.WithLeaseIdentity(store.WithTenant(ctx, "tenant-"+nonce), "tenant-"+nonce, pool), conn
}

func TestRunBirthMongoNATS(t *testing.T) {
	s, ctx, conn := birthMongoNATS(t)
	bind := func(id string) context.Context {
		t.Helper()
		c, e := store.WithRunValueFormat(ctx, store.RunValueFormat{RunID: id, Format: store.LegacyValueFormat})
		if e != nil {
			t.Fatal(e)
		}
		return c
	}
	t.Run("publication_and_owner", func(t *testing.T) {
		c := bind("child")
		b, e := s.BeginRunBirth(c, "child")
		if e != nil {
			t.Fatal(e)
		}
		defer func() { _ = b.Unlock() }()
		if _, e = s.LoadRun(c, "child"); !errors.Is(e, store.ErrRunNotFound) {
			t.Fatal("row before publication", e)
		}
		if peer, e := conn.AcquireLock(c, "child", "peer"); e == nil {
			_ = peer.Unlock()
			t.Fatal("second lease before birth")
		}
		born, e := b.Publish(c, store.RunCreateSpec{ID: "child", Format: store.LegacyValueFormat})
		if e != nil || born == nil || born.BirthID == "" {
			t.Fatalf("birth=%v %v", born, e)
		}
		got, e := s.LoadRun(c, "child")
		if e != nil || got.BirthID != born.BirthID {
			t.Fatal("receipt differs from row", e)
		}
		refresh := b.(interface{ Refresh(context.Context) error })
		if e = refresh.Refresh(c); e != nil {
			t.Fatal(e)
		}
		if e = b.Unlock(); e != nil {
			t.Fatal(e)
		}
		if e = b.Unlock(); e != nil {
			t.Fatal("release not idempotent", e)
		}
		peer, e := conn.AcquireLock(c, "child", "successor")
		if e != nil {
			t.Fatal(e)
		}
		if e = peer.Unlock(); e != nil {
			t.Fatal(e)
		}
	})
	t.Run("expiry_and_successor", func(t *testing.T) {
		c := bind("expired")
		b, e := s.BeginRunBirth(c, "expired")
		if e != nil {
			t.Fatal(e)
		}
		defer func() { _ = b.Unlock() }()
		ttl := b.(interface{ LeaseTTL() time.Duration }).LeaseTTL()
		timer := time.NewTimer(ttl + 100*time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		// Our conservative local deadline is not a promise about the exact
		// instant JetStream physically expires its message. Wait for the
		// actual successor acquisition, bounded by the test's deadline.
		deadline := time.Now().Add(3 * ttl)
		var peer *natsqueue.Lock
		for {
			peer, e = conn.AcquireLock(c, "expired", "successor")
			if e == nil {
				break
			}
			if !errors.Is(e, natsqueue.ErrLockHeld) || !time.Now().Before(deadline) {
				t.Fatal(e)
			}
			select {
			case <-time.After(25 * time.Millisecond):
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
		defer func() { _ = peer.Unlock() }()
		if born, e := b.Publish(c, store.RunCreateSpec{ID: "expired", Format: store.LegacyValueFormat}); born != nil || !errors.Is(e, store.ErrRunBirthExpired) {
			t.Fatalf("expired birth=%v %v", born, e)
		}
		if _, e = s.LoadRun(c, "expired"); !errors.Is(e, store.ErrRunNotFound) {
			t.Fatal("expired reservation published", e)
		}
		_ = b.Unlock() // A successor may make release refuse its stale revision.
		if e = peer.Refresh(c); e != nil {
			t.Fatal("old release damaged successor", e)
		}
	})
	t.Run("competing_creator", func(t *testing.T) {
		c := bind("collision")
		b, e := s.BeginRunBirth(c, "collision")
		if e != nil {
			t.Fatal(e)
		}
		defer func() { _ = b.Unlock() }()
		old, e := s.CreateRun(c, "collision", "older-creator", nil)
		if e != nil {
			t.Fatal(e)
		}
		if born, e := b.Publish(c, store.RunCreateSpec{ID: "collision", Format: store.LegacyValueFormat}); born != nil || e == nil {
			t.Fatalf("adopted competitor: %v %v", born, e)
		}
		got, e := s.LoadRun(c, "collision")
		if e != nil || got.BirthID != old.BirthID {
			t.Fatal("overwrote competitor", e)
		}
	})
}

package nats

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/store/storetest"
	natsclient "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

func censusFixture(t *testing.T) (*Conn, jetstream.KeyValue) {
	t.Helper()
	uri := os.Getenv("ITERION_TEST_NATS_URI")
	if uri == "" {
		if os.Getenv("ITERION_TEST_REQUIRED") == "1" {
			t.Fatal("required census tests need ITERION_TEST_NATS_URI")
		}
		t.Skip("ITERION_TEST_NATS_URI not set")
	}
	nc, err := natsclient.Connect(uri)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	bucket := fmt.Sprintf("ports_census_%d", time.Now().UnixNano())
	kv, err := js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{Bucket: bucket, History: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = js.DeleteKeyValue(context.Background(), bucket) })
	return &Conn{nc: nc, js: js, rolloutKV: kv, cfg: applyDefaults(Config{URL: uri, RolloutKVBucket: bucket})}, kv
}

func testPortCapability(instance string) PortInstanceCapability {
	return PortInstanceCapability{Version: PortCapabilityVersion, Principal: "runner", Instance: instance,
		BuildDigest: "sha256:" + strings.Repeat("a", 64), CapabilityDigest: strings.Repeat("b", 64),
		StoreIdentity: "mongodb:disposable", Account: "ITERION", Stream: StreamRuns, Consumer: ConsumerRunners,
		QueueVersion: queue.SchemaVersion, AuthorityEpoch: 1}
}

func TestPortCensusIdentityAndFreshness(t *testing.T) {
	for _, value := range []string{"", "runner.other", "runner.*", "runner.>", "runner@example.com", strings.Repeat("a", 129)} {
		if _, err := PortCensusKey(value, "instance"); err == nil {
			t.Errorf("unsafe principal key %q accepted", value)
		}
		if _, err := PortCensusKey("runner", value); err == nil {
			t.Errorf("unsafe instance key %q accepted", value)
		}
	}
	now := time.Now().UTC()
	for _, age := range []time.Duration{-time.Second, time.Minute, time.Hour} {
		if (PortCapabilityObservation{RecordedAt: now.Add(-age)}).Fresh(now) {
			t.Errorf("age %s is fresh", age)
		}
	}
	if !(PortCapabilityObservation{RecordedAt: now.Add(-time.Second)}).Fresh(now) {
		t.Fatal("recent broker observation is stale")
	}
}

func TestPortCensusRoundTripAndShutdown(t *testing.T) {
	c, kv := censusFixture(t)
	ctx := t.Context()
	const highWater = "27"
	if _, err := kv.PutString(ctx, RunnerEpochHighWaterKey, highWater); err != nil {
		t.Fatal(err)
	}
	empty, err := c.PortCapabilities(ctx)
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty census: %+v %v", empty, err)
	}
	p := testPortCapability("pod-a")
	rev1, err := c.PublishPortCapability(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	rev2, err := c.PublishPortCapability(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.DeletePortCapability(ctx, p.Principal, p.Instance, rev1); !errors.Is(err, jetstream.ErrKeyRevisionMismatch) {
		t.Fatalf("old writer deleted newer heartbeat: %v", err)
	}
	observations, err := c.PortCapabilities(ctx)
	if err != nil || len(observations) != 1 {
		t.Fatalf("census: %+v %v", observations, err)
	}
	o := observations[0]
	if o.Capability != p || o.Revision != rev2 || !o.Fresh(time.Now()) {
		t.Fatalf("lost broker freshness or identity: %+v", o)
	}
	if err := c.DeletePortCapability(ctx, p.Principal, p.Instance, rev2); err != nil {
		t.Fatal(err)
	}
	observations, err = c.PortCapabilities(ctx)
	if err != nil || len(observations) != 0 {
		t.Fatalf("deleted instance remained: %+v %v", observations, err)
	}
	entry, err := kv.Get(ctx, RunnerEpochHighWaterKey)
	if err != nil || string(entry.Value()) != highWater {
		t.Fatalf("census modified rollout epoch: %v", err)
	}
	status, err := kv.Status(ctx)
	if err != nil || status.TTL() != 0 {
		t.Fatalf("census changed shared bucket TTL: %v", err)
	}
}

func TestPortCensusRejectsMalformedOrMismatchedRecords(t *testing.T) {
	c, kv := censusFixture(t)
	ctx := t.Context()
	p := testPortCapability("pod-a")
	key, _ := PortCensusKey(p.Principal, p.Instance)
	for _, body := range []string{`{`, `{"version":42}`, strings.Repeat("x", 16*1024+1)} {
		if _, err := kv.PutString(ctx, key, body); err != nil {
			t.Fatal(err)
		}
		if _, err := c.PortCapabilities(ctx); err == nil {
			t.Fatal("malformed census accepted")
		}
	}
	if _, err := c.PublishPortCapability(ctx, p); err != nil {
		t.Fatal(err)
	}
	entry, err := kv.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := kv.Put(ctx, PortCensusPrefix+"other.pod-a", entry.Value()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.PortCapabilities(ctx); err == nil {
		t.Fatal("capability for another principal's key accepted")
	}
}

func TestPortCensusPrunesOnlyRetiredSubjects(t *testing.T) {
	c, kv := censusFixture(t)
	ctx := t.Context()
	if _, err := kv.PutString(ctx, RunnerEpochHighWaterKey, "45"); err != nil {
		t.Fatal(err)
	}
	for _, instance := range []string{"crashed", "stopped"} {
		p := testPortCapability(instance)
		revision, err := c.PublishPortCapability(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		if instance == "stopped" {
			if err := c.DeletePortCapability(ctx, p.Principal, p.Instance, revision); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := c.PrunePortCapabilities(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	entries, err := c.portCensusEntries(ctx)
	if err != nil || len(entries) != 2 {
		t.Fatalf("early collection: %d %v", len(entries), err)
	}
	// Advance only the retention cutoff; this is a GC test, not a claim of
	// a real 24-hour integration soak or proof-freshness validation.
	if err := c.PrunePortCapabilities(ctx, time.Now().Add(PortCapabilityRetention+time.Second)); err != nil {
		t.Fatal(err)
	}
	entries, err = c.portCensusEntries(ctx)
	if err != nil || len(entries) != 0 {
		t.Fatalf("retired values/tombstones retained: %d %v", len(entries), err)
	}
	highWater, err := kv.Get(ctx, RunnerEpochHighWaterKey)
	if err != nil || string(highWater.Value()) != "45" {
		t.Fatalf("GC swept rollout high-water mark: %v", err)
	}
	// The exact sequence bound used by GC must preserve a heartbeat that
	// arrives after its census. Exercise that race on the actual broker.
	p := testPortCapability("restarted")
	_, err = c.PublishPortCapability(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	retired, err := c.portCensusEntries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	newer, err := c.PublishPortCapability(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := PortCensusKey(p.Principal, p.Instance)
	if err := c.prunePortCensusEntries(ctx, retired, time.Now().Add(PortCapabilityRetention+time.Second)); err != nil {
		t.Fatal(err)
	}
	entry, err := kv.Get(ctx, key)
	if err != nil || entry.Revision() != newer {
		t.Fatalf("GC swept concurrently refreshed value: %v", err)
	}
}

func TestPortCensusHeartbeatCleansUpOnCancellation(t *testing.T) {
	c, kv := censusFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	p := testPortCapability("heartbeat")
	key, _ := PortCensusKey(p.Principal, p.Instance)
	watch, err := kv.Watch(t.Context(), key, jetstream.UpdatesOnly())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = watch.Stop() }()
	done := make(chan error, 1)
	go func() { done <- c.RunPortCapabilityHeartbeat(ctx, p, nil) }()
	select {
	case entry := <-watch.Updates():
		if entry == nil || entry.Operation() != jetstream.KeyValuePut {
			t.Fatal("heartbeat did not publish")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("heartbeat never published")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(7 * time.Second):
		t.Fatal("heartbeat did not stop")
	}
	if _, err := kv.Get(t.Context(), key); !errors.Is(err, jetstream.ErrKeyNotFound) {
		t.Fatalf("shutdown kept active census key: %v", err)
	}
}

func TestPortCensusLegacyEnsureSchemaPreservesKeys(t *testing.T) {
	tool := storetest.LegacyTool(t, "ITERION_TEST_LEGACY_PROBE")
	c, kv := censusFixture(t)
	p := testPortCapability("survives-old-schema")
	rev, err := c.PublishPortCapability(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	storetest.RunLegacyTool(t, tool, t.TempDir(), true,
		"--action", "queue-schema", "--nats", c.cfg.URL, "--rollout-bucket", c.cfg.RolloutKVBucket)
	key, _ := PortCensusKey(p.Principal, p.Instance)
	entry, err := kv.Get(t.Context(), key)
	if err != nil || entry.Revision() != rev {
		t.Fatalf("pinned-main EnsureSchema rewrote census key: %v", err)
	}
	status, err := kv.Status(t.Context())
	if err != nil || status.TTL() != 0 || status.History() != 1 {
		t.Fatalf("pinned-main changed census retention: %v", err)
	}
}

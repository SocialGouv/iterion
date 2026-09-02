package nats

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/nats-io/nats.go/jetstream"
)

type recordingSchemaManager struct {
	streams []jetstream.StreamConfig
	kvs     []jetstream.KeyValueConfig
}

func (r *recordingSchemaManager) CreateOrUpdateStream(_ context.Context, cfg jetstream.StreamConfig) (jetstream.Stream, error) {
	r.streams = append(r.streams, cfg)
	return nil, nil
}

func (r *recordingSchemaManager) CreateOrUpdateKeyValue(_ context.Context, cfg jetstream.KeyValueConfig) (jetstream.KeyValue, error) {
	r.kvs = append(r.kvs, cfg)
	return nil, nil
}

// NOTE on coverage scope. The bulk of pkg/queue/nats wraps the NATS
// client + JetStream + KV; meaningful coverage of Connect, EnsureSchema,
// PublishRun, Fetch, AcquireLock, Refresh requires a live JetStream
// broker. CI's nats-conformance job now supplies one for the mixed-schema
// delivery + DLQ lifecycle in pkg/runner/schema_rollout_integration_test.go;
// the ordinary unit suite below still covers the pure / standalone bits:
//   - Config defaults
//   - URL validation in Connect
//   - CancelRun input validation
//   - LeaseInfo JSON shape
//   - Pure helper functions (no nats.Conn needed)

func TestApplyDefaults_PopulatesEverything(t *testing.T) {
	got := applyDefaults(Config{})
	if got.StreamName != StreamRuns {
		t.Errorf("StreamName: got %q want %q", got.StreamName, StreamRuns)
	}
	if got.DLQStream != StreamRunsDLQ {
		t.Errorf("DLQStream: got %q want %q", got.DLQStream, StreamRunsDLQ)
	}
	if got.KVBucket != KVRunLocks {
		t.Errorf("KVBucket: got %q want %q", got.KVBucket, KVRunLocks)
	}
	if got.RolloutKVBucket != KVRolloutEpochs {
		t.Errorf("RolloutKVBucket: got %q want %q", got.RolloutKVBucket, KVRolloutEpochs)
	}
	if got.StreamReplicas != DefaultStreamReplicas {
		t.Errorf("StreamReplicas: got %d want %d", got.StreamReplicas, DefaultStreamReplicas)
	}
	if got.ConsumerName != ConsumerRunners {
		t.Errorf("ConsumerName: got %q want %q", got.ConsumerName, ConsumerRunners)
	}
	if got.MaxAge != DefaultStreamMaxAge {
		t.Errorf("MaxAge: got %v want %v", got.MaxAge, DefaultStreamMaxAge)
	}
	if got.DLQMaxAge != DefaultDLQMaxAge {
		t.Errorf("DLQMaxAge: got %v want %v", got.DLQMaxAge, DefaultDLQMaxAge)
	}
	if got.MaxDeliver != DefaultStreamMaxRetry {
		t.Errorf("MaxDeliver: got %d want %d", got.MaxDeliver, DefaultStreamMaxRetry)
	}
	if got.AckWait != DefaultAckWait {
		t.Errorf("AckWait: got %v want %v", got.AckWait, DefaultAckWait)
	}
	if got.SchemaMismatchDelay != SchemaMismatchNakDelay {
		t.Errorf("SchemaMismatchDelay: got %v want %v", got.SchemaMismatchDelay, SchemaMismatchNakDelay)
	}
	if got.EpochMismatchDelay != EpochMismatchNakDelay {
		t.Errorf("EpochMismatchDelay: got %v want %v", got.EpochMismatchDelay, EpochMismatchNakDelay)
	}
	if got.MaxAckPending != DefaultMaxAckPending {
		t.Errorf("MaxAckPending: got %d want %d", got.MaxAckPending, DefaultMaxAckPending)
	}
	if got.MaxAckPending <= 1 {
		t.Errorf("MaxAckPending default %d must be >1 or the fleet serialises to one run", got.MaxAckPending)
	}
	if got.LockTTL != DefaultLockTTL {
		t.Errorf("LockTTL: got %v want %v", got.LockTTL, DefaultLockTTL)
	}
	if got.Logger == nil {
		t.Error("Logger should be defaulted to a non-nil logger")
	}
}

func TestApplyDefaults_PreservesExplicitValues(t *testing.T) {
	logger := iterlog.New(iterlog.LevelDebug, nil)
	in := Config{
		StreamName:          "X",
		DLQStream:           "Y",
		KVBucket:            "Z",
		RolloutKVBucket:     "R",
		StreamReplicas:      3,
		ConsumerName:        "C",
		MaxAge:              1 * time.Hour,
		DLQMaxAge:           2 * time.Hour,
		MaxDeliver:          42,
		AckWait:             30 * time.Second,
		SchemaMismatchDelay: 45 * time.Second,
		EpochMismatchDelay:  3 * time.Minute,
		RunnerEpoch:         9,
		MaxAckPending:       12,
		LockTTL:             15 * time.Second,
		Logger:              logger,
	}
	got := applyDefaults(in)
	if got != in {
		t.Errorf("explicit fields should be preserved verbatim; got %+v want %+v", got, in)
	}
}

func TestEnsureSchema_ConfiguresReplicas(t *testing.T) {
	recorder := &recordingSchemaManager{}
	cfg := applyDefaults(Config{StreamReplicas: 3})
	if _, err := ensureSchema(context.Background(), recorder, cfg); err != nil {
		t.Fatalf("ensureSchema: %v", err)
	}
	if len(recorder.streams) != 2 {
		t.Fatalf("stream configs: got %d want 2", len(recorder.streams))
	}
	wantStreams := map[string]bool{StreamRuns: true, StreamRunsDLQ: true}
	for _, stream := range recorder.streams {
		if !wantStreams[stream.Name] {
			t.Errorf("unexpected stream config %q", stream.Name)
		}
		delete(wantStreams, stream.Name)
		if stream.Replicas != 3 {
			t.Errorf("stream %s replicas: got %d want 3", stream.Name, stream.Replicas)
		}
	}
	if len(wantStreams) != 0 {
		t.Errorf("missing stream configs: %v", wantStreams)
	}
	if len(recorder.kvs) != 2 {
		t.Fatalf("KV configs: got %d want 2", len(recorder.kvs))
	}
	for _, kv := range recorder.kvs {
		if got := kv.Replicas; got != 3 {
			t.Errorf("KV %s replicas: got %d want 3", kv.Bucket, got)
		}
	}
	if recorder.kvs[0].TTL == 0 {
		t.Errorf("run-lock KV must retain its lease TTL")
	}
	if recorder.kvs[1].TTL != 0 {
		t.Errorf("rollout KV TTL = %s, want no TTL", recorder.kvs[1].TTL)
	}
}

func TestConnect_RejectsInvalidStreamReplicas(t *testing.T) {
	_, err := Connect(context.Background(), Config{URL: "nats://127.0.0.1:4222", StreamReplicas: -1})
	if err == nil || !strings.Contains(err.Error(), "stream replicas -1 invalid") {
		t.Fatalf("Connect error = %v, want invalid stream replicas", err)
	}
}

func TestRedeliveryWindowAccountsForAdmissionDelays(t *testing.T) {
	cases := []struct {
		name   string
		ack    time.Duration
		schema time.Duration
		epoch  time.Duration
		want   time.Duration
	}{
		{"ack wait is larger", 10 * time.Minute, 30 * time.Second, 2 * time.Minute, 80 * time.Minute},
		{"schema delay is larger", time.Minute, 3 * time.Minute, 2 * time.Minute, 24 * time.Minute},
		{"epoch delay is larger", time.Minute, 30 * time.Second, 4 * time.Minute, 32 * time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Conn{cfg: Config{MaxDeliver: 8, AckWait: tc.ack, SchemaMismatchDelay: tc.schema, EpochMismatchDelay: tc.epoch}}
			if got := c.RedeliveryWindow(); got != tc.want {
				t.Errorf("RedeliveryWindow() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRunMessageIDSeparatesLaunchAndResume(t *testing.T) {
	launch := &queue.RunMessage{RunID: "run-1", PublishedAtRFC: "2026-08-25T08:00:00Z"}
	resume := &queue.RunMessage{RunID: "run-1", PublishedAtRFC: "2026-08-25T08:01:00Z", Resume: &queue.ResumeSpec{}}
	if got := runMessageID(launch); got != "run-1" {
		t.Fatalf("launch message id = %q, want bare run id", got)
	}
	if got := runMessageID(resume); got != "run-1|resume-2026-08-25T08:01:00Z" {
		t.Fatalf("resume message id = %q, want per-attempt salt", got)
	}
	// PublishRun stamps PublishedAtRFC only when it is empty, so retrying the
	// same object preserves this id. A genuinely new resume attempt carries a
	// different durable timestamp and must get a different id.
	second := &queue.RunMessage{RunID: "run-1", PublishedAtRFC: "2026-08-25T08:02:00Z", Resume: &queue.ResumeSpec{}}
	if runMessageID(second) == runMessageID(resume) {
		t.Fatalf("two distinct resume attempts share a message id: %q", runMessageID(second))
	}
}

func TestConnect_RejectsEmptyURL(t *testing.T) {
	_, err := Connect(context.Background(), Config{})
	if err == nil {
		t.Fatal("expected URL-required error, got nil")
	}
	if !strings.Contains(err.Error(), "URL is required") {
		t.Errorf("expected URL-required err, got %v", err)
	}
}

func TestCancelRun_RejectsEmptyRunID(t *testing.T) {
	// Conn dereference is guarded by the empty-RunID check, so we can
	// pass a zero-value Conn here.
	c := &Conn{}
	err := c.CancelRun("")
	if err == nil {
		t.Fatal("expected error for empty runID")
	}
	if !strings.Contains(err.Error(), "requires runID") {
		t.Errorf("expected runID-required err, got %v", err)
	}
}

func TestPing_ErrorsWhenNotInitialised(t *testing.T) {
	// Verify Ping handles nil receiver / uninitialised connection
	// gracefully — the /readyz handler calls this during boot before
	// Connect completes on slow brokers.
	var c *Conn
	if err := c.Ping(context.Background()); err == nil {
		t.Error("expected error on nil receiver")
	}
	c = &Conn{}
	if err := c.Ping(context.Background()); err == nil {
		t.Error("expected error when nc is nil")
	}
}

func TestClose_IdempotentOnZeroValue(t *testing.T) {
	// Should not panic on nil receiver / empty conn — Close is in
	// shutdown defer chains; a panic here would mask the real error.
	var c *Conn
	c.Close()
	c = &Conn{}
	c.Close()
}

func TestErrLockHeld_IsSentinel(t *testing.T) {
	// Sanity: errors.Is unwraps wrapped variants — important so the
	// runner's `errors.Is(err, natsq.ErrLockHeld)` branch works after
	// any fmt.Errorf wrap from a higher layer.
	wrapped := wrapErrLockHeld()
	if !errors.Is(wrapped, ErrLockHeld) {
		t.Error("wrapped ErrLockHeld should pass errors.Is")
	}
}

func wrapErrLockHeld() error {
	return errIs("nats: wrapped error: ", ErrLockHeld)
}

// errIs is a tiny helper used by the sentinel test. Pulls in a
// fmt.Errorf("...: %w", inner) without importing fmt at file scope.
func errIs(prefix string, inner error) error {
	return wrappedErr{prefix: prefix, inner: inner}
}

type wrappedErr struct {
	prefix string
	inner  error
}

func (w wrappedErr) Error() string { return w.prefix + w.inner.Error() }
func (w wrappedErr) Unwrap() error { return w.inner }

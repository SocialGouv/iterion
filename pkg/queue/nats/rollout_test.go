package nats

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/queue"
	gonats "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

type memoryEpochKV struct {
	mu           sync.Mutex
	value        []byte
	rev          uint64
	exists       bool
	conflictOnce bool
}

type memoryEpochEntry struct {
	value []byte
	rev   uint64
}

func (e memoryEpochEntry) Bucket() string                  { return KVRolloutEpochs }
func (e memoryEpochEntry) Key() string                     { return RunnerEpochHighWaterKey }
func (e memoryEpochEntry) Value() []byte                   { return append([]byte(nil), e.value...) }
func (e memoryEpochEntry) Revision() uint64                { return e.rev }
func (e memoryEpochEntry) Created() time.Time              { return time.Time{} }
func (e memoryEpochEntry) Delta() uint64                   { return 0 }
func (e memoryEpochEntry) Operation() jetstream.KeyValueOp { return jetstream.KeyValuePut }

func (m *memoryEpochKV) Get(context.Context, string) (jetstream.KeyValueEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.exists {
		return nil, jetstream.ErrKeyNotFound
	}
	return memoryEpochEntry{value: append([]byte(nil), m.value...), rev: m.rev}, nil
}

func (m *memoryEpochKV) Create(_ context.Context, _ string, value []byte, _ ...jetstream.KVCreateOpt) (uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.exists {
		return 0, jetstream.ErrKeyExists
	}
	m.exists = true
	m.rev++
	m.value = append([]byte(nil), value...)
	return m.rev, nil
}

func (m *memoryEpochKV) Update(_ context.Context, _ string, value []byte, revision uint64) (uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.conflictOnce {
		m.conflictOnce = false
		return 0, jetstream.ErrKeyRevisionMismatch
	}
	if !m.exists || revision != m.rev {
		return 0, jetstream.ErrKeyRevisionMismatch
	}
	m.rev++
	m.value = append([]byte(nil), value...)
	return m.rev, nil
}

func TestReconcileRunnerEpochRetriesRevisionConflict(t *testing.T) {
	kv := &memoryEpochKV{value: []byte("1"), rev: 1, exists: true, conflictOnce: true}
	high, stale, err := reconcileRunnerEpoch(context.Background(), kv, 2)
	if err != nil || high != 2 || stale {
		t.Fatalf("reconcile after CAS conflict = high %d stale %t err %v, want 2 false nil", high, stale, err)
	}
}

func TestObserveRunnerEpochDoesNotAdvance(t *testing.T) {
	kv := &memoryEpochKV{value: []byte("1"), rev: 4, exists: true}
	high, stale, err := observeRunnerEpoch(context.Background(), kv, 9)
	if err != nil || high != 1 || stale {
		t.Fatalf("observe = high %d stale %t err %v, want 1 false nil", high, stale, err)
	}
	if string(kv.value) != "1" || kv.rev != 4 {
		t.Fatalf("observe mutated KV to value %q rev %d", kv.value, kv.rev)
	}

	empty := &memoryEpochKV{}
	high, stale, err = observeRunnerEpoch(context.Background(), empty, 9)
	if err != nil || high != 0 || stale || empty.exists {
		t.Fatalf("observe missing = high %d stale %t exists %t err %v, want 0 false false nil", high, stale, empty.exists, err)
	}
}

func TestReconcileRunnerEpochMonotonic(t *testing.T) {
	kv := &memoryEpochKV{}
	for _, tc := range []struct {
		self      uint64
		wantHigh  uint64
		wantStale bool
	}{
		{self: 0, wantHigh: 0},
		{self: 0, wantHigh: 0},
		{self: 2, wantHigh: 2},
		{self: 1, wantHigh: 2, wantStale: true},
	} {
		high, stale, err := reconcileRunnerEpoch(context.Background(), kv, tc.self)
		if err != nil {
			t.Fatalf("self %d: %v", tc.self, err)
		}
		if high != tc.wantHigh || stale != tc.wantStale {
			t.Errorf("self %d => high=%d stale=%t, want high=%d stale=%t", tc.self, high, stale, tc.wantHigh, tc.wantStale)
		}
	}
}

func TestReconcileRunnerEpochConcurrentAdvance(t *testing.T) {
	kv := &memoryEpochKV{}
	if _, _, err := reconcileRunnerEpoch(context.Background(), kv, 1); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errCh := make(chan error, 2)
	for _, epoch := range []uint64{7, 9} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := reconcileRunnerEpoch(context.Background(), kv, epoch)
			errCh <- err
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	high, stale, err := reconcileRunnerEpoch(context.Background(), kv, 9)
	if err != nil || high != 9 || stale {
		t.Fatalf("final reconcile = high %d stale %t err %v, want 9 false nil", high, stale, err)
	}
}

func TestReconcileRunnerEpochRejectsCorruptHighWaterMark(t *testing.T) {
	kv := &memoryEpochKV{value: []byte("not-an-epoch"), rev: 1, exists: true}
	if _, _, err := reconcileRunnerEpoch(context.Background(), kv, 2); err == nil {
		t.Fatal("corrupt high-water mark accepted")
	}
}

func TestStampRunnerEpochAndSupersededFence(t *testing.T) {
	kv := &memoryEpochKV{value: []byte("7"), rev: 1, exists: true}
	conn := &Conn{cfg: Config{RunnerEpoch: 8}, rolloutKV: kv}
	msg := &queue.RunMessage{RunnerEpoch: 1}
	if err := conn.stampRunnerEpoch(msg); !errors.Is(err, ErrRunnerEpochUnclaimed) {
		t.Fatalf("pre-claim stamp error = %v, want ErrRunnerEpochUnclaimed", err)
	}
	if string(kv.value) != "7" {
		t.Fatalf("pre-claim stamp advanced high-water to %q", kv.value)
	}
	if err := conn.ClaimRunnerEpoch(context.Background()); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := conn.stampRunnerEpoch(msg); err != nil {
		t.Fatal(err)
	}
	if msg.RunnerEpoch != 8 {
		t.Fatalf("RunnerEpoch = %d, want central stamp 8", msg.RunnerEpoch)
	}

	conn.superseded = true
	conn.highWaterEpoch = 9
	if err := conn.stampRunnerEpoch(msg); !errors.Is(err, ErrRunnerEpochSuperseded) {
		t.Fatalf("stamp error = %v, want ErrRunnerEpochSuperseded", err)
	}
	if _, err := conn.NewConsumer(context.Background()); !errors.Is(err, ErrRunnerEpochSuperseded) {
		t.Fatalf("consumer error = %v, want ErrRunnerEpochSuperseded", err)
	}
	conn.nc = &gonats.Conn{}
	if _, err := conn.PublishRun(context.Background(), msg); !errors.Is(err, ErrRunnerEpochSuperseded) {
		t.Fatalf("publish error = %v, want ErrRunnerEpochSuperseded", err)
	}
	if _, err := conn.RepublishDLQ(context.Background(), 1); !errors.Is(err, ErrRunnerEpochSuperseded) {
		t.Fatalf("DLQ replay error = %v, want ErrRunnerEpochSuperseded", err)
	}
}

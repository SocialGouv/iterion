package nats

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/SocialGouv/iterion/pkg/internal/appinfo"
	"github.com/SocialGouv/iterion/pkg/queue"
)

// memoryPresenceKV is an in-memory stand-in for the registry bucket.
type memoryPresenceKV struct {
	mu      sync.Mutex
	values  map[string][]byte
	created map[string]time.Time
	puts    int
	putErr  error
	// blockFirstPut, when set, holds the first Put until released — to
	// order two concurrent writes deterministically.
	blockFirstPut chan struct{}
	firstPutIn    chan struct{}
	// listed overrides the listing (keys gone by the read); listErr fails it.
	listed  []string
	listErr error
	// getErr fails the read of one key.
	getErr map[string]error
}

func newMemoryPresenceKV() *memoryPresenceKV {
	return &memoryPresenceKV{values: map[string][]byte{}, created: map[string]time.Time{}}
}

func (m *memoryPresenceKV) Put(_ context.Context, key string, value []byte) (uint64, error) {
	m.mu.Lock()
	m.puts++
	first := m.puts == 1
	block, in := m.blockFirstPut, m.firstPutIn
	err := m.putErr
	m.mu.Unlock()
	if first && block != nil {
		close(in)
		<-block
	}
	if err != nil {
		return 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.values[key] = append([]byte(nil), value...)
	m.created[key] = time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	return uint64(m.puts), nil
}

func (m *memoryPresenceKV) Delete(_ context.Context, key string, _ ...jetstream.KVDeleteOpt) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.values, key)
	return nil
}

func (m *memoryPresenceKV) Get(_ context.Context, key string) (jetstream.KeyValueEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.getErr[key]; err != nil {
		return nil, err
	}
	v, ok := m.values[key]
	if !ok {
		return nil, jetstream.ErrKeyNotFound
	}
	return presenceEntry{key: key, value: append([]byte(nil), v...), created: m.created[key]}, nil
}

// keys is the registry's key lister over this memory bucket.
func (m *memoryPresenceKV) keys(context.Context) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.listErr != nil {
		return nil, m.listErr
	}
	if m.listed != nil {
		return append([]string(nil), m.listed...), nil
	}
	var keys []string
	for k := range m.values {
		keys = append(keys, k)
	}
	return keys, nil
}

func (m *memoryPresenceKV) stored(t *testing.T, key string) queue.FleetMember {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.values[key]
	if !ok {
		t.Fatalf("no record stored under %q", key)
	}
	var member queue.FleetMember
	if err := json.Unmarshal(v, &member); err != nil {
		t.Fatal(err)
	}
	return member
}

type presenceEntry struct {
	key     string
	value   []byte
	created time.Time
}

func (e presenceEntry) Bucket() string                  { return KVFleetPresence }
func (e presenceEntry) Key() string                     { return e.key }
func (e presenceEntry) Value() []byte                   { return e.value }
func (e presenceEntry) Revision() uint64                { return 1 }
func (e presenceEntry) Created() time.Time              { return e.created }
func (e presenceEntry) Delta() uint64                   { return 0 }
func (e presenceEntry) Operation() jetstream.KeyValueOp { return jetstream.KeyValuePut }

func testPresence(kv presenceKV) (*Presence, *time.Time) {
	now := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	p := newPresence(kv, queue.FleetMember{Role: queue.FleetRoleRunner, Instance: "pod-1-abcd", Build: "v7.40.3", Epoch: 54}, time.Minute, nil)
	p.now = func() time.Time { return now }
	return p, &now
}

// Fresh means "known to be in the registry with a heartbeat of margin": only
// after a write that landed, and only until ttl - interval after it was sent.
func TestPresence_FreshOnlyWithinTheTTLMarginOfALandedWrite(t *testing.T) {
	kv := newMemoryPresenceKV()
	p, now := testPresence(kv)
	if p.Fresh() {
		t.Fatal("fresh before any write")
	}
	if err := p.Register(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !p.Fresh() {
		t.Fatal("not fresh right after the registration landed")
	}
	*now = now.Add(45*time.Second - time.Nanosecond) // ttl 60 s, interval 15 s
	if !p.Fresh() {
		t.Fatal("not fresh just inside the margin")
	}
	*now = now.Add(time.Nanosecond)
	if p.Fresh() {
		t.Fatal("still fresh at ttl - interval: the record may expire before a fetch started now returns")
	}
}

// A write that failed does not refresh the margin: only a landed one does.
func TestPresence_AFailedWriteDoesNotRefreshTheMargin(t *testing.T) {
	kv := newMemoryPresenceKV()
	p, now := testPresence(kv)
	if err := p.Register(context.Background()); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(40 * time.Second)
	kv.putErr = errors.New("nats: timeout")
	if err := p.write(context.Background()); err == nil {
		t.Fatal("the failed write reported success")
	}
	*now = now.Add(5 * time.Second)
	if p.Fresh() {
		t.Fatal("a failed heartbeat kept the record fresh")
	}
	if err := p.Register(context.Background()); err == nil || !strings.Contains(err.Error(), "register runner.pod-1-abcd") {
		t.Fatalf("Register = %v, want the failure naming the record", err)
	}
}

func TestPresence_MarkDrainingIsStored(t *testing.T) {
	kv := newMemoryPresenceKV()
	p, _ := testPresence(kv)
	if err := p.Register(context.Background()); err != nil {
		t.Fatal(err)
	}
	if kv.stored(t, "runner.pod-1-abcd").Draining {
		t.Fatal("registered as draining")
	}
	if err := p.MarkDraining(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := kv.stored(t, "runner.pod-1-abcd"); !got.Draining || got.Build != "v7.40.3" || got.Epoch != 54 {
		t.Fatalf("stored %+v, want the same record marked draining", got)
	}
}

// A draining mark whose write failed is not lost: the record keeps the
// mark, and the next heartbeat that lands carries it.
func TestPresence_AFailedDrainingMarkRidesTheNextHeartbeat(t *testing.T) {
	kv := newMemoryPresenceKV()
	p, _ := testPresence(kv)
	if err := p.Register(context.Background()); err != nil {
		t.Fatal(err)
	}
	kv.mu.Lock()
	kv.putErr = errors.New("nats: timeout")
	kv.mu.Unlock()
	if err := p.MarkDraining(context.Background()); err == nil {
		t.Fatal("the failed mark reported success")
	}
	if kv.stored(t, "runner.pod-1-abcd").Draining {
		t.Fatal("the failed write landed")
	}
	kv.mu.Lock()
	kv.putErr = nil
	kv.mu.Unlock()
	if err := p.write(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !kv.stored(t, "runner.pod-1-abcd").Draining {
		t.Fatal("the next heartbeat did not carry the draining mark")
	}
}

// A heartbeat that read the record before MarkDraining must not land after
// it with the old state: the write that lands last carries the newest one.
func TestPresence_AHeartbeatCannotUndoTheDrainingMark(t *testing.T) {
	kv := newMemoryPresenceKV()
	kv.blockFirstPut = make(chan struct{})
	kv.firstPutIn = make(chan struct{})
	p, _ := testPresence(kv)

	heartbeat := make(chan error, 1)
	go func() { heartbeat <- p.write(context.Background()) }()
	<-kv.firstPutIn // the heartbeat holds its snapshot (not draining), mid-Put

	marked := make(chan error, 1)
	go func() { marked <- p.MarkDraining(context.Background()) }()
	// The window an unserialised MarkDraining needs to land its write
	// before the held heartbeat does; serialised, it waits on the heartbeat
	// whatever the window.
	time.Sleep(50 * time.Millisecond)
	close(kv.blockFirstPut)
	if err := <-heartbeat; err != nil {
		t.Fatal(err)
	}
	if err := <-marked; err != nil {
		t.Fatal(err)
	}
	if !kv.stored(t, "runner.pod-1-abcd").Draining {
		t.Fatal("the stale heartbeat landed after the draining mark and undid it")
	}
}

// A write waiting behind a slow one honours its own deadline: the draining
// mark of a stopping runner must not outlive its timeout behind a heartbeat
// stuck on the broker.
func TestPresence_AWriteHonoursItsDeadlineBehindAHeldWrite(t *testing.T) {
	kv := newMemoryPresenceKV()
	kv.blockFirstPut = make(chan struct{})
	kv.firstPutIn = make(chan struct{})
	p, _ := testPresence(kv)
	go func() { _ = p.write(context.Background()) }()
	<-kv.firstPutIn
	defer close(kv.blockFirstPut)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := p.MarkDraining(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("MarkDraining = %v, want its deadline", err)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("MarkDraining returned after %s with a 50ms deadline", took)
	}
}

func TestPresence_NoWriteLandsAfterDeregister(t *testing.T) {
	kv := newMemoryPresenceKV()
	p, _ := testPresence(kv)
	if err := p.Register(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := p.Deregister(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := p.write(context.Background()); !errors.Is(err, errPresenceClosed) {
		t.Fatalf("write after Deregister = %v, want errPresenceClosed", err)
	}
	if _, err := kv.Get(context.Background(), "runner.pod-1-abcd"); !errors.Is(err, jetstream.ErrKeyNotFound) {
		t.Fatalf("record after Deregister: %v, want gone", err)
	}
	// An idle runner's exit can deregister before Shutdown marks it: the
	// mark is then a no-op, not a failure, and writes nothing back.
	if err := p.MarkDraining(context.Background()); err != nil {
		t.Fatalf("MarkDraining after Deregister = %v, want nil", err)
	}
	if _, err := kv.Get(context.Background(), "runner.pod-1-abcd"); !errors.Is(err, jetstream.ErrKeyNotFound) {
		t.Fatalf("record after a late MarkDraining: %v, want still gone", err)
	}
}

// Run refreshes the record until its context ends, then stops writing.
func TestPresence_RunHeartbeatsUntilCancelled(t *testing.T) {
	kv := newMemoryPresenceKV()
	p := newPresence(kv, queue.FleetMember{Role: queue.FleetRoleServer, Instance: "srv"}, 20*time.Millisecond, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		kv.mu.Lock()
		puts := kv.puts
		kv.mu.Unlock()
		if puts >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("heartbeat wrote %d times in 5 s, want ≥ 3", puts)
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	kv.mu.Lock()
	after := kv.puts
	kv.mu.Unlock()
	time.Sleep(30 * time.Millisecond)
	kv.mu.Lock()
	defer kv.mu.Unlock()
	if kv.puts != after {
		t.Fatalf("heartbeat kept writing after cancel: %d → %d", after, kv.puts)
	}
}

func TestPresenceKey_KeepsTwoValidTokens(t *testing.T) {
	if got := presenceKey("runner", "iterion-runner-646cb5cc85-5mct9.ns/x y"); got != "runner.iterion-runner-646cb5cc85-5mct9_ns_x_y" {
		t.Fatalf("key = %q", got)
	}
}

func TestNewPresence(t *testing.T) {
	kv := newMemoryPresenceKV()
	c := &Conn{presenceKV: kv, cfg: applyDefaults(Config{RunnerEpoch: 54})}
	if _, err := c.NewPresence("scheduler", "x", ""); err == nil {
		t.Fatal("an unknown role was accepted")
	}
	if _, err := (&Conn{}).NewPresence(queue.FleetRoleRunner, "x", ""); err == nil {
		t.Fatal("a connection without the registry built a record")
	}
	a, err := c.NewPresence(queue.FleetRoleRunner, "pod-1", "egapro")
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.NewPresence(queue.FleetRoleRunner, "pod-1", "egapro")
	if err != nil {
		t.Fatal(err)
	}
	if a.key == b.key {
		t.Fatalf("two processes on one host share the key %q", a.key)
	}
	m := a.Member()
	if !strings.HasPrefix(m.Instance, "pod-1-") || m.Epoch != 54 || m.Pool != "egapro" || m.Build != appinfo.FullVersion() || m.StartedAt.IsZero() {
		t.Fatalf("member = %+v, want the host, the conn's epoch, the pool and this binary's build", m)
	}
}

func TestReadFleet(t *testing.T) {
	kv := newMemoryPresenceKV()
	for _, m := range []queue.FleetMember{
		{Role: queue.FleetRoleServer, Instance: "s1", Build: "v7.40.3"},
		{Role: queue.FleetRoleRunner, Instance: "r1", Build: "v7.40.3", Draining: true},
	} {
		p := newPresence(kv, m, time.Minute, nil)
		if err := p.Register(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	// A key gone between the listing and the read (expired, deregistered).
	kv.listed = []string{"server.s1", "runner.r1", "runner.gone"}
	got, err := readFleet(context.Background(), kv, kv.keys)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Instance != "r1" || !got[0].Draining || got[1].Instance != "s1" || got[1].SeenAt.IsZero() {
		t.Fatalf("fleet = %+v, want r1 (draining) then s1, sorted, with SeenAt", got)
	}

	kv.getErr = map[string]error{"runner.r1": errors.New("nats: timeout")}
	if _, err := readFleet(context.Background(), kv, kv.keys); err == nil || !strings.Contains(err.Error(), "runner.r1") {
		t.Fatalf("readFleet with an unreadable record = %v, want the record named", err)
	}
	kv.getErr = nil

	kv.mu.Lock()
	kv.values["runner.bad"] = []byte("{not json")
	kv.mu.Unlock()
	kv.listed = nil
	if _, err := readFleet(context.Background(), kv, kv.keys); err == nil || !strings.Contains(err.Error(), "decode fleet record runner.bad") {
		t.Fatalf("readFleet with a malformed record = %v, want a decode error", err)
	}
}

// The listing is a request/response: its error fails the whole read rather
// than yielding the part already received.
func TestReadFleet_AListingErrorFailsTheRead(t *testing.T) {
	kv := newMemoryPresenceKV()
	p := newPresence(kv, queue.FleetMember{Role: queue.FleetRoleRunner, Instance: "a"}, time.Minute, nil)
	if err := p.Register(context.Background()); err != nil {
		t.Fatal(err)
	}
	kv.listErr = errors.New("nats: connection closed")
	if got, err := readFleet(context.Background(), kv, kv.keys); err == nil || !strings.Contains(err.Error(), "connection closed") {
		t.Fatalf("readFleet = %+v, %v; want the listing error", got, err)
	}
}

// A push request carries no deadline: the registry read sets its own, so a
// registry that does not answer is an error, never a hung push.
func TestFleetMembers_BoundsAReadWithoutDeadline(t *testing.T) {
	kv := newMemoryPresenceKV()
	var deadline time.Time
	var hasDeadline bool
	c := &Conn{presenceKV: kv, presenceKeys: func(ctx context.Context) ([]string, error) {
		deadline, hasDeadline = ctx.Deadline()
		return nil, nil
	}}
	if _, err := c.FleetMembers(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !hasDeadline || time.Until(deadline) > fleetReadTimeout {
		t.Fatalf("listing ran with deadline=%v (set=%v), want one within %s", deadline, hasDeadline, fleetReadTimeout)
	}
}

// The lister asks the bucket's backing stream for every subject of the
// bucket — the stream and the filter the client's KV layout uses — and
// passes an error through rather than an empty fleet.
func TestStreamKeyLister_AsksTheBucketsStreamForEverySubject(t *testing.T) {
	var gotStream, gotFilter string
	list := streamKeyLister(func(_ context.Context, stream, filter string) (map[string]uint64, error) {
		gotStream, gotFilter = stream, filter
		return map[string]uint64{"$KV.iterion-fleet-presence.runner.pod-1-ab": 1}, nil
	}, KVFleetPresence)
	keys, err := list(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if gotStream != "KV_iterion-fleet-presence" || gotFilter != "$KV.iterion-fleet-presence.>" {
		t.Fatalf("asked stream %q with filter %q, want KV_iterion-fleet-presence and $KV.iterion-fleet-presence.>", gotStream, gotFilter)
	}
	if strings.Join(keys, ",") != "runner.pod-1-ab" {
		t.Fatalf("keys = %q", keys)
	}
	failing := streamKeyLister(func(context.Context, string, string) (map[string]uint64, error) {
		return nil, errors.New("nats: stream not found")
	}, KVFleetPresence)
	if keys, err := failing(context.Background()); err == nil {
		t.Fatalf("a failed stream read listed %q, want the error", keys)
	}
}

func TestKeysOfSubjects(t *testing.T) {
	got := keysOfSubjects(KVFleetPresence, map[string]uint64{
		"$KV.iterion-fleet-presence.runner.pod-1-ab": 1,
		"$KV.iterion-fleet-presence.server.srv-cd":   1,
		"$KV.iterion-fleet-presence.":                1,
		"$KV.iterion-run-locks.run-1":                1,
	})
	sort.Strings(got)
	if strings.Join(got, ",") != "runner.pod-1-ab,server.srv-cd" {
		t.Fatalf("keys = %q, want the registry's two keys only", got)
	}
}

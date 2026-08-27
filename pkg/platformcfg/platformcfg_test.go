package platformcfg

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func strp(s string) *string { return &s }

func TestBotRolesValidate(t *testing.T) {
	if err := (BotRoles{Reviewer: strp("my-reviewer"), Brancher: strp("billy_2")}).Validate(); err != nil {
		t.Fatalf("valid roles: %v", err)
	}
	for _, bad := range []string{"", "Has-Upper", "a/b", strings.Repeat("x", 65)} {
		if err := (BotRoles{Implementer: strp(bad)}).Validate(); err == nil {
			t.Errorf("id %q must be rejected", bad)
		}
	}
}

func TestSandboxValidate(t *testing.T) {
	if err := (Sandbox{DefaultImage: strp("ghcr.io/x/y@sha256:abc")}).Validate(); err != nil {
		t.Fatalf("valid image: %v", err)
	}
	if err := (Sandbox{DefaultImage: strp("  ")}).Validate(); err == nil {
		t.Error("blank override must be rejected (clear = nil, never empty string)")
	}
	if err := (Sandbox{}).Validate(); err != nil {
		t.Errorf("no override is valid: %v", err)
	}
}

// failingStore errors on every read after an initial success — the outage
// shape the resolver must serve last-known values through.
type failingStore struct {
	rec  *BotRoles
	fail bool
}

func (f *failingStore) Get(context.Context) (*BotRoles, error) {
	if f.fail {
		return nil, errors.New("mongo down")
	}
	return f.rec, nil
}
func (f *failingStore) Put(_ context.Context, rec BotRoles) error { f.rec = &rec; return nil }

func TestResolver_InvalidateAndServeStale(t *testing.T) {
	st := &failingStore{}
	warned := 0
	r := NewResolver[BotRoles](st, func(string, ...any) { warned++ })

	if got := r.Get(context.Background()); got != nil {
		t.Fatalf("empty store must resolve nil, got %+v", got)
	}
	// A write + Invalidate must be visible immediately (no TTL wait).
	if err := st.Put(context.Background(), BotRoles{Reviewer: strp("alt")}); err != nil {
		t.Fatal(err)
	}
	r.Invalidate()
	got := r.Get(context.Background())
	if got == nil || got.Reviewer == nil || *got.Reviewer != "alt" {
		t.Fatalf("post-invalidate read = %+v, want the fresh write", got)
	}
	// An outage serves the LAST-KNOWN value, logged — availability over
	// freshness (the ADR-090 posture).
	st.fail = true
	r.Invalidate()
	got = r.Get(context.Background())
	if got == nil || got.Reviewer == nil || *got.Reviewer != "alt" {
		t.Fatalf("outage read = %+v, want the last-known value", got)
	}
	if warned == 0 {
		t.Error("the degraded read must be logged")
	}
}

// The memory store round-trips records.
func TestMemoryStore(t *testing.T) {
	m := NewMemoryStore[Sandbox]()
	if rec, err := m.Get(context.Background()); err != nil || rec != nil {
		t.Fatalf("empty store: %v %v", rec, err)
	}
	if err := m.Put(context.Background(), Sandbox{DefaultImage: strp("img")}); err != nil {
		t.Fatal(err)
	}
	rec, err := m.Get(context.Background())
	if err != nil || rec == nil || *rec.DefaultImage != "img" {
		t.Fatalf("round trip: %+v %v", rec, err)
	}
}

// slowStore delays reads so tests can hold a refresh in flight.
type slowStore struct {
	mu    sync.Mutex
	rec   *BotRoles
	delay time.Duration
	gate  chan struct{} // when non-nil, Get blocks until closed
	reads int
}

func (s *slowStore) Get(ctx context.Context) (*BotRoles, error) {
	s.mu.Lock()
	s.reads++
	rec, gate, delay := s.rec, s.gate, s.delay
	s.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if rec == nil {
		return nil, nil
	}
	cp := *rec
	return &cp, nil
}
func (s *slowStore) Put(_ context.Context, rec BotRoles) error {
	s.mu.Lock()
	s.rec = &rec
	s.mu.Unlock()
	return nil
}

// Cold start must not silently serve defaults: concurrent Gets during the
// FIRST read wait for it (bounded) instead of resolving nil.
func TestResolver_ColdStartWaitsForFirstRead(t *testing.T) {
	st := &slowStore{rec: &BotRoles{Reviewer: strp("override")}, delay: 30 * time.Millisecond}
	r := NewResolver[BotRoles](st, nil)

	const n = 25
	results := make(chan *BotRoles, n)
	for i := 0; i < n; i++ {
		go func() { results <- r.Get(context.Background()) }()
	}
	for i := 0; i < n; i++ {
		got := <-results
		if got == nil || got.Reviewer == nil || *got.Reviewer != "override" {
			t.Fatalf("cold-start Get resolved %+v — a caller was served defaults while the first read was in flight", got)
		}
	}
}

// A refresh in flight when Invalidate lands must DISCARD its pre-write
// result: the next Get re-reads and sees the mutation, instead of the
// stale value being re-pinned for a full TTL.
func TestResolver_InvalidateDiscardsInFlightRefresh(t *testing.T) {
	gate := make(chan struct{})
	st := &slowStore{rec: &BotRoles{Reviewer: strp("old")}, gate: gate}
	r := NewResolver[BotRoles](st, nil)

	done := make(chan *BotRoles, 1)
	go func() { done <- r.Get(context.Background()) }() // refresh starts, blocked on gate
	time.Sleep(10 * time.Millisecond)

	// The write + invalidate land while the pre-write read is in flight.
	if err := st.Put(context.Background(), BotRoles{Reviewer: strp("new")}); err != nil {
		t.Fatal(err)
	}
	r.Invalidate()
	close(gate)
	<-done

	st.mu.Lock()
	st.gate = nil
	st.mu.Unlock()
	got := r.Get(context.Background())
	if got == nil || got.Reviewer == nil || *got.Reviewer != "new" {
		t.Fatalf("post-invalidate Get = %+v — the in-flight pre-write result was re-pinned over the mutation", got)
	}
}

// One caller's cancelled context must not fail — and TTL-poison — the
// shared refresh for everyone behind it.
func TestResolver_CallerCancellationDoesNotPoisonCache(t *testing.T) {
	// The delay forces the fake store through its ctx-aware wait, so a
	// refresh inheriting the cancelled ctx FAILS (the defect) while a
	// detached one succeeds.
	st := &slowStore{rec: &BotRoles{Reviewer: strp("override")}, delay: 5 * time.Millisecond}
	r := NewResolver[BotRoles](st, nil)

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_ = r.Get(cancelled) // the refresh itself must run detached and commit

	got := r.Get(context.Background())
	if got == nil || got.Reviewer == nil || *got.Reviewer != "override" {
		t.Fatalf("Get after a cancelled caller = %+v — the aborted request poisoned the cache", got)
	}
	st.mu.Lock()
	reads := st.reads
	st.mu.Unlock()
	if reads != 1 {
		t.Fatalf("store reads = %d, want 1 (the detached refresh committed on the first call)", reads)
	}
}

// Put stamps UpdatedAt in the store — every writer covered, no handler
// duplication.
func TestPut_StampsUpdatedAt(t *testing.T) {
	m := NewMemoryStore[Sandbox]()
	if err := m.Put(context.Background(), Sandbox{DefaultImage: strp("img")}); err != nil {
		t.Fatal(err)
	}
	rec, _ := m.Get(context.Background())
	if rec.UpdatedAt.IsZero() {
		t.Fatal("UpdatedAt not stamped on Put")
	}
}

// panicStore panics once on Get — the shape of a malformed operator-pushed
// bundle blowing up inside the platform-bots fetch (DSL/YAML parse).
type panicStore struct {
	mu       sync.Mutex
	panicked bool
	rec      *BotRoles
}

func (s *panicStore) Get(context.Context) (*BotRoles, error) {
	s.mu.Lock()
	first := !s.panicked
	s.panicked = true
	s.mu.Unlock()
	if first {
		panic("malformed bundle content")
	}
	cp := *s.rec
	return &cp, nil
}
func (s *panicStore) Put(context.Context, BotRoles) error { return nil }

// A panicking store read must not wedge the resolver: without the recover,
// refreshing stays true and waiters never close — every later Get (all on
// context.Background in production) blocks forever.
func TestResolver_PanicInFetchDoesNotWedge(t *testing.T) {
	st := &panicStore{rec: &BotRoles{Reviewer: strp("override")}}
	warned := 0
	r := NewResolver[BotRoles](st, func(string, ...any) { warned++ })
	r.ttl = time.Millisecond

	_ = r.Get(context.Background()) // panics inside; must return, not wedge

	done := make(chan *BotRoles, 1)
	go func() {
		time.Sleep(5 * time.Millisecond) // let the TTL window pass
		done <- r.Get(context.Background())
	}()
	select {
	case got := <-done:
		if got == nil || got.Reviewer == nil || *got.Reviewer != "override" {
			t.Fatalf("post-panic Get = %+v, want the second (healthy) read", got)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("resolver WEDGED after a panicking store read")
	}
	if warned == 0 {
		t.Error("the panic must be logged as a failed read")
	}
}

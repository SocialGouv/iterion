package platformcfg

import (
	"context"
	"errors"
	"strings"
	"testing"
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

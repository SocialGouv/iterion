package mongo

import (
	"context"
	"os"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
)

// newTTLTestStore builds a Mongo store with a non-zero EventsTTLDays
// against a throwaway database, or skips when ITERION_TEST_MONGO_URI is
// unset — same gate as the plan-store suite.
func newTTLTestStore(t *testing.T, ttlDays int) *Store {
	t.Helper()
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping Mongo TTL schema test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	s, err := New(ctx, Config{
		URI:           uri,
		Database:      "iterion_ttl_" + bsonNonce(t),
		Blob:          newInMemoryBlob(),
		EventsTTLDays: ttlDays,
	})
	if err != nil {
		t.Fatalf("mongo New: %v", err)
	}
	t.Cleanup(func() {
		drop, dcancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer dcancel()
		_ = s.db.Drop(drop)
		_ = s.Close(drop)
	})
	return s
}

// ttlSeconds returns the expireAfterSeconds of the named index on coll,
// or (0, false) when no such index exists (or it carries no TTL).
func ttlSeconds(t *testing.T, coll *mongo.Collection, name string) (int32, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	specs, err := coll.Indexes().ListSpecifications(ctx)
	if err != nil {
		t.Fatalf("list index specifications: %v", err)
	}
	for _, spec := range specs {
		if spec.Name == name {
			if spec.ExpireAfterSeconds == nil {
				return 0, false
			}
			return *spec.ExpireAfterSeconds, true
		}
	}
	return 0, false
}

// TestEnsureSchema_RunPlansTTL is the core of the feature: after
// EnsureSchema with a non-zero retention knob, run_plans carries a TTL
// index on `ts` with exactly the same expireAfterSeconds as its sibling
// derived-observability streams (events, run_logs). Plan snapshots are
// N-docs-per-run like events; they must expire on the same knob so a
// deleted run leaves no orphaned snapshots long after its events are gone.
func TestEnsureSchema_RunPlansTTL(t *testing.T) {
	const ttlDays = 7
	s := newTTLTestStore(t, ttlDays)
	wantSecs := int32(ttlDays * 86400)

	got, ok := ttlSeconds(t, s.runPlans, "run_plans_ttl")
	if !ok {
		t.Fatalf("run_plans has no run_plans_ttl index after EnsureSchema")
	}
	if got != wantSecs {
		t.Errorf("run_plans_ttl expireAfterSeconds = %d, want %d", got, wantSecs)
	}

	// Parity with the sibling streams: identical TTL on the same knob.
	for _, sib := range []struct {
		coll *mongo.Collection
		name string
	}{
		{s.events, "events_ttl"},
		{s.runLogs, "run_logs_ttl"},
	} {
		sibSecs, sibOK := ttlSeconds(t, sib.coll, sib.name)
		if !sibOK {
			t.Fatalf("%s missing after EnsureSchema", sib.name)
		}
		if sibSecs != got {
			t.Errorf("%s expireAfterSeconds = %d, want %d (parity with run_plans_ttl)", sib.name, sibSecs, got)
		}
	}
}

// TestEnsureSchema_RunPlansTTLDisabled verifies the knob is honoured:
// EventsTTLDays==0 leaves run_plans (and its siblings) with no TTL index,
// so snapshots persist until explicitly deleted.
func TestEnsureSchema_RunPlansTTLDisabled(t *testing.T) {
	s := newTTLTestStore(t, 0)
	if _, ok := ttlSeconds(t, s.runPlans, "run_plans_ttl"); ok {
		t.Errorf("run_plans_ttl present with EventsTTLDays=0; TTL should be disabled")
	}
}

// TestEnsureSchema_RunTurnsTTL is the TurnStore twin of the run_plans TTL
// test: per-LLM-turn checkpoints are an N-docs-per-run derived stream too,
// so run_turns must carry a TTL on `ts` with the same expireAfterSeconds
// as its sibling streams — a deleted run leaves no orphaned turns lingering
// past the events retention window.
func TestEnsureSchema_RunTurnsTTL(t *testing.T) {
	const ttlDays = 7
	s := newTTLTestStore(t, ttlDays)
	wantSecs := int32(ttlDays * 86400)

	got, ok := ttlSeconds(t, s.runTurns, "run_turns_ttl")
	if !ok {
		t.Fatalf("run_turns has no run_turns_ttl index after EnsureSchema")
	}
	if got != wantSecs {
		t.Errorf("run_turns_ttl expireAfterSeconds = %d, want %d", got, wantSecs)
	}
	// Parity with events (both retain on the same eventsTTLDays knob).
	sibSecs, sibOK := ttlSeconds(t, s.events, "events_ttl")
	if !sibOK {
		t.Fatalf("events_ttl missing after EnsureSchema")
	}
	if sibSecs != got {
		t.Errorf("events_ttl expireAfterSeconds = %d, want %d (parity with run_turns_ttl)", sibSecs, got)
	}
}

// TestEnsureSchema_RunTurnsTTLDisabled: EventsTTLDays==0 leaves run_turns
// with no TTL index.
func TestEnsureSchema_RunTurnsTTLDisabled(t *testing.T) {
	s := newTTLTestStore(t, 0)
	if _, ok := ttlSeconds(t, s.runTurns, "run_turns_ttl"); ok {
		t.Errorf("run_turns_ttl present with EventsTTLDays=0; TTL should be disabled")
	}
}

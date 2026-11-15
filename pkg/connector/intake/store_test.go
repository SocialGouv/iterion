package intake

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
)

// An intake and its pairs leave together at their expiry, on the twins that
// expire rows themselves. (Mongo's TTL monitor runs on its own minute; its
// index is checked below.)
func TestExpiredRowsAreDropped(t *testing.T) {
	ctx := context.Background()
	clock := t0
	stores := map[string]func() Store{
		"memory": func() Store { m := NewMemoryStore(); m.now = func() time.Time { return clock }; return m },
		"file": func() Store {
			f := NewFileStore(filepath.Join(t.TempDir(), "intake.json"))
			f.now = func() time.Time { return clock }
			return f
		},
	}
	for name, open := range stores {
		t.Run(name, func(t *testing.T) {
			clock = t0
			s := open()
			kept := delivery("t1", "ing-1", "kept", t0)
			gone := delivery("t1", "ing-1", "gone", t0)
			gone.ExpiresAt = ExpiryFor(t0, time.Hour)
			k := mustAccept(t, s, kept)
			g := mustAccept(t, s, gone)
			p := pending(g, "s1", t0)
			p.ExpiresAt = g.ExpiresAt
			if err := s.WritePairs(ctx, []Pair{p, pending(k, "s1", t0)}); err != nil {
				t.Fatal(err)
			}
			clock = t0.Add(59 * time.Minute)
			if _, err := s.GetIntake(ctx, "t1", g.ID); err != nil {
				t.Fatalf("before its expiry the row is gone: %v", err)
			}
			clock = t0.Add(time.Hour)
			rows, _ := s.ListIntakes(ctx, "t1", "", 0)
			if len(rows) != 1 || rows[0].ID != k.ID {
				t.Fatalf("intakes at the expiry = %v, want only the one without an expiry", intakeIDs(rows))
			}
			pairs, _ := s.ListPairs(ctx, "t1", PairQuery{}, 0)
			if len(pairs) != 1 || pairs[0].IntakeID != k.ID {
				t.Fatalf("pairs at the expiry = %v, want only the kept intake's", pairSubs(pairs))
			}
		})
	}
}

// The Mongo twin expires rows through TTL indexes on expires_at with no
// delay: a row leaves at its own instant, and one without the field never
// does.
func TestMongoExpiryIsATTLIndex(t *testing.T) {
	db := mongoDB(t)
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()
	for col, name := range map[string]string{colIntakes: "intake_expiry", colPairs: "pair_expiry"} {
		cur, err := db.Collection(col).Indexes().List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var specs []bson.M
		if err := cur.All(ctx, &specs); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, s := range specs {
			if s["name"] != name {
				continue
			}
			found = true
			key, _ := s["key"].(bson.D)
			if len(key) != 1 || key[0].Key != "expires_at" {
				t.Fatalf("%s.%s keys %v, want expires_at alone", col, name, s["key"])
			}
			if ttl, ok := s["expireAfterSeconds"]; !ok || toInt(ttl) != 0 {
				t.Fatalf("%s.%s expireAfterSeconds = %v, want 0", col, name, ttl)
			}
		}
		if !found {
			t.Fatalf("%s has no %s index: %v", col, name, specs)
		}
	}
}

func toInt(v any) int64 {
	switch n := v.(type) {
	case int32:
		return int64(n)
	case int64:
		return n
	case float64:
		return int64(n)
	}
	return -1
}

// A local server that restarts reopens its journal and resumes from it:
// pending work is claimable, and a claim the dead process held expires like
// any other.
func TestFileStoreResumesAfterARestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "connector", "intake.json")
	before := NewFileStore(path)
	a := mustAccept(t, before, delivery("t1", "ing-1", "a", t0))
	b := mustAccept(t, before, delivery("t1", "ing-1", "b", t0))
	held, err := before.ClaimIntakes(ctx, "dead-process", t0, ttl, 1)
	if err != nil || len(held) != 1 || held[0].ID != minID(a.ID, b.ID) {
		t.Fatalf("pre-restart claim = (%v, %v)", intakeIDs(held), err)
	}

	after := NewFileStore(path)
	got, err := after.ClaimIntakes(ctx, "next-process", t0.Add(ttl/2), ttl, 10)
	if err != nil || len(got) != 1 || got[0].ID != maxID(a.ID, b.ID) {
		t.Fatalf("claim after the restart = (%v, %v), want the pending row only", intakeIDs(got), err)
	}
	got, err = after.ClaimIntakes(ctx, "next-process", t0.Add(ttl), ttl, 10)
	if err != nil || len(got) != 1 || got[0].ID != held[0].ID || got[0].Attempts != 1 {
		t.Fatalf("claim past the dead claim's expiry = (%+v, %v), want it reclaimed with an attempt spent", got, err)
	}
}

func minID(a, b string) string {
	if a < b {
		return a
	}
	return b
}

func maxID(a, b string) string {
	if a > b {
		return a
	}
	return b
}

// A journal that does not decode is an error naming the file, never an empty
// store that would forget every pending launch.
func TestFileStoreRefusesACorruptJournal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "intake.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := NewFileStore(path).ListIntakes(context.Background(), "t1", "", 0)
	if err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("ListIntakes over a corrupt journal = %v, want an error naming it", err)
	}
}

func TestPolicyDelayDoublesUpToItsCeiling(t *testing.T) {
	p := Policy{Backoff: 10 * time.Second, BackoffMax: time.Minute}
	for n, want := range map[int]time.Duration{0: 10 * time.Second, 1: 10 * time.Second, 2: 20 * time.Second, 3: 40 * time.Second, 4: time.Minute, 40: time.Minute} {
		if got := p.Delay(n); got != want {
			t.Errorf("Delay(%d) = %s, want %s", n, got, want)
		}
	}
}

// The doubling never runs past its ceiling, nor past the duration's range:
// a ceiling near the range's end is the delay, never a negative or zero one.
func TestPolicyDelayNeverOverflows(t *testing.T) {
	p := Policy{Backoff: time.Second, BackoffMax: 2562047 * time.Hour}
	for _, n := range []int{30, 34, 35, 40, 64, 100} {
		if d := p.Delay(n); d < p.Backoff || d > p.BackoffMax {
			t.Errorf("Delay(%d) = %s, want within [%s, %s]", n, d, p.Backoff, p.BackoffMax)
		}
	}
}

func TestPolicyFromEnv(t *testing.T) {
	env := func(kv map[string]string) func(string) string { return func(k string) string { return kv[k] } }
	p, err := PolicyFromEnv(EnvPairPrefix, env(nil))
	if err != nil || p != DefaultPolicy {
		t.Fatalf("no dial = (%+v, %v), want the default", p, err)
	}
	p, err = PolicyFromEnv(EnvPairPrefix, env(map[string]string{
		"ITERION_CONNECTOR_PAIR_MAX_ATTEMPTS": "0", "ITERION_CONNECTOR_PAIR_BACKOFF": "1s",
		"ITERION_CONNECTOR_PAIR_BACKOFF_MAX": "5s", "ITERION_CONNECTOR_PAIR_CLAIM_TTL": "90s",
		// Another family's dial is not read.
		"ITERION_CONNECTOR_INTAKE_MAX_ATTEMPTS": "7",
	}))
	want := Policy{MaxAttempts: 0, Backoff: time.Second, BackoffMax: 5 * time.Second, ClaimTTL: 90 * time.Second}
	if err != nil || p != want {
		t.Fatalf("dials = (%+v, %v), want %+v", p, err, want)
	}
	if p.exhausted(1000) {
		t.Fatal("MAX_ATTEMPTS=0 bounds the attempts")
	}
	for name, kv := range map[string]map[string]string{
		"a count that is not one": {"ITERION_CONNECTOR_INTAKE_MAX_ATTEMPTS": "many"},
		"a negative count":        {"ITERION_CONNECTOR_INTAKE_MAX_ATTEMPTS": "-1"},
		"a duration that is not":  {"ITERION_CONNECTOR_INTAKE_BACKOFF": "soon"},
		"a zero claim":            {"ITERION_CONNECTOR_INTAKE_CLAIM_TTL": "0s"},
		// Half of it is the work's window; under a minute, a launch that
		// overran it could still be rolling back when the claim expires.
		"a claim under the worker floor": {"ITERION_CONNECTOR_INTAKE_CLAIM_TTL": "30s"},
		"a ceiling under the floor":      {"ITERION_CONNECTOR_INTAKE_BACKOFF": "1h"},
	} {
		if _, err := PolicyFromEnv(EnvIntakePrefix, env(kv)); err == nil || !strings.Contains(err.Error(), EnvIntakePrefix) {
			t.Errorf("%s: %v, want an error naming the dial", name, err)
		}
	}
}

func TestRetentionFromEnv(t *testing.T) {
	get := func(v string) func(string) string { return func(string) string { return v } }
	for v, want := range map[string]time.Duration{"": DefaultRetention, "0": 0, "72h": 72 * time.Hour} {
		if got, err := RetentionFromEnv(get(v)); err != nil || got != want {
			t.Errorf("%q = (%s, %v), want %s", v, got, err, want)
		}
	}
	if _, err := RetentionFromEnv(get("forever")); err == nil || !strings.Contains(err.Error(), EnvRetention) {
		t.Errorf("an unparsable retention = %v, want an error naming %s", err, EnvRetention)
	}
	if ExpiryFor(t0, 0) != nil {
		t.Error("a zero retention expires rows")
	}
}

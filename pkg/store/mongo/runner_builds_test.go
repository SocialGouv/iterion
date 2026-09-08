package mongo

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ObservedRunnerBuilds is a FILTER, and the one thing that can silently break
// a filter is that it does not match what we think it matches — a `$ne: ""`
// that also excludes documents where the field is absent, a `$gte` on a field
// stored as something other than a date, a projection that drops the value.
// Every one of those failure modes reads as "no runner observed", which the
// push guard would report as "could not check" and let every push through.
//
//	docker run -d -p 27026:27026 mongo:8.0 --replSet rs0 --bind_ip_all --port 27026
//	# then rs.initiate()
//	ITERION_TEST_MONGO_URI='mongodb://localhost:27026/?replicaSet=rs0' \
//	    devbox run -- go test ./pkg/store/mongo/ -run TestObservedRunnerBuilds

func buildsTestStore(t *testing.T) *Store {
	t.Helper()
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping the runner-build observation test")
	}
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()
	s, err := New(ctx, Config{
		URI:      uri,
		Database: "iterion_builds_" + bsonNonce(t),
		Blob:     newInMemoryBlob(),
	})
	if err != nil {
		t.Fatalf("mongo New: %v", err)
	}
	t.Cleanup(func() {
		drop, dcancel := mongotest.TeardownCtx()
		defer dcancel()
		_ = s.db.Drop(drop)
		_ = s.Close(drop)
	})
	return s
}

func TestObservedRunnerBuilds(t *testing.T) {
	s := buildsTestStore(t)
	ctx := store.WithTenant(context.Background(), "team-builds")
	now := time.Now().UTC()

	seed := func(id, version string, age time.Duration) {
		t.Helper()
		if _, err := s.CreateRun(ctx, id, "wf", nil); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
		if version != "" {
			if err := s.SetRunnerVersion(ctx, id, version); err != nil {
				t.Fatalf("stamp %s: %v", id, err)
			}
		}
		// Age the row past/inside the lookback window. Written directly
		// because every store setter refreshes updated_at by design.
		if _, err := s.runs.UpdateOne(ctx, map[string]any{"_id": id},
			map[string]any{"$set": map[string]any{"updated_at": now.Add(-age)}}); err != nil {
			t.Fatalf("age %s: %v", id, err)
		}
	}

	seed("rb-new", "v3.116.4+deadbeef", time.Minute)
	seed("rb-old-build", "v3.112.7+abc123", time.Hour)
	seed("rb-dup", "v3.116.4+deadbeef", 2*time.Hour) // same build twice → one entry
	seed("rb-never-ran", "", 3*time.Hour)            // no runner_version at all
	seed("rb-stale", "v2.0.0+ancient", 30*24*time.Hour)

	got, err := s.ObservedRunnerBuilds(ctx, now.Add(-7*24*time.Hour), 200)
	if err != nil {
		t.Fatalf("ObservedRunnerBuilds: %v", err)
	}
	set := map[string]bool{}
	for _, v := range got {
		if set[v] {
			t.Errorf("build %q returned twice — the caller compares a SET of builds", v)
		}
		set[v] = true
	}
	for _, want := range []string{"v3.116.4+deadbeef", "v3.112.7+abc123"} {
		if !set[want] {
			t.Errorf("build %q missing from %v — a filter that misses a live pod makes the guard blind", want, got)
		}
	}
	if set["v2.0.0+ancient"] {
		t.Errorf("a build last seen 30 days ago is in %v — it is not evidence about today's fleet", got)
	}
	if set[""] {
		t.Errorf("an empty runner_version leaked into %v — a run nobody executed says nothing about the fleet", got)
	}
	if len(got) != 2 {
		t.Fatalf("got %v, want exactly the two recent distinct builds", got)
	}

	// The limit is honoured (and never returns everything by accident).
	if one, err := s.ObservedRunnerBuilds(ctx, now.Add(-7*24*time.Hour), 1); err != nil || len(one) != 1 {
		t.Fatalf("limit 1 = %v (%v), want a single build — the sort is newest-first", one, err)
	}
}

// "What build executes runs here" is a PLATFORM question, and its callers ask
// it from wherever they happen to be: the platform-bot push handler re-scopes
// its context to the `platform:` sentinel tenant, under which no run exists.
// A tenant-scoped query would answer "no runner observed" — and the push guard
// reads that as "could not check" and lets every push through. Silent
// blindness is the failure class this whole change exists to close, so the
// method scopes itself.
func TestObservedRunnerBuildsIgnoresTheCallersTenantScope(t *testing.T) {
	s := buildsTestStore(t)
	now := time.Now().UTC()

	seed := func(tenant, id, version string) {
		t.Helper()
		tctx := store.WithTenant(context.Background(), tenant)
		if _, err := s.CreateRun(tctx, id, "wf", nil); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
		if err := s.SetRunnerVersion(tctx, id, version); err != nil {
			t.Fatalf("stamp %s: %v", id, err)
		}
	}
	seed("team-a", "rb-a", "v3.116.4+aaa")
	seed("team-b", "rb-b", "v3.112.7+bbb")

	for _, name := range []string{"platform:", "team-a", ""} {
		ctx := context.Background()
		if name != "" {
			ctx = store.WithTenant(ctx, name)
		}
		got, err := s.ObservedRunnerBuilds(ctx, now.Add(-time.Hour), 200)
		if err != nil {
			t.Fatalf("under tenant %q: %v", name, err)
		}
		if len(got) != 2 {
			t.Fatalf("under tenant %q got %v, want BOTH teams' builds — the fleet is one fleet whatever scope asks", name, got)
		}
	}
}

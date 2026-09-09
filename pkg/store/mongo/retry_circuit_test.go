package mongo

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/SocialGouv/iterion/pkg/internal/mongotest"
	"github.com/SocialGouv/iterion/pkg/store"
)

// These tests run against a real Mongo replica set because the behaviour
// under test IS Mongo's. RecordRetryFailure is one aggregation-pipeline
// update, and every way it can be wrong is server-side and invisible to a
// compiler: whether a second $set stage sees the first stage's write, what
// $set does with an expression that resolves to missing, whether $max picks
// a date over an absent field, and whether an upsert's base document carries
// the filter's equality fields. A green build proves none of it.
//
//	docker run -d -p 27077:27017 mongo:8.0 --replSet rs0 --bind_ip_all
//	# then rs.initiate()
//	ITERION_TEST_MONGO_URI='mongodb://localhost:27077/?replicaSet=rs0' \
//	    devbox run -- go test ./pkg/store/mongo/ -run TestRetryCircuit

const circuitTenant = "team-circuit"

func circuitCtx() context.Context {
	return store.WithTenant(context.Background(), circuitTenant)
}

// bsonMillis is what a Go instant looks like after a Mongo round trip. BSON
// dates are int64 milliseconds, so any nanoseconds are floored away on the
// way in — an expectation built from an untruncated time.Time is off by up
// to 1ms and fails a comparison the code got right.
func bsonMillis(t time.Time) time.Time { return t.Truncate(time.Millisecond) }

func circuitTestStore(t *testing.T) *Store {
	t.Helper()
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping Mongo retry-circuit tests")
	}
	ctx, cancel := mongotest.Ctx(t)
	defer cancel()
	s, err := New(ctx, Config{
		URI:      uri,
		Database: "iterion_circuit_" + bsonNonce(t),
		Blob:     newInMemoryBlob(),
	})
	if err != nil {
		t.Fatalf("connect mongo: %v", err)
	}
	// EnsureSchema is what creates the unique {tenant_id, key} index the
	// concurrency test depends on, so the fixture builds the same shape a
	// booting server does.
	if err := s.EnsureSchema(ctx, 0); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s
}

// TestRetryCircuit_OpensAtThresholdNotBefore is the core contract: the
// breaker stays shut while the streak is below threshold, and opens for
// exactly one cooldown when it reaches it.
func TestRetryCircuit_OpensAtThresholdNotBefore(t *testing.T) {
	s := circuitTestStore(t)
	ctx := circuitCtx()
	now := time.Now().UTC()
	const cooldown = 15 * time.Minute

	for i := 1; i <= 2; i++ {
		st, err := s.RecordRetryFailure(ctx, "workflow:abc", "run-1", now, 3, cooldown)
		if err != nil {
			t.Fatalf("failure %d: %v", i, err)
		}
		if st.ConsecutiveFailures != i {
			t.Fatalf("failure %d: streak = %d, want %d", i, st.ConsecutiveFailures, i)
		}
		if st.OpenUntil != nil {
			t.Fatalf("failure %d: breaker opened below the threshold (open_until=%s)", i, st.OpenUntil)
		}
		// The read path must agree with the write path's own return value.
		open, err := s.RetryCircuitOpen(ctx, "workflow:abc", now)
		if err != nil {
			t.Fatalf("RetryCircuitOpen: %v", err)
		}
		if open != nil {
			t.Fatalf("failure %d: RetryCircuitOpen reports open below the threshold", i)
		}
	}

	st, err := s.RecordRetryFailure(ctx, "workflow:abc", "run-2", now, 3, cooldown)
	if err != nil {
		t.Fatalf("third failure: %v", err)
	}
	if st.ConsecutiveFailures != 3 {
		t.Fatalf("streak = %d, want 3", st.ConsecutiveFailures)
	}
	if st.OpenUntil == nil {
		t.Fatal("breaker did not open at the threshold")
	}
	if want := bsonMillis(now.Add(cooldown)); !st.OpenUntil.Equal(want) {
		t.Errorf("open_until = %s, want %s (one cooldown from the failure)", st.OpenUntil, want)
	}
	if st.LastFailureRunID != "run-2" {
		t.Errorf("last_failure_run_id = %q, want run-2", st.LastFailureRunID)
	}
}

// TestRetryCircuit_OpenExpiresWithoutASuccess pins the read semantics that
// make `if state != nil { blocked }` safe: the document outlives its own
// cooldown, so an expired breaker must still report closed.
func TestRetryCircuit_OpenExpiresWithoutASuccess(t *testing.T) {
	s := circuitTestStore(t)
	ctx := circuitCtx()
	now := time.Now().UTC()
	const cooldown = 15 * time.Minute

	for range 3 {
		if _, err := s.RecordRetryFailure(ctx, "workflow:exp", "run-1", now, 3, cooldown); err != nil {
			t.Fatalf("RecordRetryFailure: %v", err)
		}
	}
	open, err := s.RetryCircuitOpen(ctx, "workflow:exp", now.Add(time.Minute))
	if err != nil {
		t.Fatalf("RetryCircuitOpen: %v", err)
	}
	if open == nil {
		t.Fatal("breaker reads closed one minute into its cooldown")
	}

	// One second past the cooldown: the document is still there, the breaker
	// is not.
	open, err = s.RetryCircuitOpen(ctx, "workflow:exp", now.Add(cooldown+time.Second))
	if err != nil {
		t.Fatalf("RetryCircuitOpen: %v", err)
	}
	if open != nil {
		t.Errorf("expired breaker still reads open (open_until=%s) — a recovered provider would be blocked forever", open.OpenUntil)
	}
}

// TestRetryCircuit_SuccessClosesTheBreaker covers the reset path.
func TestRetryCircuit_SuccessClosesTheBreaker(t *testing.T) {
	s := circuitTestStore(t)
	ctx := circuitCtx()
	now := time.Now().UTC()
	const cooldown = 15 * time.Minute

	for range 3 {
		if _, err := s.RecordRetryFailure(ctx, "workflow:ok", "run-1", now, 3, cooldown); err != nil {
			t.Fatalf("RecordRetryFailure: %v", err)
		}
	}
	if err := s.RecordRetrySuccess(ctx, "workflow:ok", now); err != nil {
		t.Fatalf("RecordRetrySuccess: %v", err)
	}
	open, err := s.RetryCircuitOpen(ctx, "workflow:ok", now)
	if err != nil {
		t.Fatalf("RetryCircuitOpen: %v", err)
	}
	if open != nil {
		t.Fatalf("breaker still open after a success (open_until=%s)", open.OpenUntil)
	}

	// And the streak restarts from scratch: the next failure is the first of
	// a new storm, not the fourth of the old one.
	st, err := s.RecordRetryFailure(ctx, "workflow:ok", "run-2", now, 3, cooldown)
	if err != nil {
		t.Fatalf("RecordRetryFailure: %v", err)
	}
	if st.ConsecutiveFailures != 1 {
		t.Errorf("streak after a success = %d, want 1", st.ConsecutiveFailures)
	}
	if st.OpenUntil != nil {
		t.Errorf("breaker re-opened on the first failure after a success (open_until=%s)", st.OpenUntil)
	}
}

// TestRetryCircuit_StaleStreakDecays is the regression test for a breaker
// that could never be disarmed. consecutive_failures is only incremented,
// and RecordRetrySuccess needs a SUCCESSFUL engine run of the same workflow
// revision — which a revision that always fails never produces. Without a
// decay window, two failures months apart plus one today would arm a
// tenant-wide cooldown on an isolated failure.
func TestRetryCircuit_StaleStreakDecays(t *testing.T) {
	s := circuitTestStore(t)
	ctx := circuitCtx()
	now := time.Now().UTC()
	const cooldown = 15 * time.Minute

	old := now.Add(-90 * 24 * time.Hour)
	for i := 1; i <= 2; i++ {
		st, err := s.RecordRetryFailure(ctx, "workflow:stale", "run-old", old, 3, cooldown)
		if err != nil {
			t.Fatalf("old failure %d: %v", i, err)
		}
		// Both land inside one cooldown of each other, so the streak builds.
		if want := i; st.ConsecutiveFailures != want {
			t.Fatalf("old failure %d: streak = %d, want %d", i, st.ConsecutiveFailures, want)
		}
	}

	st, err := s.RecordRetryFailure(ctx, "workflow:stale", "run-new", now, 3, cooldown)
	if err != nil {
		t.Fatalf("fresh failure: %v", err)
	}
	if st.ConsecutiveFailures != 1 {
		t.Errorf("streak = %d, want 1 — a 90-day-old storm still counts toward today's threshold", st.ConsecutiveFailures)
	}
	if st.OpenUntil != nil {
		t.Errorf("an isolated failure armed the breaker off a stale streak (open_until=%s)", st.OpenUntil)
	}
}

// TestRetryCircuit_TenantsSharingAWorkflowStayIsolated is what the unique
// {tenant_id, key} index exists for: two teams running the same bot hash
// must not share a breaker, or one tenant's provider outage would delay the
// other's retries.
func TestRetryCircuit_TenantsSharingAWorkflowStayIsolated(t *testing.T) {
	s := circuitTestStore(t)
	now := time.Now().UTC()
	const cooldown = 15 * time.Minute

	a := store.WithTenant(context.Background(), "team-a")
	b := store.WithTenant(context.Background(), "team-b")

	for range 3 {
		if _, err := s.RecordRetryFailure(a, "workflow:shared", "run-a", now, 3, cooldown); err != nil {
			t.Fatalf("team-a failure: %v", err)
		}
	}
	openA, err := s.RetryCircuitOpen(a, "workflow:shared", now)
	if err != nil {
		t.Fatalf("RetryCircuitOpen(a): %v", err)
	}
	if openA == nil {
		t.Fatal("team-a's breaker did not open")
	}

	openB, err := s.RetryCircuitOpen(b, "workflow:shared", now)
	if err != nil {
		t.Fatalf("RetryCircuitOpen(b): %v", err)
	}
	if openB != nil {
		t.Fatal("team-b reads team-a's breaker — the tenant filter leaks")
	}
	stB, err := s.RecordRetryFailure(b, "workflow:shared", "run-b", now, 3, cooldown)
	if err != nil {
		t.Fatalf("team-b failure: %v", err)
	}
	if stB.ConsecutiveFailures != 1 {
		t.Errorf("team-b streak = %d, want 1 — the two tenants share one counter", stB.ConsecutiveFailures)
	}
}

// TestRetryCircuit_ConcurrentFailuresLoseNothing is the atomicity claim the
// header comment makes. Concurrent pods recording failures for one key must
// produce exactly N increments and exactly one document — the three-call
// shape this replaced could drop increments and, on the very first failure,
// race two upserts against the unique index.
func TestRetryCircuit_ConcurrentFailuresLoseNothing(t *testing.T) {
	s := circuitTestStore(t)
	ctx := circuitCtx()
	now := time.Now().UTC()
	const (
		cooldown = 15 * time.Minute
		writers  = 12
	)

	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := range writers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := s.RecordRetryFailure(ctx, "workflow:race", "run-x", now, 3, cooldown); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent RecordRetryFailure: %v", err)
	}

	open, err := s.RetryCircuitOpen(ctx, "workflow:race", now)
	if err != nil {
		t.Fatalf("RetryCircuitOpen: %v", err)
	}
	if open == nil {
		t.Fatal("breaker not open after 12 concurrent failures")
	}
	if open.ConsecutiveFailures != writers {
		t.Errorf("streak = %d, want %d — concurrent increments were lost", open.ConsecutiveFailures, writers)
	}

	n, err := s.retryCircuits.CountDocuments(ctx, withTenantFilter(ctx, bson.M{"key": "workflow:race"}))
	if err != nil {
		t.Fatalf("CountDocuments: %v", err)
	}
	if n != 1 {
		t.Errorf("%d circuit documents for one key, want 1 — the breaker split", n)
	}
}

// TestRetryCircuit_OpenNeverOutlivesItsStreak is the atomicity regression
// test. The three-call shape this replaced — $inc, then a conditional $max
// open_until, then a re-read — decides whether to open from a streak it read
// in an EARLIER round trip. A RecordRetrySuccess landing in that gap zeroes
// the counter and unsets open_until, and the second write then re-opens a
// full cooldown on top of a streak that was just cleared: every later retry
// of that revision is delayed for nothing.
//
// The interleaving cannot be forced, so this asserts the INVARIANT it
// breaks — a live cooldown implies a streak that reached the threshold —
// while writers and a sampler run concurrently. Both operations are single
// atomic document writes now, so no observer can ever see the pair torn.
// (The premise is a constant cooldown, which the fixture controls: only a
// deployment that shrank the cooldown mid-flight could leave a live
// open_until beside a legitimately decayed streak.)
func TestRetryCircuit_OpenNeverOutlivesItsStreak(t *testing.T) {
	s := circuitTestStore(t)
	ctx := circuitCtx()
	// One frozen instant for every writer, so decay never fires and any live
	// open_until is unambiguously this storm's.
	now := time.Now().UTC()
	const (
		cooldown  = 15 * time.Minute
		threshold = 3
		writers   = 6
		rounds    = 25
		key       = "workflow:torn"
	)

	stop := make(chan struct{})
	var writersWG sync.WaitGroup
	for range writers {
		writersWG.Add(1)
		go func() {
			defer writersWG.Done()
			for range rounds {
				for range threshold {
					if _, err := s.RecordRetryFailure(ctx, key, "run-x", now, threshold, cooldown); err != nil {
						return
					}
				}
				if err := s.RecordRetrySuccess(ctx, key, now); err != nil {
					return
				}
			}
		}()
	}

	var (
		mu        sync.Mutex
		violation *store.RetryCircuitState
	)
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		for {
			select {
			case <-stop:
				return
			default:
			}
			var st store.RetryCircuitState
			if err := s.retryCircuits.FindOne(ctx, withTenantFilter(ctx, bson.M{"key": key})).Decode(&st); err != nil {
				continue
			}
			if st.OpenUntil != nil && st.OpenUntil.After(now) && st.ConsecutiveFailures < threshold {
				mu.Lock()
				if violation == nil {
					snapshot := st
					violation = &snapshot
				}
				mu.Unlock()
				return
			}
		}
	}()

	writersWG.Wait()
	close(stop)
	<-sampled

	mu.Lock()
	defer mu.Unlock()
	if violation != nil {
		t.Errorf("observed a live cooldown (%s) on a streak of %d, below the threshold of %d — "+
			"the open decision was taken against a streak another writer had already cleared",
			violation.OpenUntil, violation.ConsecutiveFailures, threshold)
	}
}

// TestRetryCircuit_ConcurrentOpenKeepsTheLongerCooldown pins the $max
// intent: a pod recording a failure must never SHORTEN a cooldown another
// pod already opened further out.
func TestRetryCircuit_ConcurrentOpenKeepsTheLongerCooldown(t *testing.T) {
	s := circuitTestStore(t)
	ctx := circuitCtx()
	now := time.Now().UTC()

	// Open far out first, with a long cooldown.
	for range 3 {
		if _, err := s.RecordRetryFailure(ctx, "workflow:max", "run-long", now, 3, 6*time.Hour); err != nil {
			t.Fatalf("long-cooldown failure: %v", err)
		}
	}
	far := bsonMillis(now.Add(6 * time.Hour))

	// A second pod, configured with a shorter cooldown, records another
	// failure a moment later.
	st, err := s.RecordRetryFailure(ctx, "workflow:max", "run-short", now.Add(time.Minute), 3, time.Minute)
	if err != nil {
		t.Fatalf("short-cooldown failure: %v", err)
	}
	if st.OpenUntil == nil {
		t.Fatal("breaker closed after a fourth failure")
	}
	if st.OpenUntil.Before(far) {
		t.Errorf("open_until = %s, shortened below the cooldown already opened (%s)", st.OpenUntil, far)
	}
}

// TestRetryCircuit_EmptyKeyIsRefused keeps a keyless run — one with neither
// a workflow hash nor a name — from collapsing every tenant's failures into
// one shared document.
func TestRetryCircuit_EmptyKeyIsRefused(t *testing.T) {
	s := circuitTestStore(t)
	ctx := circuitCtx()
	if _, err := s.RecordRetryFailure(ctx, "", "run-1", time.Now().UTC(), 3, time.Minute); err == nil {
		t.Error("RecordRetryFailure accepted an empty key")
	}
	open, err := s.RetryCircuitOpen(ctx, "", time.Now().UTC())
	if err != nil || open != nil {
		t.Errorf("RetryCircuitOpen(\"\") = %v, %v; want nil, nil", open, err)
	}
	if err := s.RecordRetrySuccess(ctx, "", time.Now().UTC()); err != nil {
		t.Errorf("RecordRetrySuccess(\"\") = %v; want nil", err)
	}
}

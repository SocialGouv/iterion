package mongo

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/SocialGouv/iterion/pkg/store"
)

// These tests run against a real Mongo replica set because the behaviour
// under test IS Mongo's: the arm/claim pair is a compare-and-swap expressed
// as a conditional update, and the one thing that can silently break it is a
// filter that does not match what we think it matches. Notably, comparison
// operators are type-bracketed — {$lt: n} does not match a document whose
// field is absent — which would refuse to arm the FIRST retry of every run
// and disable the feature with no error anywhere.
//
//	docker run -d -p 27077:27017 mongo:8.0 --replSet rs0 --bind_ip_all
//	# then rs.initiate()
//	ITERION_TEST_MONGO_URI='mongodb://localhost:27077/?replicaSet=rs0' \
//	    devbox run -- go test ./pkg/store/mongo/ -run TestRunRetry

// retryTenant scopes the fixtures. Runs are tenant-scoped in cloud mode and
// the store panics on a tenant-less query, so the tests exercise the same
// scoping production uses — only the sweeper's platform-wide scan opts out.
const retryTenant = "team-retry"

func retryCtx() context.Context {
	return store.WithTenant(context.Background(), retryTenant)
}

func retryTestStore(t *testing.T) *Store {
	t.Helper()
	uri := os.Getenv("ITERION_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("ITERION_TEST_MONGO_URI not set; skipping Mongo retry tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	s, err := New(ctx, Config{
		URI:      uri,
		Database: "iterion_retry_" + bsonNonce(t),
		Blob:     newInMemoryBlob(),
	})
	if err != nil {
		t.Fatalf("connect mongo: %v", err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s
}

// seedFailedResumable creates a run and drives it to failed_resumable, the
// only status from which a retry may be armed.
func seedFailedResumable(t *testing.T, s *Store, runID string) {
	t.Helper()
	ctx := retryCtx()
	r, err := s.CreateRun(ctx, runID, "feed_watch", nil)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	// FilePath is what the sweeper resolves the workflow source from, so the
	// fixture has to carry one.
	r.FilePath = "bots/some-bot/main.bot"
	if err := s.SaveRun(ctx, r); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
	if err := s.FailRunResumable(ctx, runID, &store.Checkpoint{NodeID: "synthesize"}, "usage window exhausted"); err != nil {
		t.Fatalf("FailRunResumable: %v", err)
	}
}

// TestRunRetry_ArmsFirstAttemptWithNoPriorState is the regression test for
// the type-bracketing trap: a run with no retry_state at all must still arm.
func TestRunRetry_ArmsFirstAttemptWithNoPriorState(t *testing.T) {
	s := retryTestStore(t)
	ctx := retryCtx()
	seedFailedResumable(t, s, "run-first-arm")

	at := time.Now().UTC().Add(30 * time.Hour).Truncate(time.Millisecond)
	scheduled, attempt, err := s.ScheduleRunRetry(ctx, "run-first-arm", at, "usage_window", "USAGE_LIMIT_BLOCKED", 5)
	if err != nil {
		t.Fatalf("ScheduleRunRetry: %v", err)
	}
	if !scheduled {
		t.Fatal("scheduled = false for a run with no prior retry_state — the attempts filter does not match a missing field")
	}
	if attempt != 1 {
		t.Errorf("attempt = %d, want 1", attempt)
	}

	r, err := s.LoadRun(ctx, "run-first-arm")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r.RetryState == nil || r.RetryState.RetryAfter == nil {
		t.Fatal("retry state not persisted")
	}
	if !r.RetryState.RetryAfter.Equal(at) {
		t.Errorf("retry_after = %v, want %v", r.RetryState.RetryAfter, at)
	}
	if r.RetryState.Code != "USAGE_LIMIT_BLOCKED" || r.RetryState.Reason != "usage_window" {
		t.Errorf("reason/code = %q/%q, want usage_window/USAGE_LIMIT_BLOCKED", r.RetryState.Reason, r.RetryState.Code)
	}
	// Arming must not resurrect the run: only the sweeper's resume does that.
	if r.Status != store.RunStatusFailedResumable {
		t.Errorf("status = %v, want failed_resumable", r.Status)
	}
}

// TestRunRetry_AttemptsAreMonotonicAndBounded pins the anti-loop bound.
func TestRunRetry_AttemptsAreMonotonicAndBounded(t *testing.T) {
	s := retryTestStore(t)
	ctx := retryCtx()
	seedFailedResumable(t, s, "run-bounded")

	at := time.Now().UTC().Add(time.Hour)
	for i := 1; i <= 2; i++ {
		scheduled, attempt, err := s.ScheduleRunRetry(ctx, "run-bounded", at, "usage_window", "USAGE_LIMIT_BLOCKED", 2)
		if err != nil {
			t.Fatalf("arm %d: %v", i, err)
		}
		if !scheduled || attempt != i {
			t.Fatalf("arm %d: scheduled=%v attempt=%d, want true/%d", i, scheduled, attempt, i)
		}
	}
	// Budget spent: the third arming must refuse, and must not be an error.
	scheduled, _, err := s.ScheduleRunRetry(ctx, "run-bounded", at, "usage_window", "USAGE_LIMIT_BLOCKED", 2)
	if err != nil {
		t.Fatalf("arm past budget returned an error: %v", err)
	}
	if scheduled {
		t.Error("scheduled = true past the attempt budget, want false")
	}
}

// TestRunRetry_RefusesWhenNotResumable covers the operator race: someone
// resumed the run by hand, so the run is no longer failed_resumable and the
// retry must not be re-armed behind them.
func TestRunRetry_RefusesWhenNotResumable(t *testing.T) {
	s := retryTestStore(t)
	ctx := retryCtx()
	seedFailedResumable(t, s, "run-resumed-by-hand")
	if err := s.UpdateRunStatus(ctx, "run-resumed-by-hand", store.RunStatusRunning, ""); err != nil {
		t.Fatalf("UpdateRunStatus: %v", err)
	}

	scheduled, _, err := s.ScheduleRunRetry(ctx, "run-resumed-by-hand", time.Now().UTC().Add(time.Hour), "usage_window", "USAGE_LIMIT_BLOCKED", 5)
	if err != nil {
		t.Fatalf("ScheduleRunRetry: %v", err)
	}
	if scheduled {
		t.Error("scheduled = true for a running run, want false")
	}
}

// TestRunRetry_ClaimIsExclusive is the multi-replica contract: many
// sweepers, exactly one resume.
func TestRunRetry_ClaimIsExclusive(t *testing.T) {
	s := retryTestStore(t)
	ctx := retryCtx()
	seedFailedResumable(t, s, "run-claim")

	at := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond) // already due
	if scheduled, _, err := s.ScheduleRunRetry(ctx, "run-claim", at, "usage_window", "USAGE_LIMIT_BLOCKED", 5); err != nil || !scheduled {
		t.Fatalf("arm: scheduled=%v err=%v", scheduled, err)
	}

	const replicas = 8
	var wg sync.WaitGroup
	wins := make([]bool, replicas)
	errs := make([]error, replicas)
	wg.Add(replicas)
	for i := 0; i < replicas; i++ {
		go func(i int) {
			defer wg.Done()
			wins[i], errs[i] = s.ClaimRunRetry(ctx, "run-claim", at)
		}(i)
	}
	wg.Wait()

	won := 0
	for i := range wins {
		if errs[i] != nil {
			t.Fatalf("replica %d: %v", i, errs[i])
		}
		if wins[i] {
			won++
		}
	}
	if won != 1 {
		t.Fatalf("%d replicas won the claim, want exactly 1", won)
	}

	// The claim is a LEASE: the row stays armed (so a claimer that dies
	// cannot strand it) but a second claim loses while the lease is live.
	due, err := s.ListRunsDueForRetry(store.WithoutTenantFilter(ctx), time.Now().UTC(), 10)
	if err != nil {
		t.Fatalf("ListRunsDueForRetry: %v", err)
	}
	var stillListed bool
	for _, d := range due {
		if d.ID == "run-claim" {
			stillListed = true
		}
	}
	if !stillListed {
		t.Error("the claimed run left the due set — a claimer that dies would strand it forever")
	}
	if won, err := s.ClaimRunRetry(ctx, "run-claim", at); err != nil || won {
		t.Errorf("re-claim under a live lease: won=%v err=%v, want false/nil", won, err)
	}
}

// TestRunRetry_ClearDisarmsAfterAResume pins the lease's counterpart: once
// the resume is enqueued the retry must leave the due set, or a past
// retry_after would re-fire the next time this run failed for any reason.
func TestRunRetry_ClearDisarmsAfterAResume(t *testing.T) {
	s := retryTestStore(t)
	ctx := retryCtx()
	seedFailedResumable(t, s, "run-clear")

	at := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	if _, _, err := s.ScheduleRunRetry(ctx, "run-clear", at, "usage_window", "USAGE_LIMIT_BLOCKED", 5); err != nil {
		t.Fatalf("arm: %v", err)
	}
	if won, err := s.ClaimRunRetry(ctx, "run-clear", at); err != nil || !won {
		t.Fatalf("claim: won=%v err=%v", won, err)
	}
	if err := s.ClearRunRetry(ctx, "run-clear"); err != nil {
		t.Fatalf("ClearRunRetry: %v", err)
	}

	due, err := s.ListRunsDueForRetry(store.WithoutTenantFilter(ctx), time.Now().UTC(), 10)
	if err != nil {
		t.Fatalf("ListRunsDueForRetry: %v", err)
	}
	for _, d := range due {
		if d.ID == "run-clear" {
			t.Error("a cleared retry is still listed as due")
		}
	}
	r, _ := s.LoadRun(ctx, "run-clear")
	if r.RetryState != nil && r.RetryState.RetryAfter != nil {
		t.Error("retry_after survived the clear")
	}
	// Attempts stay spent — clearing is not a refund.
	if r.RetryState == nil || r.RetryState.Attempts != 1 {
		t.Errorf("attempts = %v, want 1", r.RetryState)
	}
}

// TestRunRetry_StrandedClaimRecoversAfterTheLease is the durability case the
// lease exists for: a sweeper that claims and then dies must not take the
// run with it.
func TestRunRetry_StrandedClaimRecoversAfterTheLease(t *testing.T) {
	s := retryTestStore(t)
	ctx := retryCtx()
	seedFailedResumable(t, s, "run-stranded")

	at := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	if _, _, err := s.ScheduleRunRetry(ctx, "run-stranded", at, "usage_window", "USAGE_LIMIT_BLOCKED", 5); err != nil {
		t.Fatalf("arm: %v", err)
	}
	if won, err := s.ClaimRunRetry(ctx, "run-stranded", at); err != nil || !won {
		t.Fatalf("first claim: won=%v err=%v", won, err)
	}
	// Simulate the pod dying right after the claim: nothing cleared, and the
	// lease is now stale.
	stale := time.Now().UTC().Add(-2 * retryClaimLease)
	if _, err := s.runs.UpdateOne(ctx, bson.M{"_id": "run-stranded"},
		bson.M{"$set": bson.M{retryPath("claimed_at"): stale}}); err != nil {
		t.Fatalf("age the lease: %v", err)
	}

	won, err := s.ClaimRunRetry(ctx, "run-stranded", at)
	if err != nil {
		t.Fatalf("re-claim: %v", err)
	}
	if !won {
		t.Fatal("a stranded claim never becomes re-claimable — the run is lost for good")
	}
}

// TestRunRetry_ClaimRejectsStaleExpectation covers the re-arm race: the
// retry instant moved between read and claim, so the claim must lose rather
// than resume against a stale plan.
func TestRunRetry_ClaimRejectsStaleExpectation(t *testing.T) {
	s := retryTestStore(t)
	ctx := retryCtx()
	seedFailedResumable(t, s, "run-stale-claim")

	stale := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	fresh := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	if _, _, err := s.ScheduleRunRetry(ctx, "run-stale-claim", stale, "usage_window", "USAGE_LIMIT_BLOCKED", 5); err != nil {
		t.Fatalf("arm stale: %v", err)
	}
	if _, _, err := s.ScheduleRunRetry(ctx, "run-stale-claim", fresh, "usage_window", "USAGE_LIMIT_BLOCKED", 5); err != nil {
		t.Fatalf("re-arm fresh: %v", err)
	}

	won, err := s.ClaimRunRetry(ctx, "run-stale-claim", stale)
	if err != nil {
		t.Fatalf("ClaimRunRetry: %v", err)
	}
	if won {
		t.Error("won = true against a stale retry_after, want false")
	}
	if won, err := s.ClaimRunRetry(ctx, "run-stale-claim", fresh); err != nil || !won {
		t.Errorf("claim with the current value: won=%v err=%v, want true/nil", won, err)
	}
}

// TestRunRetry_ListDueOrdersAndFilters pins what the sweeper scans: only
// armed runs whose instant has arrived, oldest first.
func TestRunRetry_ListDueOrdersAndFilters(t *testing.T) {
	s := retryTestStore(t)
	ctx := retryCtx()
	now := time.Now().UTC()

	seedFailedResumable(t, s, "run-due-late")
	seedFailedResumable(t, s, "run-due-early")
	seedFailedResumable(t, s, "run-not-yet")
	seedFailedResumable(t, s, "run-never-armed")

	arm := func(id string, at time.Time) {
		t.Helper()
		if scheduled, _, err := s.ScheduleRunRetry(ctx, id, at, "usage_window", "USAGE_LIMIT_BLOCKED", 5); err != nil || !scheduled {
			t.Fatalf("arm %s: scheduled=%v err=%v", id, scheduled, err)
		}
	}
	arm("run-due-late", now.Add(-time.Minute))
	arm("run-due-early", now.Add(-time.Hour))
	arm("run-not-yet", now.Add(time.Hour))

	due, err := s.ListRunsDueForRetry(store.WithoutTenantFilter(ctx), now, 50)
	if err != nil {
		t.Fatalf("ListRunsDueForRetry: %v", err)
	}
	// Scoped to this test's own runs: the scan is platform-wide by design,
	// so asserting on the whole result would couple this test to whatever
	// else shares the database.
	mine := map[string]bool{"run-due-late": true, "run-due-early": true, "run-not-yet": true, "run-never-armed": true}
	var ids []string
	for _, d := range due {
		if mine[d.ID] {
			ids = append(ids, d.ID)
		}
	}
	if len(ids) != 2 || ids[0] != "run-due-early" || ids[1] != "run-due-late" {
		t.Fatalf("due = %v, want [run-due-early run-due-late] (oldest first, future and unarmed excluded)", ids)
	}
	// The sweeper needs the armed instant to CAS on, without a second read.
	if got := due[0].RetryAfter(); got.IsZero() {
		t.Error("RetryAfter() is zero — the sweeper cannot claim without it")
	}
	if due[0].FilePath == "" {
		t.Error("FilePath not projected — the sweeper cannot resolve the workflow source")
	}
}

// TestRunRetry_AbandonDisarmsAndExplains pins that a run which stops
// retrying says why instead of going quiet.
func TestRunRetry_AbandonDisarmsAndExplains(t *testing.T) {
	s := retryTestStore(t)
	ctx := retryCtx()
	seedFailedResumable(t, s, "run-abandon")
	if _, _, err := s.ScheduleRunRetry(ctx, "run-abandon", time.Now().UTC().Add(-time.Minute), "usage_window", "USAGE_LIMIT_BLOCKED", 5); err != nil {
		t.Fatalf("arm: %v", err)
	}

	if err := s.AbandonRunRetry(ctx, "run-abandon", "auto-retry abandoned: monthly run quota reached"); err != nil {
		t.Fatalf("AbandonRunRetry: %v", err)
	}
	r, err := s.LoadRun(ctx, "run-abandon")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r.RetryState == nil || r.RetryState.RetryAfter != nil {
		t.Error("retry still armed after abandon")
	}
	if r.RetryState.LastError == "" {
		t.Error("LastError empty — an abandoned retry must record why")
	}
	// Attempts stay spent: abandoning is not a refund.
	if r.RetryState.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", r.RetryState.Attempts)
	}
}

// TestRunRetry_StoreCapabilityIsDetectable pins the nil-check contract every
// caller relies on.
func TestRunRetry_StoreCapabilityIsDetectable(t *testing.T) {
	s := retryTestStore(t)
	if store.AsRunRetryStore(s) == nil {
		t.Error("the Mongo store must satisfy store.RunRetryStore")
	}
}

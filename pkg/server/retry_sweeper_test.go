package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
	mongostore "github.com/SocialGouv/iterion/pkg/store/mongo"
)

// The sweeper is the half of the retry that can lose a run: it claims,
// gates, resolves and re-enqueues, and every one of those can fail. What
// these tests pin is that each failure lands somewhere legible — resumed,
// re-armed with a reason, or abandoned with a reason — and never silently.

type fakeRetryLister struct{ refs []mongostore.RetryDueRef }

func (f *fakeRetryLister) ListRunsDueForRetry(_ context.Context, _ time.Time, limit int) ([]mongostore.RetryDueRef, error) {
	if limit > 0 && len(f.refs) > limit {
		return f.refs[:limit], nil
	}
	return f.refs, nil
}

// fakeRetryStore records the sweeper's decisions. Embedding store.RunStore
// leaves every unused method nil — a call to one panics loudly rather than
// passing silently, which is what we want from a fake.
type fakeRetryStore struct {
	store.RunStore
	mu sync.Mutex

	claimWins map[string]bool // run id -> does the claim win
	loadRun   map[string]*store.Run

	claimed   []string
	cleared   []string
	abandoned map[string]string
	rearmed   map[string]time.Time
	armBudget map[string]int // run id -> max attempts seen by ScheduleRunRetry
	armOK     bool
}

func newFakeRetryStore() *fakeRetryStore {
	return &fakeRetryStore{
		claimWins: map[string]bool{},
		loadRun:   map[string]*store.Run{},
		abandoned: map[string]string{},
		rearmed:   map[string]time.Time{},
		armBudget: map[string]int{},
		armOK:     true,
	}
}

func (f *fakeRetryStore) ClaimRunRetry(ctx context.Context, runID string, _ time.Time) (bool, error) {
	// The sweeper must stamp the run's tenant before any write — the mongo
	// store panics otherwise, so assert it here rather than discover it in
	// production.
	if tenant, ok := store.TenantFromContext(ctx); !ok || tenant == "" {
		panic("retry sweeper: claim without tenant ctx")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.claimWins[runID] {
		return false, nil
	}
	f.claimed = append(f.claimed, runID)
	return true, nil
}

func (f *fakeRetryStore) ScheduleRunRetry(_ context.Context, runID string, at time.Time, _, _ string, maxAttempts int) (bool, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.armBudget[runID] = maxAttempts
	if !f.armOK {
		return false, 0, nil
	}
	f.rearmed[runID] = at
	return true, 2, nil
}

func (f *fakeRetryStore) ClearRunRetry(_ context.Context, runID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleared = append(f.cleared, runID)
	return nil
}

func (f *fakeRetryStore) AbandonRunRetry(_ context.Context, runID, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.abandoned[runID] = reason
	return nil
}

func (f *fakeRetryStore) LoadRun(_ context.Context, id string) (*store.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.loadRun[id]; ok {
		return r, nil
	}
	return &store.Run{ID: id}, nil
}

// fakeResumer captures what the sweeper asked runview to resume.
type fakeResumer struct {
	mu      sync.Mutex
	calls   []runview.ResumeSpec
	failing error
}

func (f *fakeResumer) Resume(_ context.Context, spec runview.ResumeSpec) (*runview.LaunchResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, spec)
	if f.failing != nil {
		return nil, f.failing
	}
	return &runview.LaunchResult{RunID: spec.RunID}, nil
}

func dueRef(id string, at time.Time) mongostore.RetryDueRef {
	return mongostore.RetryDueRef{
		ID:       id,
		TenantID: "team-1",
		OwnerID:  "user-1",
		FilePath: "bots/feed-watch/main.bot",
		RetryState: &store.RunRetryState{
			RetryAfter: &at,
			Reason:     "usage_window",
			Code:       "USAGE_LIMIT_BLOCKED",
			Attempts:   1,
		},
	}
}

func TestSweepDueRetries_ResumesAClaimedRun(t *testing.T) {
	st := newFakeRetryStore()
	st.claimWins["run-a"] = true
	resumer := &fakeResumer{}
	s := newRetrySweeperServer(t, st, resumer)

	at := time.Now().UTC().Add(-time.Minute)
	s.sweepDueRetries(context.Background(), &fakeRetryLister{refs: []mongostore.RetryDueRef{dueRef("run-a", at)}}, resumer, time.Now().UTC())

	if len(resumer.calls) != 1 {
		t.Fatalf("Resume called %d times, want 1", len(resumer.calls))
	}
	if got := resumer.calls[0].RunID; got != "run-a" {
		t.Errorf("resumed %q, want run-a", got)
	}
	// A multi-day wait can outlive a bot redeploy; forcing past the hash
	// check would resume a checkpoint against a workflow that moved.
	if resumer.calls[0].Force {
		t.Error("Force = true — an automatic resume must never skip the workflow-hash check")
	}
	if len(st.abandoned) != 0 {
		t.Errorf("unexpected abandon: %v", st.abandoned)
	}
	// The claim is a lease, so the successful resume is what disarms it —
	// otherwise a past retry_after survives and re-fires the next time this
	// run fails for any unrelated reason.
	if len(st.cleared) != 1 || st.cleared[0] != "run-a" {
		t.Errorf("cleared = %v, want [run-a] after a successful resume", st.cleared)
	}
}

// TestSweepDueRetries_TransientDenialReArms pins the denial classification: a
// monthly quota refills on the 1st and a concurrency cap clears in minutes,
// so those must defer the retry rather than throw the run away.
func TestSweepDueRetries_TransientDenialReArms(t *testing.T) {
	for _, reason := range []string{denyMonthlyRunQuota, denyMonthlyCostCap, denyConcurrencyCap, denyLaunchRateLimited} {
		if !retryDenialIsTransient(reason) {
			t.Errorf("%s classified permanent — the run would be dropped for a condition that clears itself", reason)
		}
	}
	for _, reason := range []string{denyOrgSuspended, denyNoWorkspace, "some_future_code"} {
		if retryDenialIsTransient(reason) {
			t.Errorf("%s classified transient — it needs a human, and re-arming would loop forever", reason)
		}
	}
}

func TestSweepDueRetries_LostClaimDoesNotResume(t *testing.T) {
	st := newFakeRetryStore() // claimWins empty → every claim loses
	resumer := &fakeResumer{}
	s := newRetrySweeperServer(t, st, resumer)

	at := time.Now().UTC().Add(-time.Minute)
	s.sweepDueRetries(context.Background(), &fakeRetryLister{refs: []mongostore.RetryDueRef{dueRef("run-a", at)}}, resumer, time.Now().UTC())

	if len(resumer.calls) != 0 {
		t.Fatalf("Resume called %d times after losing the claim, want 0 — two replicas would double-resume", len(resumer.calls))
	}
}

func TestSweepDueRetries_FailedResumeReArmsWithinBudget(t *testing.T) {
	st := newFakeRetryStore()
	st.claimWins["run-a"] = true
	st.loadRun["run-a"] = &store.Run{ID: "run-a", RetryPolicy: &store.RunRetryPolicy{MaxAttempts: 4}}
	resumer := &fakeResumer{failing: errors.New("publish blip")}
	s := newRetrySweeperServer(t, st, resumer)

	at := time.Now().UTC().Add(-time.Minute)
	s.sweepDueRetries(context.Background(), &fakeRetryLister{refs: []mongostore.RetryDueRef{dueRef("run-a", at)}}, resumer, time.Now().UTC())

	if _, ok := st.rearmed["run-a"]; !ok {
		t.Fatal("a failed resume must re-arm, not drop the run")
	}
	// The re-arm has to respect the run's own budget, not a fresh default,
	// or a failing resume could loop past the operator's ceiling.
	if got := st.armBudget["run-a"]; got != 4 {
		t.Errorf("re-arm budget = %d, want the run's 4", got)
	}
	if len(st.abandoned) != 0 {
		t.Errorf("re-armable failure must not abandon: %v", st.abandoned)
	}
}

func TestSweepDueRetries_ExhaustedBudgetAbandonsWithAReason(t *testing.T) {
	st := newFakeRetryStore()
	st.claimWins["run-a"] = true
	st.armOK = false // budget spent: ScheduleRunRetry refuses
	resumer := &fakeResumer{failing: errors.New("publish blip")}
	s := newRetrySweeperServer(t, st, resumer)

	at := time.Now().UTC().Add(-time.Minute)
	s.sweepDueRetries(context.Background(), &fakeRetryLister{refs: []mongostore.RetryDueRef{dueRef("run-a", at)}}, resumer, time.Now().UTC())

	// The resume must actually have been attempted: otherwise this test
	// would pass on any earlier abandon (an unresolvable source, say) and
	// prove nothing about the attempt budget.
	if len(resumer.calls) != 1 {
		t.Fatalf("Resume called %d times, want 1 before the budget check", len(resumer.calls))
	}
	reason, ok := st.abandoned["run-a"]
	if !ok {
		t.Fatal("a run out of attempts must be abandoned, not left armed")
	}
	if reason == "" {
		t.Error("abandon reason empty — a run that stops retrying must say why")
	}
}

func TestSweepDueRetries_UnresolvableSourceAbandons(t *testing.T) {
	st := newFakeRetryStore()
	st.claimWins["run-a"] = true
	resumer := &fakeResumer{}
	s := newRetrySweeperServer(t, st, resumer)
	s.cfg.Mode = "cloud" // cloud + no catalog match → source unresolvable

	ref := dueRef("run-a", time.Now().UTC().Add(-time.Minute))
	ref.FilePath = "bots/deleted-bot/main.bot"
	s.sweepDueRetries(context.Background(), &fakeRetryLister{refs: []mongostore.RetryDueRef{ref}}, resumer, time.Now().UTC())

	if len(resumer.calls) != 0 {
		t.Errorf("resumed despite an unresolvable source: %+v", resumer.calls)
	}
	if _, ok := st.abandoned["run-a"]; !ok {
		t.Fatal("an unresolvable source is permanent — abandon, do not re-arm every 15 minutes forever")
	}
	if _, ok := st.rearmed["run-a"]; ok {
		t.Error("a permanently unresolvable run must not be re-armed")
	}
}

func TestSweepDueRetries_RespectsTheBatchCap(t *testing.T) {
	st := newFakeRetryStore()
	var refs []mongostore.RetryDueRef
	at := time.Now().UTC().Add(-time.Minute)
	for _, id := range []string{"r1", "r2", "r3", "r4", "r5", "r6", "r7"} {
		st.claimWins[id] = true
		refs = append(refs, dueRef(id, at))
	}
	resumer := &fakeResumer{}
	s := newRetrySweeperServer(t, st, resumer)

	s.sweepDueRetries(context.Background(), &fakeRetryLister{refs: refs}, resumer, time.Now().UTC())

	// Several schedules routinely share one reset; resuming all of them at
	// once is how the freshly-reopened window is exhausted immediately.
	if len(resumer.calls) > retrySweepBatch {
		t.Fatalf("resumed %d runs in one pass, want at most %d", len(resumer.calls), retrySweepBatch)
	}
}

func TestSweepDueRetries_NoRetryStoreIsANoOp(t *testing.T) {
	// Local/filesystem mode has no durable retry surface; the sweeper must
	// simply not run rather than panic on a missing capability.
	s := &Server{cfg: Config{Store: plainRunStore{}}}
	s.sweepDueRetries(context.Background(), &fakeRetryLister{refs: []mongostore.RetryDueRef{
		dueRef("run-a", time.Now().UTC().Add(-time.Minute)),
	}}, &fakeResumer{}, time.Now().UTC())
}

// plainRunStore satisfies store.RunStore without RunRetryStore.
type plainRunStore struct{ store.RunStore }

// newRetrySweeperServer builds a local-mode server whose WorkDir holds the
// fixture bot, so resolveResumeSource succeeds and the tests exercise the
// branch they name rather than all funnelling into "source unresolvable".
func newRetrySweeperServer(t *testing.T, st store.RunStore, resumer runResumer) *Server {
	t.Helper()
	_ = resumer // passed per-call, not stored on the Server
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "bots", "feed-watch"), 0o755); err != nil {
		t.Fatalf("mkdir fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bots", "feed-watch", "main.bot"), []byte("workflow w:\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return &Server{cfg: Config{Store: st, WorkDir: dir}}
}

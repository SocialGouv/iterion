package cloudpublisher

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

type resumeLoadBarrierStore struct {
	store.RunStore
	mu      sync.Mutex
	loads   int
	ready   chan struct{}
	release chan struct{}
}

func (s *resumeLoadBarrierStore) LoadRun(ctx context.Context, id string) (*store.Run, error) {
	r, err := s.RunStore.LoadRun(ctx, id)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.loads++
	n := s.loads
	if n == 2 {
		close(s.ready)
	}
	s.mu.Unlock()
	if n <= 2 {
		<-s.release
	}
	return r, nil
}

func TestSubmitResume_PublishFailureRestoresOperatorPause(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}

	ctx := store.WithIdentity(context.Background(), "team", "alice")
	const runID = "run-operator-paused"
	if err := st.SaveRun(ctx, &store.Run{
		ID:       runID,
		TenantID: "team",
		OwnerID:  "alice",
		Status:   store.RunStatusPausedOperator,
	}); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	p := &Publisher{
		store: st,
		publishRun: func(context.Context, *queue.RunMessage) error {
			return errors.New("nats unavailable")
		},
	}
	wf := &ir.Workflow{Name: "operator_resume"}
	spec := runview.ResumeSpec{
		RunID:    runID,
		FilePath: "operator_resume.bot",
		Source:   "workflow operator_resume:\n  entry: done\n",
	}

	err = p.SubmitResume(ctx, spec, wf, "hash")
	if err == nil {
		t.Fatal("SubmitResume returned nil, want publish failure")
	}
	if !strings.Contains(err.Error(), "nats unavailable") {
		t.Fatalf("SubmitResume error = %v, want publish failure", err)
	}

	r, err := st.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r.Status != store.RunStatusPausedOperator {
		t.Fatalf("status after publish failure = %s, want paused_operator", r.Status)
	}
}

// retryStoreSpy wraps a RunStore and adds RunRetryStore so we can observe
// whether SubmitResume disarms the retry state on a successful publish.
type retryStoreSpy struct {
	store.RunStore
	cleared []string
	claimed []string
	armed   []string
	// abandoned is left unused but part of the interface contract.
	abandoned []string
}

func (r *retryStoreSpy) ScheduleRunRetry(_ context.Context, runID string, _ time.Time, _, _ string, _ int) (bool, int, error) {
	r.armed = append(r.armed, runID)
	return true, 1, nil
}
func (r *retryStoreSpy) ClaimRunRetry(_ context.Context, runID string, _ time.Time) (bool, error) {
	r.claimed = append(r.claimed, runID)
	return true, nil
}
func (r *retryStoreSpy) ClearRunRetry(_ context.Context, runID string) error {
	r.cleared = append(r.cleared, runID)
	return nil
}
func (r *retryStoreSpy) AbandonRunRetry(_ context.Context, runID, _ string) error {
	r.abandoned = append(r.abandoned, runID)
	return nil
}

// #669 part 3: a manual resume never called ClearRunRetry, so a
// usage-window retry_after armed BEFORE the operator resumed survived it
// and re-fired days later on a run that had finished. SubmitResume is the
// single choke point for a cloud resume — it must clear on success.
func TestSubmitResume_ClearsArmedRetryOnSuccessfulPublish(t *testing.T) {
	base, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	spy := &retryStoreSpy{RunStore: base}
	ctx := store.WithIdentity(context.Background(), "team", "alice")
	const runID = "run-manual-resume-with-armed-retry"
	at := time.Now().UTC().Add(24 * time.Hour)
	if err := base.SaveRun(ctx, &store.Run{
		ID: runID, TenantID: "team", OwnerID: "alice",
		Status:     store.RunStatusFailedResumable,
		RetryState: &store.RunRetryState{RetryAfter: &at, Reason: "usage_window", Attempts: 2},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	p := &Publisher{
		store: spy,
		publishRun: func(context.Context, *queue.RunMessage) error {
			return nil
		},
	}
	wf := &ir.Workflow{Name: "wf"}
	spec := runview.ResumeSpec{
		RunID: runID, FilePath: "wf.bot",
		Source: "workflow wf:\n  entry: done\n",
	}
	if err := p.SubmitResume(ctx, spec, wf, "hash"); err != nil {
		t.Fatalf("SubmitResume: %v", err)
	}
	if len(spy.cleared) != 1 || spy.cleared[0] != runID {
		t.Fatalf("ClearRunRetry calls = %v, want exactly [%q] — an unarmed retry survives the manual resume otherwise (#669 part 3)", spy.cleared, runID)
	}
}

// A publish failure must NOT clear the retry state: rollback restores the
// resumable status, and dropping the armed retry would strand a run whose
// only remaining wake-up was the sweeper.
func TestSubmitResume_KeepsRetryStateOnPublishFailure(t *testing.T) {
	base, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	spy := &retryStoreSpy{RunStore: base}
	ctx := store.WithIdentity(context.Background(), "team", "alice")
	const runID = "run-manual-resume-publish-fails"
	at := time.Now().UTC().Add(24 * time.Hour)
	if err := base.SaveRun(ctx, &store.Run{
		ID: runID, TenantID: "team", OwnerID: "alice",
		Status:     store.RunStatusFailedResumable,
		RetryState: &store.RunRetryState{RetryAfter: &at, Reason: "usage_window"},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	p := &Publisher{
		store: spy,
		publishRun: func(context.Context, *queue.RunMessage) error {
			return errors.New("nats unavailable")
		},
	}
	wf := &ir.Workflow{Name: "wf"}
	spec := runview.ResumeSpec{RunID: runID, FilePath: "wf.bot", Source: "workflow wf:\n  entry: done\n"}
	if err := p.SubmitResume(ctx, spec, wf, "hash"); err == nil {
		t.Fatal("SubmitResume returned nil, want publish failure")
	}
	if len(spy.cleared) != 0 {
		t.Fatalf("publish failure cleared %v retries, want none (a rollback that also drops the armed retry strands the run)", spy.cleared)
	}
}

func TestSubmitResume_ConcurrentRequestsPublishOnce(t *testing.T) {
	base, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	ctx := store.WithIdentity(context.Background(), "team", "alice")
	const runID = "run-concurrent-resume"
	if err := base.SaveRun(ctx, &store.Run{
		ID: runID, TenantID: "team", OwnerID: "alice", Status: store.RunStatusFailedResumable,
	}); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	barrier := &resumeLoadBarrierStore{
		RunStore: base,
		ready:    make(chan struct{}),
		release:  make(chan struct{}),
	}
	var publishes atomic.Int32
	p := &Publisher{
		store: barrier,
		publishRun: func(context.Context, *queue.RunMessage) error {
			publishes.Add(1)
			return nil
		},
	}
	wf := &ir.Workflow{Name: "concurrent_resume"}
	spec := runview.ResumeSpec{
		RunID: runID, FilePath: "concurrent_resume.bot",
		Source: "workflow concurrent_resume:\n  entry: done\n",
	}
	errs := make(chan error, 2)
	for range 2 {
		go func() { errs <- p.SubmitResume(ctx, spec, wf, "hash") }()
	}
	select {
	case <-barrier.ready:
		close(barrier.release)
	case <-time.After(2 * time.Second):
		t.Fatal("concurrent resumes did not reach the pre-CAS barrier")
	}

	var successes, raced int
	for range 2 {
		if err := <-errs; err == nil {
			successes++
		} else if strings.Contains(err.Error(), "resume raced") {
			raced++
		} else {
			t.Fatalf("unexpected resume error: %v", err)
		}
	}
	if successes != 1 || raced != 1 || publishes.Load() != 1 {
		t.Fatalf("concurrent resumes: successes=%d raced=%d publishes=%d, want 1/1/1", successes, raced, publishes.Load())
	}
}

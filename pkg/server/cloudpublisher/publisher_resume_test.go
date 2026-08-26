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

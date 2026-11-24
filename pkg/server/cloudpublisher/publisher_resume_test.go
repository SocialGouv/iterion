package cloudpublisher

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/permission"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestSubmitResume_RefusesAnAnswerForAnotherApproval(t *testing.T) {
	for _, id := range []string{"", "previous"} {
		st, err := store.New(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		ctx := store.WithIdentity(context.Background(), "team", "alice")
		run := &store.Run{ID: "approval", TenantID: "team", OwnerID: "alice", Status: store.RunStatusPausedWaitingHuman,
			Checkpoint: &store.Checkpoint{InteractionID: "current", BackendName: delegate.BackendClaw, BackendPermissionInput: []byte(`{"label":"cyan"}`)},
		}
		if err := st.SaveRun(ctx, run); err != nil {
			t.Fatal(err)
		}
		published := false
		p := &Publisher{store: st, publishRun: func(context.Context, *queue.RunMessage) error { published = true; return nil }}
		spec := runview.ResumeSpec{RunID: run.ID, FilePath: "wf.bot", Source: "workflow wf:\n  entry: done\n",
			Answers: map[string]any{delegate.AskUserQuestionKey: "allow", permission.ApprovalInteractionKey: id},
		}
		err = p.SubmitResume(ctx, spec, &ir.Workflow{Name: "wf"}, nil)
		if err == nil || !strings.Contains(err.Error(), "approval does not match the pending interaction") || published {
			t.Fatalf("approval answer ID %q: error = %v, published = %t; want refusal before publication", id, err, published)
		}
		got, err := st.LoadRun(ctx, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != run.Status || got.CASVersion != run.CASVersion || got.Checkpoint == nil || got.Checkpoint.InteractionID != "current" {
			t.Fatal("refusal changed the pending pause")
		}
	}
}

type replaceApprovalOnQueue struct {
	*store.FilesystemRunStore
}

func (s replaceApprovalOnQueue) FlipRevisionToQueued(ctx context.Context, id string, from store.RunStatus, at time.Time, version int64) (store.QueuedFlip, bool, error) {
	run, err := s.LoadRun(ctx, id)
	if err != nil {
		return store.QueuedFlip{}, false, err
	}
	run.Checkpoint.InteractionID = "replacement"
	if err := s.SaveRun(ctx, run); err != nil {
		return store.QueuedFlip{}, false, err
	}
	return s.FilesystemRunStore.FlipRevisionToQueued(ctx, id, from, at, version)
}

func TestSubmitResume_ApprovalFlipBindsTheCheckedRevision(t *testing.T) {
	for _, replace := range []bool{false, true} {
		st, err := store.New(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		ctx := store.WithIdentity(context.Background(), "team", "alice")
		run := &store.Run{ID: "approval", TenantID: "team", OwnerID: "alice", Status: store.RunStatusPausedWaitingHuman,
			Checkpoint: &store.Checkpoint{InteractionID: "current", BackendName: delegate.BackendClaw, BackendPermissionInput: []byte(`{"label":"cyan"}`)},
		}
		if err := st.SaveRun(ctx, run); err != nil {
			t.Fatal(err)
		}
		var runs store.RunStore = st
		if replace {
			runs = replaceApprovalOnQueue{st}
		}
		var published *queue.RunMessage
		p := &Publisher{store: runs, publishRun: func(_ context.Context, msg *queue.RunMessage) error { published = msg; return nil }}
		spec := runview.ResumeSpec{RunID: run.ID, FilePath: "wf.bot", Source: "workflow wf:\n  entry: done\n",
			Answers: map[string]any{delegate.AskUserQuestionKey: "allow", permission.ApprovalInteractionKey: "current"},
		}
		err = p.SubmitResume(ctx, spec, &ir.Workflow{Name: "wf"}, nil)
		if !replace {
			if err != nil || published == nil || published.Resume == nil || published.Resume.Answers[permission.ApprovalInteractionKey] != "current" {
				t.Fatalf("current answer was not published intact: %v", err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), "resume raced") || published != nil {
			t.Fatalf("replacement won before the flip: error = %v, published = %t", err, published != nil)
		}
		got, err := st.LoadRun(ctx, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != store.RunStatusPausedWaitingHuman || got.Checkpoint == nil || got.Checkpoint.InteractionID != "replacement" {
			t.Fatal("refusal changed the replacement pause")
		}
	}
}

// The documented --force recovery must reach the queue in cloud mode: the
// acknowledged flip is allowed past an unresolved marker, an unforced one is
// not.
func TestSubmitResume_ForceQueuesAnUnresolvedApprovalRun(t *testing.T) {
	for _, force := range []bool{false, true} {
		st, err := store.New(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		ctx := store.WithIdentity(context.Background(), "team", "alice")
		// Build the ambiguous state through the store primitives: SaveRun
		// normalizes the marker away on direct creation.
		const runID = "stale-approval"
		if err := st.SaveRun(ctx, &store.Run{ID: runID, TenantID: "team", OwnerID: "alice", Status: store.RunStatusPausedWaitingHuman}); err != nil {
			t.Fatal(err)
		}
		spender := store.AsToolApprovalSpender(st)
		if spender == nil {
			t.Fatal("store does not support tool approvals")
		}
		if err := st.PauseRun(ctx, runID, &store.Checkpoint{NodeID: "worker", InteractionID: "choice", BackendPermissionInput: []byte(`{"label":"cyan"}`)}); err != nil {
			t.Fatal(err)
		}
		paused, err := st.LoadRun(ctx, runID)
		if err != nil {
			t.Fatal(err)
		}
		claim := store.ToolApprovalClaim{InteractionID: "choice", Token: "tok"}
		if won, err := spender.ClaimToolApprovalResume(ctx, runID, claim, paused.Status, paused.CASVersion); err != nil || !won {
			t.Fatalf("claim: won=%v err=%v", won, err)
		}
		if won, err := spender.SpendToolApproval(ctx, runID, claim); err != nil || !won {
			t.Fatalf("spend: won=%v err=%v", won, err)
		}
		if code, err := spender.FailOwnedToolApproval(ctx, runID, claim, &store.Checkpoint{NodeID: "worker"}, "interrupted", store.FailureExecutionFailed); err != nil || code != store.FailureAmbiguousEffect {
			t.Fatalf("fail: code=%q err=%v", code, err)
		}
		published := false
		p := &Publisher{store: st, publishRun: func(context.Context, *queue.RunMessage) error { published = true; return nil }}
		spec := runview.ResumeSpec{RunID: runID, Force: force, FilePath: "wf.bot", Source: "workflow wf:\n  entry: done\n"}
		err = p.SubmitResume(ctx, spec, &ir.Workflow{Name: "wf"}, nil)
		if !force {
			if err == nil || published {
				t.Fatalf("unforced resume of an unresolved run = %v (published %t), want refusal", err, published)
			}
			continue
		}
		if err != nil || !published {
			t.Fatalf("forced resume of an unresolved run = %v (published %t), want queued", err, published)
		}
		got, err := st.LoadRun(ctx, runID)
		if err != nil || got.Status != store.RunStatusQueued {
			t.Fatalf("forced resume left status %s (err %v), want queued", got.Status, err)
		}
	}
}

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

	err = p.SubmitResume(ctx, spec, wf, &runview.CompiledSource{Hash: "hash"})
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

func (r *retryStoreSpy) ScheduleRunRetry(_ context.Context, runID string, _, _ time.Time, _, _ string, _ int) (bool, int, error) {
	r.armed = append(r.armed, runID)
	return true, 1, nil
}

func (r *retryStoreSpy) PullRunRetry(_ context.Context, _ string, _, _ time.Time) (bool, error) {
	return false, nil
}

func (r *retryStoreSpy) ReleaseRunRetry(_ context.Context, _ string, _, _ time.Time, _ string) (bool, error) {
	return false, nil
}

func (r *retryStoreSpy) RearmProbedRunRetry(_ context.Context, _ string, _, _ time.Time) (bool, error) {
	return false, nil
}

func (r *retryStoreSpy) DelayRunRetry(_ context.Context, _ string, _, _ time.Time, _, _ string) (bool, error) {
	return false, nil
}
func (r *retryStoreSpy) ClaimRunRetry(_ context.Context, runID string, _ time.Time) (bool, error) {
	r.claimed = append(r.claimed, runID)
	return true, nil
}
func (r *retryStoreSpy) ClearRunRetry(_ context.Context, runID string) error {
	r.cleared = append(r.cleared, runID)
	return nil
}
func (r *retryStoreSpy) AbandonRunRetry(_ context.Context, runID, _ string, _ time.Time) (bool, error) {
	r.abandoned = append(r.abandoned, runID)
	return true, nil
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
	if err := p.SubmitResume(ctx, spec, wf, &runview.CompiledSource{Hash: "hash"}); err != nil {
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
	if err := p.SubmitResume(ctx, spec, wf, &runview.CompiledSource{Hash: "hash"}); err == nil {
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
		go func() { errs <- p.SubmitResume(ctx, spec, wf, &runview.CompiledSource{Hash: "hash"}) }()
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

func TestSubmitResume_MissionExpectedStatusAndReceiptCrossPublisherCAS(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := store.WithIdentity(context.Background(), "team", "alice")
	const runID = "run-mission-receipt"
	if err := st.SaveRun(ctx, &store.Run{ID: runID, TenantID: "team", OwnerID: "alice", Status: store.RunStatusFailedResumable}); err != nil {
		t.Fatal(err)
	}
	var published *queue.RunMessage
	p := &Publisher{store: st, publishRun: func(_ context.Context, msg *queue.RunMessage) error { published = msg; return nil }}
	wf := &ir.Workflow{Name: "wf"}
	spec := runview.ResumeSpec{RunID: runID, FilePath: "wf.bot", Source: "workflow wf:\n  entry: done\n", ExpectedStatus: store.RunStatusPausedOperator, ReceiptID: "receipt-1"}
	if err := p.SubmitResume(ctx, spec, wf, &runview.CompiledSource{Hash: "hash"}); err == nil || !strings.Contains(err.Error(), "status changed") {
		t.Fatalf("mismatched expected status = %v", err)
	}
	if published != nil {
		t.Fatal("mismatched expected status reached the queue")
	}
	spec.ExpectedStatus = store.RunStatusFailedResumable
	if err := p.SubmitResume(ctx, spec, wf, &runview.CompiledSource{Hash: "hash"}); err != nil {
		t.Fatal(err)
	}
	if published == nil || published.Resume == nil || published.Resume.ExpectedStatus != store.RunStatusFailedResumable || published.Resume.ReceiptID != "receipt-1" {
		t.Fatalf("mission wire fields lost: %#v", published)
	}
}

// TestSubmitResume_PublishesTheStatusItMovedTheRunFrom: the runner puts a
// resume the engine refused before its claim back where it came from, and
// only the publisher knows where that is — the queued flip erased it.
func TestSubmitResume_PublishesTheStatusItMovedTheRunFrom(t *testing.T) {
	for _, from := range []store.RunStatus{store.RunStatusPausedWaitingHuman, store.RunStatusPausedOperator, store.RunStatusFailedResumable} {
		t.Run(string(from), func(t *testing.T) {
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			ctx := store.WithIdentity(context.Background(), "team", "alice")
			const runID = "run-prior-status"
			if err := st.SaveRun(ctx, &store.Run{ID: runID, TenantID: "team", OwnerID: "alice", Status: from}); err != nil {
				t.Fatal(err)
			}
			var published *queue.RunMessage
			p := &Publisher{store: st, publishRun: func(_ context.Context, msg *queue.RunMessage) error { published = msg; return nil }}
			spec := runview.ResumeSpec{RunID: runID, FilePath: "wf.bot", Source: "workflow wf:\n  entry: done\n"}
			if err := p.SubmitResume(ctx, spec, &ir.Workflow{Name: "wf"}, &runview.CompiledSource{Hash: "hash"}); err != nil {
				t.Fatal(err)
			}
			if published == nil || published.Resume == nil || published.Resume.PriorStatus != from {
				t.Fatalf("published resume = %#v, want prior status %s", published, from)
			}
		})
	}
}

// TestSubmitResume_carriesTheScratchsConsent: the operator's consent to the
// scratch's loss rides the resume message, and nothing else gives it.
func TestSubmitResume_carriesTheScratchsConsent(t *testing.T) {
	for _, accept := range []bool{false, true} {
		st, err := store.New(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		ctx := store.WithIdentity(context.Background(), "team", "alice")
		const runID = "run-scratch-consent"
		if err := st.SaveRun(ctx, &store.Run{ID: runID, TenantID: "team", OwnerID: "alice", Status: store.RunStatusFailedResumable}); err != nil {
			t.Fatal(err)
		}
		var published *queue.RunMessage
		p := &Publisher{store: st, publishRun: func(_ context.Context, msg *queue.RunMessage) error { published = msg; return nil }}
		spec := runview.ResumeSpec{RunID: runID, FilePath: "wf.bot", Source: "workflow wf:\n  entry: done\n", Force: true, AcceptScratchLoss: accept}
		if err := p.SubmitResume(ctx, spec, &ir.Workflow{Name: "wf"}, &runview.CompiledSource{Hash: "hash"}); err != nil {
			t.Fatal(err)
		}
		if published == nil || published.Resume == nil || published.Resume.AcceptScratchLoss != accept || !published.Resume.Force {
			t.Fatalf("accept=%v: published resume = %#v, want the consent carried as given, force kept", accept, published)
		}
	}
}

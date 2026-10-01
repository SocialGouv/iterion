package cloudpublisher

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A resume refused before its publication puts back what its own flip to
// queued replaced, and only that: these tests drive SubmitResume's rollback.

func rollbackTestSource() *runview.CompiledSource {
	return &runview.CompiledSource{Hash: "h", Main: "main.bot", Files: map[string]string{"main.bot": "workflow w:\n  entry: a\n  a -> done\n"}}
}

// TestSubmitResume_aRefusedResumeLeavesAConcurrentResumesAttemptAlone: resume
// A flips the run to queued and is slow to publish; in its window an operator
// cancels the run and resumes it again (B), which publishes. A is then
// refused, and its rollback leaves B's attempt as B left it: queued, B's
// marker, B's recorded source — B's delivery is admitted with B's answers and
// consent, not dropped or stripped.
func TestSubmitResume_aRefusedResumeLeavesAConcurrentResumesAttemptAlone(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	ctx := store.WithIdentity(context.Background(), "team", "alice")
	const runID = "run-rollback-race"
	prior := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	if err := st.SaveRun(ctx, &store.Run{ID: runID, TenantID: "team", OwnerID: "alice", Status: store.RunStatusPausedWaitingHuman, QueuedAt: &prior}); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	aInPublish := make(chan struct{})
	releaseA := make(chan struct{})
	var mu sync.Mutex
	var publishedB *queue.RunMessage
	p := &Publisher{
		store:              st,
		publishRetryDelays: []time.Duration{},
		cancelRun:          func(string) error { return nil },
		publishRun: func(_ context.Context, m *queue.RunMessage) error {
			if m.Resume != nil && m.Resume.PriorStatus == store.RunStatusPausedWaitingHuman {
				close(aInPublish)
				<-releaseA
				return errors.New("nats: publish ack timeout (A)")
			}
			mu.Lock()
			publishedB = m
			mu.Unlock()
			return nil
		},
	}
	cs := rollbackTestSource()
	aErr := make(chan error, 1)
	go func() {
		aErr <- p.SubmitResume(ctx, runview.ResumeSpec{RunID: runID, FilePath: "main.bot", Source: cs.Files["main.bot"], Answers: map[string]any{"approve": true}}, &ir.Workflow{Name: "w"}, cs)
	}()
	<-aInPublish
	if err := p.CancelRun(ctx, runID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	// An attempt's marker is to the millisecond (store.QueuedFlipAt): two
	// flips in one are one attempt to every reader — a cancel and a resume
	// by request take longer than that.
	time.Sleep(2 * time.Millisecond)
	csB := &runview.CompiledSource{Hash: "hB", Main: "main.bot", Files: map[string]string{"main.bot": "workflow w:\n  entry: b\n  b -> done\n"}}
	if err := p.SubmitResume(ctx, runview.ResumeSpec{RunID: runID, FilePath: "main.bot", Source: csB.Files["main.bot"], AcceptScratchLoss: true}, &ir.Workflow{Name: "w"}, csB); err != nil {
		t.Fatalf("B's resume = %v, want success", err)
	}
	afterB, err := st.LoadRun(ctx, runID)
	if err != nil || afterB.QueuedAt == nil {
		t.Fatalf("after B: %v (%v)", afterB, err)
	}
	close(releaseA)
	if err := <-aErr; err == nil || !strings.Contains(err.Error(), "(A)") {
		t.Fatalf("A = %v, want its publish failure", err)
	}
	final, err := st.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	published, perr := time.Parse(time.RFC3339Nano, publishedB.PublishedAtRFC)
	if perr != nil {
		t.Fatal(perr)
	}
	if final.Status != store.RunStatusQueued || final.QueuedAt == nil || !final.QueuedAt.Equal(*afterB.QueuedAt) || final.QueuedAt.After(published) {
		t.Fatalf("A's rollback reverted B's published attempt: status %s queued_at %v, want queued at B's marker %v (B published at %s)", final.Status, final.QueuedAt, afterB.QueuedAt, publishedB.PublishedAtRFC)
	}
	if final.WorkflowHash != "hB" || !strings.Contains(final.WorkflowSource, "entry: b") {
		t.Fatalf("A's rollback put back the rewind baseline over B's: %q (%s)", final.WorkflowSource, final.WorkflowHash)
	}
}

// TestSubmitResume_aRefusedResumeKeepsTheRunsEpisodeAndContinuation: a
// resume refused before its publication is no new outcome: the run keeps its
// episode, and the continuation it carried — an armed retry, a pending
// redelivery, a final park — instead of an unknown one.
func TestSubmitResume_aRefusedResumeKeepsTheRunsEpisodeAndContinuation(t *testing.T) {
	for _, cont := range []store.ContinuationState{store.ContinuationRetryArmed, store.ContinuationRedeliveryPending, store.ContinuationFinal} {
		t.Run(string(cont), func(t *testing.T) {
			st, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			ctx := store.WithIdentity(context.Background(), "team", "alice")
			const runID = "run-bookkeeping"
			prior := time.Now().UTC().Add(-time.Hour)
			if err := st.SaveRun(ctx, &store.Run{ID: runID, TenantID: "team", OwnerID: "alice", Status: store.RunStatusRunning, QueuedAt: &prior}); err != nil {
				t.Fatal(err)
			}
			if ok, err := st.UpdateRunOutcome(ctx, runID, store.RunStatusFailedResumable, "usage window",
				store.RunOutcomeMeta{Code: store.FailureUsageLimitBlocked, Continuation: cont}, []store.RunStatus{store.RunStatusRunning}); err != nil || !ok {
				t.Fatalf("park: %v %v", ok, err)
			}
			before, err := st.LoadRun(ctx, runID)
			if err != nil {
				t.Fatal(err)
			}
			p := &Publisher{store: st, publishRetryDelays: []time.Duration{}, publishRun: func(context.Context, *queue.RunMessage) error {
				return errors.New("nats unavailable")
			}}
			cs := rollbackTestSource()
			if err := p.SubmitResume(ctx, runview.ResumeSpec{RunID: runID, FilePath: "main.bot", Source: cs.Files["main.bot"]}, &ir.Workflow{Name: "w"}, cs); err == nil {
				t.Fatal("SubmitResume succeeded: want the publish failure, for the rollback to be judged")
			}
			after, err := st.LoadRun(ctx, runID)
			if err != nil {
				t.Fatal(err)
			}
			if after.Status != store.RunStatusFailedResumable || after.OutcomeSeq != before.OutcomeSeq || after.ContinuationState != before.ContinuationState || after.FailureCode != before.FailureCode {
				t.Fatalf("after the refused resume: status %s outcome_seq %d continuation %q code %q, want %s %d %q %q — a refused resume is no new episode", after.Status, after.OutcomeSeq, after.ContinuationState, after.FailureCode,
					before.Status, before.OutcomeSeq, before.ContinuationState, before.FailureCode)
			}
			if !strings.Contains(after.Error, "queue resume") || !strings.Contains(after.Error, "nats unavailable") {
				t.Fatalf("after the refused resume: error %q, want it to say why the resume was refused", after.Error)
			}
		})
	}
}

// flipInterleaver runs `before` once, right before the first flip to queued
// it is asked for: between SubmitResume's read of the run and its flip.
type flipInterleaver struct {
	store.RunStore
	before func()
	done   bool
}

func (s *flipInterleaver) Unwrap() store.RunStore { return s.RunStore }

func (s *flipInterleaver) FlipToQueued(ctx context.Context, id string, from store.RunStatus, at time.Time) (store.QueuedFlip, bool, error) {
	if !s.done {
		s.done = true
		s.before()
	}
	return store.AsQueuedFlipper(s.RunStore).FlipToQueued(ctx, id, from, at)
}

func (s *flipInterleaver) RevertQueuedFlip(ctx context.Context, id string, flip store.QueuedFlip, runErr string) (bool, error) {
	return store.AsQueuedFlipper(s.RunStore).RevertQueuedFlip(ctx, id, flip, runErr)
}

func (s *flipInterleaver) UpdateRunStatusIf(ctx context.Context, id string, status store.RunStatus, runErr string, from []store.RunStatus) (bool, error) {
	if status == store.RunStatusQueued && !s.done {
		s.done = true
		s.before()
	}
	return s.RunStore.UpdateRunStatusIf(ctx, id, status, runErr, from)
}

// TestSubmitResume_aRefusedResumeRestoresWhatItsFlipReplaced: a whole
// attempt (B: flip, publication, claim, park) lands between a resume's read
// of the run and its flip. Refused, the resume puts back what its flip
// replaced — B's marker — never the older one it read first, which would
// make every delivery published since read as current again.
func TestSubmitResume_aRefusedResumeRestoresWhatItsFlipReplaced(t *testing.T) {
	inner, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := store.WithIdentity(context.Background(), "team", "alice")
	const runID = "run-stale-prior"
	t0 := time.Now().UTC().Add(-time.Hour)
	if err := inner.SaveRun(ctx, &store.Run{ID: runID, TenantID: "team", OwnerID: "alice", Status: store.RunStatusFailedResumable, QueuedAt: &t0}); err != nil {
		t.Fatal(err)
	}
	var tB time.Time
	st := &flipInterleaver{RunStore: inner, before: func() {
		if ok, err := inner.UpdateRunStatusIf(ctx, runID, store.RunStatusQueued, "", []store.RunStatus{store.RunStatusFailedResumable}); err != nil || !ok {
			t.Fatalf("B's flip: %v %v", ok, err)
		}
		r, _ := inner.LoadRun(ctx, runID)
		tB = *r.QueuedAt
		if ok, err := inner.UpdateRunStatusIf(ctx, runID, store.RunStatusRunning, "", []store.RunStatus{store.RunStatusQueued}); err != nil || !ok {
			t.Fatalf("B's claim: %v %v", ok, err)
		}
		if ok, err := inner.UpdateRunOutcome(ctx, runID, store.RunStatusFailedResumable, "B died", store.RunOutcomeMeta{}, []store.RunStatus{store.RunStatusRunning}); err != nil || !ok {
			t.Fatalf("B's park: %v %v", ok, err)
		}
		time.Sleep(2 * time.Millisecond)
	}}
	p := &Publisher{store: st, publishRetryDelays: []time.Duration{}, publishRun: func(context.Context, *queue.RunMessage) error {
		return errors.New("nats unavailable")
	}}
	cs := rollbackTestSource()
	if err := p.SubmitResume(ctx, runview.ResumeSpec{RunID: runID, FilePath: "main.bot", Source: cs.Files["main.bot"]}, &ir.Workflow{Name: "w"}, cs); err == nil {
		t.Fatal("SubmitResume succeeded: want the publish failure")
	}
	if !st.done || tB.IsZero() {
		t.Fatal("no attempt landed before the resume's flip: nothing judged")
	}
	final, err := inner.LoadRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Status != store.RunStatusFailedResumable || final.QueuedAt == nil || !final.QueuedAt.Equal(tB) {
		t.Fatalf("after the refused resume: status %s queued_at %v, want failed_resumable at B's marker %v (A read %v)", final.Status, final.QueuedAt, tB, t0)
	}
}

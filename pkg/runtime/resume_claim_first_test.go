package runtime

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A delivery's resume (WithQueuedAttempt) claims its run before its pause
// path writes anything (claimsFirst). These tests drive Engine.Resume as the
// runner does, through every pause path.

// cfCase is a run a delivery resumes through a pause path.
type cfCase struct {
	name string
	// setup brings runID to what the delivery reads, and returns the
	// delivery's publication.
	setup func(t *testing.T, s store.RunStore, runID string) time.Time
	// engine builds the delivery's engine on s.
	engine  func(s store.RunStore, opts ...EngineOption) *Engine
	answers map[string]any
}

func cfGateEngine(s store.RunStore, opts ...EngineOption) *Engine {
	return New(gateReplayWorkflow(), s, newStubExecutor(), opts...)
}

// cfPausedAtGate runs the gate workflow to its pause.
func cfPausedAtGate(t *testing.T, s store.RunStore, runID string) {
	t.Helper()
	if err := New(gateReplayWorkflow(), s, newStubExecutor()).Run(context.Background(), runID, nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("Run: want ErrRunPaused, got %v", err)
	}
}

// cfAwaitWorkflow is a human node paused on two async questions
// (cfAwaitPause), then done.
func cfAwaitWorkflow() *ir.Workflow {
	return &ir.Workflow{Name: "cf_await", Entry: "asker", Nodes: map[string]ir.Node{
		"asker": &ir.HumanNode{BaseNode: ir.BaseNode{ID: "asker"}, InteractionFields: ir.InteractionFields{Interaction: ir.InteractionHuman}},
		"done":  &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
	}, Edges: []*ir.Edge{{From: "asker", To: "done"}}}
}

func cfAwaitPause(t *testing.T, s store.RunStore, runID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.CreateRun(ctx, runID, "cf_await", nil); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"q1", "q2"} {
		if err := s.WriteInteraction(ctx, &store.Interaction{ID: id, RunID: runID, NodeID: "asker", Kind: store.InteractionKindAsync, RequestedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.PauseRun(ctx, runID, &store.Checkpoint{NodeID: "asker", InteractionID: "pause-1", InteractionQuestions: map[string]any{
		delegate.AwaitPendingInteractionsKey: delegate.AwaitPendingToQuestions([]delegate.PendingAsync{{InteractionID: "q1"}, {InteractionID: "q2"}}),
	}}); err != nil {
		t.Fatal(err)
	}
}

func cfCases() []cfCase {
	recovery := &flakyExecutor{target: "agent_a", failErr: &RuntimeError{Code: ErrCodeAuthFailed, Message: "authentication failed"}, failures: 1}
	recoveryEngine := func(s store.RunStore, opts ...EngineOption) *Engine {
		return New(newRecoveryGateWorkflow(), s, recovery, append([]EngineOption{WithRecoveryDispatch(pauseForHumanOn(ErrCodeAuthFailed))}, opts...)...)
	}
	return []cfCase{
		{
			name: "a queued gate",
			setup: func(t *testing.T, s store.RunStore, runID string) time.Time {
				cfPausedAtGate(t, s, runID)
				return queueAttempt(t, s, runID, store.RunStatusPausedWaitingHuman)
			},
			engine:  cfGateEngine,
			answers: map[string]any{"decision": "go"},
		},
		{
			name: "a queued parallel gate",
			setup: func(t *testing.T, s store.RunStore, runID string) time.Time {
				wf, exec := parallelGateWorkflow()
				if err := New(wf, s, exec).Run(context.Background(), runID, nil); !errors.Is(err, ErrRunPaused) {
					t.Fatalf("Run: want ErrRunPaused, got %v", err)
				}
				return queueAttempt(t, s, runID, store.RunStatusPausedWaitingHuman)
			},
			engine: func(s store.RunStore, opts ...EngineOption) *Engine {
				wf, exec := parallelGateWorkflow()
				return New(wf, s, exec, opts...)
			},
			answers: map[string]any{"decision": "go"},
		},
		{
			name: "a queued answered gate's replay",
			setup: func(t *testing.T, s store.RunStore, runID string) time.Time {
				parkedAtGate(t, s, runID, true)
				return queueAttempt(t, s, runID, store.RunStatusFailedResumable)
			},
			engine: cfGateEngine,
		},
		{
			// The redelivery of an attempt its previous delivery claimed, then
			// parked: the run reads failed_resumable, its gate answered.
			name: "a redelivered answered gate's replay",
			setup: func(t *testing.T, s store.RunStore, runID string) time.Time {
				ctx := context.Background()
				parkedAtGate(t, s, runID, true)
				published := queueAttempt(t, s, runID, store.RunStatusFailedResumable)
				if ok, err := s.UpdateRunStatusIf(ctx, runID, store.RunStatusRunning, "", []store.RunStatus{store.RunStatusQueued}); err != nil || !ok {
					t.Fatalf("the previous delivery's claim: %v %v", ok, err)
				}
				if ok, err := s.UpdateRunOutcome(ctx, runID, store.RunStatusFailedResumable, "drain", store.RunOutcomeMeta{Continuation: store.ContinuationRedeliveryPending}, []store.RunStatus{store.RunStatusRunning}); err != nil || !ok {
					t.Fatalf("the previous delivery's park: %v %v", ok, err)
				}
				return published
			},
			engine: cfGateEngine,
		},
		{
			name: "a queued recovery pause",
			setup: func(t *testing.T, s store.RunStore, runID string) time.Time {
				if err := recoveryEngine(s).Run(context.Background(), runID, nil); !errors.Is(err, ErrRunPaused) {
					t.Fatalf("Run: want ErrRunPaused, got %v", err)
				}
				return queueAttempt(t, s, runID, store.RunStatusPausedWaitingHuman)
			},
			engine:  recoveryEngine,
			answers: map[string]any{"acknowledge_recovery": "retry"},
		},
		{
			name: "a queued await gate",
			setup: func(t *testing.T, s store.RunStore, runID string) time.Time {
				cfAwaitPause(t, s, runID)
				return queueAttempt(t, s, runID, store.RunStatusPausedWaitingHuman)
			},
			engine: func(s store.RunStore, opts ...EngineOption) *Engine {
				return New(cfAwaitWorkflow(), s, newStubExecutor(), opts...)
			},
			answers: map[string]any{"q1": "blue", "q2": "green"},
		},
	}
}

// cfPlainResumeRacer is an operator resuming the run — the publisher's
// paused_waiting_human → queued flip, a newer attempt — before every store
// call the delivery makes once armed. It lands the moment the run reads
// paused; a delivery that claims first never lets it.
type cfPlainResumeRacer struct {
	store.RunStore
	runID  string
	mu     sync.Mutex
	armed  bool
	landed string
}

func (s *cfPlainResumeRacer) Unwrap() store.RunStore { return s.RunStore }

func (s *cfPlainResumeRacer) race(call string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.armed || s.landed != "" {
		return
	}
	if ok, err := s.RunStore.UpdateRunStatusIf(context.Background(), s.runID, store.RunStatusQueued, "", []store.RunStatus{store.RunStatusPausedWaitingHuman}); err == nil && ok {
		s.landed = call
	}
}

func (s *cfPlainResumeRacer) LoadRun(ctx context.Context, id string) (*store.Run, error) {
	s.race("LoadRun")
	return s.RunStore.LoadRun(ctx, id)
}

func (s *cfPlainResumeRacer) LoadInteraction(ctx context.Context, runID, id string) (*store.Interaction, error) {
	s.race("LoadInteraction")
	return s.RunStore.LoadInteraction(ctx, runID, id)
}

func (s *cfPlainResumeRacer) ListInteractions(ctx context.Context, runID string) ([]string, error) {
	s.race("ListInteractions")
	return s.RunStore.ListInteractions(ctx, runID)
}

func (s *cfPlainResumeRacer) WriteInteraction(ctx context.Context, in *store.Interaction) error {
	s.race("WriteInteraction")
	return s.RunStore.WriteInteraction(ctx, in)
}

func (s *cfPlainResumeRacer) AppendEvent(ctx context.Context, runID string, ev store.Event) (*store.Event, error) {
	s.race("AppendEvent " + string(ev.Type))
	return s.RunStore.AppendEvent(ctx, runID, ev)
}

func (s *cfPlainResumeRacer) WriteArtifact(ctx context.Context, a *store.Artifact) error {
	s.race("WriteArtifact")
	return s.RunStore.WriteArtifact(ctx, a)
}

func (s *cfPlainResumeRacer) SaveCheckpoint(ctx context.Context, id string, cp *store.Checkpoint) error {
	s.race("SaveCheckpoint")
	return s.RunStore.SaveCheckpoint(ctx, id, cp)
}

func (s *cfPlainResumeRacer) PauseRun(ctx context.Context, id string, cp *store.Checkpoint) error {
	s.race("PauseRun")
	return s.RunStore.PauseRun(ctx, id, cp)
}

func (s *cfPlainResumeRacer) UpdateRunStatusIf(ctx context.Context, id string, status store.RunStatus, runErr string, from []store.RunStatus) (bool, error) {
	s.race("UpdateRunStatusIf")
	return s.RunStore.UpdateRunStatusIf(ctx, id, status, runErr, from)
}

// TestResume_aDeliveryNeverLeavesItsRunToAPlainResume: whatever pause path a
// delivery resumes through — a gate, a parallel branch's gate, an answered
// gate's replay from queued or from its park, a recovery pause, an await
// gate — its run never reads paused_waiting_human while the delivery works on
// it: an operator's plain resume cannot queue a newer attempt under the
// path's writes. The delivery claims the run and finishes it.
func TestResume_aDeliveryNeverLeavesItsRunToAPlainResume(t *testing.T) {
	for _, tc := range cfCases() {
		t.Run(tc.name, func(t *testing.T) {
			base := tmpStore(t)
			const runID = "run-cf-racer"
			published := tc.setup(t, base, runID)
			s := &cfPlainResumeRacer{RunStore: base, runID: runID, armed: true}
			claimed := false
			err := tc.engine(s, WithQueuedAttempt(published), WithOnResumeClaimed(func() { claimed = true })).Resume(context.Background(), runID, tc.answers)
			s.mu.Lock()
			landed := s.landed
			s.armed = false
			s.mu.Unlock()
			if landed != "" {
				t.Fatalf("a plain resume queued a newer attempt before the delivery's %s: the run read paused_waiting_human while the delivery resumed it (delivery: err=%v claimed=%v)", landed, err, claimed)
			}
			doc, lerr := base.LoadRun(context.Background(), runID)
			if lerr != nil {
				t.Fatal(lerr)
			}
			if err != nil || !claimed || doc.Status != store.RunStatusFinished {
				t.Fatalf("the delivery: err=%v claimed=%v status=%s, want it claimed and finished", err, claimed, doc.Status)
			}
		})
	}
}

// cfNewerAttemptAtTheClaim queues a newer attempt right before the
// delivery's claim — an operator's cancel then resume of a queued run, the
// resume of a parked one.
type cfNewerAttemptAtTheClaim struct {
	store.RunStore
	t     *testing.T
	runID string
	once  sync.Once
	newer time.Time
}

func (s *cfNewerAttemptAtTheClaim) Unwrap() store.RunStore { return s.RunStore }

func (s *cfNewerAttemptAtTheClaim) ClaimQueuedRunIfAttempt(ctx context.Context, id string, publishedAt time.Time) (bool, error) {
	if id == s.runID {
		s.once.Do(func() { s.newer = cfQueueNewerAttempt(s.t, s.RunStore, id) })
	}
	return store.AsQueuedAttemptClaimer(s.RunStore).ClaimQueuedRunIfAttempt(ctx, id, publishedAt)
}

// cfQueueNewerAttempt queues a newer attempt of runID from whatever status
// it holds — cancelled first when it is queued — and returns its marker.
func cfQueueNewerAttempt(t *testing.T, s store.RunStore, runID string) time.Time {
	t.Helper()
	ctx := context.Background()
	r, err := s.LoadRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	from := r.Status
	if from == store.RunStatusQueued {
		if ok, err := s.UpdateRunStatusIf(ctx, runID, store.RunStatusCancelled, "operator", []store.RunStatus{from}); err != nil || !ok {
			t.Fatalf("cancel: %v %v", ok, err)
		}
		from = store.RunStatusCancelled
	}
	time.Sleep(2 * time.Millisecond)
	if ok, err := s.UpdateRunStatusIf(ctx, runID, store.RunStatusQueued, "", []store.RunStatus{from}); err != nil || !ok {
		t.Fatalf("the newer attempt's flip from %s: %v %v", from, ok, err)
	}
	r, err = s.LoadRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	return *r.QueuedAt
}

// cfWritesSince lists the pause path's writes on runID after its first
// `since` events: the answers, the gate's artifact, its node's finish, the
// claim's run_resumed.
func cfWritesSince(t *testing.T, s store.RunStore, runID string, since int) []string {
	t.Helper()
	evs, err := s.LoadEvents(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, ev := range evs[since:] {
		switch ev.Type {
		case store.EventHumanAnswersRecorded, store.EventInteractionAnswered, store.EventArtifactWritten, store.EventNodeFinished, store.EventRunResumed:
			out = append(out, string(ev.Type)+" "+ev.NodeID)
		}
	}
	return out
}

func cfEventCount(t *testing.T, s store.RunStore, runID string) int {
	t.Helper()
	evs, err := s.LoadEvents(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	return len(evs)
}

// TestResume_aDeliveryOutrunAtItsClaimWritesNothing: a newer attempt queued
// right before a delivery's claim takes the run from it — and finds nothing
// of the delivery on it: no answer recorded, no gate artifact, no node
// finished, no resume. The newer attempt stays queued for its own delivery.
func TestResume_aDeliveryOutrunAtItsClaimWritesNothing(t *testing.T) {
	for _, tc := range cfCases() {
		t.Run(tc.name, func(t *testing.T) {
			base := tmpStore(t)
			const runID = "run-cf-outrun"
			published := tc.setup(t, base, runID)
			since := cfEventCount(t, base, runID)
			s := &cfNewerAttemptAtTheClaim{RunStore: base, t: t, runID: runID}
			claimed := false
			err := tc.engine(s, WithQueuedAttempt(published), WithOnResumeClaimed(func() { claimed = true })).Resume(context.Background(), runID, tc.answers)
			if s.newer.IsZero() {
				t.Fatalf("the delivery never claimed its run (err=%v)", err)
			}
			if !errors.Is(err, ErrResumeSuperseded) || claimed {
				t.Fatalf("a delivery outrun at its claim: err=%v claimed=%v, want ErrResumeSuperseded and no claim", err, claimed)
			}
			if writes := cfWritesSince(t, base, runID, since); len(writes) > 0 {
				t.Fatalf("a delivery outrun at its claim wrote on the newer attempt's run: %v", writes)
			}
			doc, lerr := base.LoadRun(context.Background(), runID)
			if lerr != nil {
				t.Fatal(lerr)
			}
			if doc.Status != store.RunStatusQueued || doc.QueuedAt == nil || !doc.QueuedAt.Equal(s.newer) {
				t.Fatalf("the newer attempt: status %s queued_at %v, want queued at %v", doc.Status, doc.QueuedAt, s.newer)
			}
		})
	}
}

// TestResume_aDeliveryRefusedBeforeItsClaimLeavesTheRunQueued: every refusal
// a pause path makes before its writes is read before the claim, so a refused
// delivery leaves its run queued, untouched — the runner then puts it back
// where it came from with the refusal (releaseRefusedResume) — and nothing of
// its answers is recorded, an await gate's answered question included.
func TestResume_aDeliveryRefusedBeforeItsClaimLeavesTheRunQueued(t *testing.T) {
	for _, tc := range []struct {
		name    string
		setup   func(t *testing.T, s store.RunStore, runID string)
		engine  func(s store.RunStore, opts ...EngineOption) *Engine
		answers map[string]any
		refusal string
	}{
		{
			name:  "the gate's node is gone from the workflow",
			setup: cfPausedAtGate,
			engine: func(s store.RunStore, opts ...EngineOption) *Engine {
				wf := &ir.Workflow{Name: "superseded_gate", Entry: "done", Nodes: map[string]ir.Node{
					"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
				}}
				return New(wf, s, newStubExecutor(), opts...)
			},
			answers: map[string]any{"decision": "go"},
			refusal: string(ErrCodeNodeNotFound),
		},
		{
			name:  "an await question is unanswered",
			setup: cfAwaitPause,
			engine: func(s store.RunStore, opts ...EngineOption) *Engine {
				return New(cfAwaitWorkflow(), s, newStubExecutor(), opts...)
			},
			answers: map[string]any{"q1": "blue"},
			refusal: "still unanswered: q2",
		},
		{
			name: "the pause's interaction is gone",
			setup: func(t *testing.T, s store.RunStore, runID string) {
				cfPausedAtGate(t, s, runID)
				if err := s.PauseRun(context.Background(), runID, &store.Checkpoint{NodeID: "gate", InteractionID: "gone"}); err != nil {
					t.Fatal(err)
				}
			},
			engine:  cfGateEngine,
			answers: map[string]any{"decision": "go"},
			refusal: "load interaction for resume",
		},
		{
			name: "a parallel gate's interaction is gone",
			setup: func(t *testing.T, s store.RunStore, runID string) {
				ctx := context.Background()
				wf, exec := parallelGateWorkflow()
				if err := New(wf, s, exec).Run(ctx, runID, nil); !errors.Is(err, ErrRunPaused) {
					t.Fatalf("Run: want ErrRunPaused, got %v", err)
				}
				r, err := s.LoadRun(ctx, runID)
				if err != nil {
					t.Fatal(err)
				}
				cp := *r.Checkpoint
				cp.InteractionID, cp.InteractionQuestions = "gone", nil
				if err := s.PauseRun(ctx, runID, &cp); err != nil {
					t.Fatal(err)
				}
			},
			engine: func(s store.RunStore, opts ...EngineOption) *Engine {
				wf, exec := parallelGateWorkflow()
				return New(wf, s, exec, opts...)
			},
			answers: map[string]any{"decision": "go"},
			refusal: "load interaction for resume",
		},
		{
			name: "a recovery pause's interaction is gone",
			setup: func(t *testing.T, s store.RunStore, runID string) {
				ctx := context.Background()
				exec := &flakyExecutor{target: "agent_a", failErr: &RuntimeError{Code: ErrCodeAuthFailed, Message: "authentication failed"}, failures: 1}
				if err := New(newRecoveryGateWorkflow(), s, exec, WithRecoveryDispatch(pauseForHumanOn(ErrCodeAuthFailed))).Run(ctx, runID, nil); !errors.Is(err, ErrRunPaused) {
					t.Fatalf("Run: want ErrRunPaused, got %v", err)
				}
				r, err := s.LoadRun(ctx, runID)
				if err != nil || r.Checkpoint == nil || !r.Checkpoint.RecoveryPause {
					t.Fatalf("precondition: a recovery pause, got %v (%v)", r, err)
				}
				cp := *r.Checkpoint
				cp.InteractionID, cp.InteractionQuestions = "gone", nil
				if err := s.PauseRun(ctx, runID, &cp); err != nil {
					t.Fatal(err)
				}
			},
			engine: func(s store.RunStore, opts ...EngineOption) *Engine {
				exec := &flakyExecutor{target: "agent_a"}
				return New(newRecoveryGateWorkflow(), s, exec, append([]EngineOption{WithRecoveryDispatch(pauseForHumanOn(ErrCodeAuthFailed))}, opts...)...)
			},
			answers: map[string]any{"acknowledge_recovery": "retry"},
			refusal: "load interaction for resume",
		},
		{
			name: "the parallel checkpoint lost the branch it waits on",
			setup: func(t *testing.T, s store.RunStore, runID string) {
				ctx := context.Background()
				wf, exec := parallelGateWorkflow()
				if err := New(wf, s, exec).Run(ctx, runID, nil); !errors.Is(err, ErrRunPaused) {
					t.Fatalf("Run: want ErrRunPaused, got %v", err)
				}
				r, err := s.LoadRun(ctx, runID)
				if err != nil {
					t.Fatal(err)
				}
				cp := *r.Checkpoint
				par := *cp.Parallel
				par.Branches = nil
				cp.Parallel = &par
				if err := s.PauseRun(ctx, runID, &cp); err != nil {
					t.Fatal(err)
				}
			},
			engine: func(s store.RunStore, opts ...EngineOption) *Engine {
				wf, exec := parallelGateWorkflow()
				return New(wf, s, exec, opts...)
			},
			answers: map[string]any{"decision": "go"},
			refusal: "is missing",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := tmpStore(t)
			ctx := context.Background()
			const runID = "run-cf-refused"
			tc.setup(t, s, runID)
			published := queueAttempt(t, s, runID, store.RunStatusPausedWaitingHuman)
			queued, err := s.LoadRun(ctx, runID)
			if err != nil {
				t.Fatal(err)
			}
			since := cfEventCount(t, s, runID)
			claimed := false
			err = tc.engine(s, WithQueuedAttempt(published), WithOnResumeClaimed(func() { claimed = true })).Resume(ctx, runID, tc.answers)
			if err == nil || !strings.Contains(err.Error(), tc.refusal) || claimed {
				t.Fatalf("resume = %v (claimed=%v), want the refusal %q before any claim", err, claimed, tc.refusal)
			}
			doc, lerr := s.LoadRun(ctx, runID)
			if lerr != nil {
				t.Fatal(lerr)
			}
			if doc.Status != store.RunStatusQueued || doc.QueuedAt == nil || !doc.QueuedAt.Equal(*queued.QueuedAt) {
				t.Fatalf("a refused delivery left its run %s (queued_at %v), want it queued for its attempt (%v), as the runner's release expects", doc.Status, doc.QueuedAt, queued.QueuedAt)
			}
			if writes := cfWritesSince(t, s, runID, since); len(writes) > 0 {
				t.Fatalf("a delivery refused before its claim wrote: %v", writes)
			}
			if q1, err := s.LoadInteraction(ctx, runID, "q1"); err == nil && q1.AnsweredAt != nil {
				t.Fatalf("a refused await resume recorded q1's answer: %v", q1.Answers)
			}
		})
	}
}

// cfMoveAwayAtTheClaim moves the run off its queued attempt right before the
// delivery's claim, to `to` — an operator's cancel, a release to the pause.
type cfMoveAwayAtTheClaim struct {
	store.RunStore
	runID string
	to    store.RunStatus
	once  sync.Once
	moved bool
}

func (s *cfMoveAwayAtTheClaim) Unwrap() store.RunStore { return s.RunStore }

func (s *cfMoveAwayAtTheClaim) ClaimQueuedRunIfAttempt(ctx context.Context, id string, publishedAt time.Time) (bool, error) {
	if id == s.runID {
		s.once.Do(func() {
			s.moved, _ = s.RunStore.UpdateRunStatusIf(ctx, id, s.to, "", []store.RunStatus{store.RunStatusQueued})
		})
	}
	return store.AsQueuedAttemptClaimer(s.RunStore).ClaimQueuedRunIfAttempt(ctx, id, publishedAt)
}

// TestResume_aRunReadQueuedIsClaimedForItsAttemptOnly: a run the delivery
// read queued and that moved before its claim — cancelled by an operator,
// put back to its pause — is not claimed from where it went: the failure
// path and the pause path both leave it there.
func TestResume_aRunReadQueuedIsClaimedForItsAttemptOnly(t *testing.T) {
	for _, tc := range []struct {
		name    string
		to      store.RunStatus
		setup   func(t *testing.T, s store.RunStore, runID string) time.Time
		answers map[string]any
	}{
		{"a failure resume, cancelled", store.RunStatusCancelled, func(t *testing.T, s store.RunStore, runID string) time.Time {
			parkedAtGate(t, s, runID, false)
			return queueAttempt(t, s, runID, store.RunStatusFailedResumable)
		}, nil},
		{"a failure resume, put back to failed_resumable", store.RunStatusFailedResumable, func(t *testing.T, s store.RunStore, runID string) time.Time {
			parkedAtGate(t, s, runID, false)
			return queueAttempt(t, s, runID, store.RunStatusFailedResumable)
		}, nil},
		{"a pause, cancelled", store.RunStatusCancelled, func(t *testing.T, s store.RunStore, runID string) time.Time {
			cfPausedAtGate(t, s, runID)
			return queueAttempt(t, s, runID, store.RunStatusPausedWaitingHuman)
		}, map[string]any{"decision": "go"}},
		{"a pause, put back to its pause", store.RunStatusPausedWaitingHuman, func(t *testing.T, s store.RunStore, runID string) time.Time {
			cfPausedAtGate(t, s, runID)
			return queueAttempt(t, s, runID, store.RunStatusPausedWaitingHuman)
		}, map[string]any{"decision": "go"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := tmpStore(t)
			const runID = "run-cf-moved"
			published := tc.setup(t, base, runID)
			s := &cfMoveAwayAtTheClaim{RunStore: base, runID: runID, to: tc.to}
			claimed := false
			err := cfGateEngine(s, WithQueuedAttempt(published), WithOnResumeClaimed(func() { claimed = true })).Resume(context.Background(), runID, tc.answers)
			if !s.moved {
				t.Fatalf("the run was never moved before the claim (err=%v)", err)
			}
			doc, lerr := base.LoadRun(context.Background(), runID)
			if lerr != nil {
				t.Fatal(lerr)
			}
			if err == nil || claimed || doc.Status != tc.to {
				t.Fatalf("a run read queued, then %s before the claim: err=%v claimed=%v status=%s — want it left %s, unclaimed", tc.to, err, claimed, doc.Status, tc.to)
			}
		})
	}
}

// cfCancelAtTheStatusClaim cancels the run right before the delivery's claim
// by status — a delivery whose publication carries no usable time claims by
// status alone.
type cfCancelAtTheStatusClaim struct {
	store.RunStore
	runID string
	once  sync.Once
	moved bool
}

func (s *cfCancelAtTheStatusClaim) Unwrap() store.RunStore { return s.RunStore }

func (s *cfCancelAtTheStatusClaim) UpdateRunStatusIf(ctx context.Context, id string, status store.RunStatus, runErr string, from []store.RunStatus) (bool, error) {
	if status == store.RunStatusRunning && id == s.runID {
		s.once.Do(func() {
			s.moved, _ = s.RunStore.UpdateRunStatusIf(ctx, id, store.RunStatusCancelled, "operator", []store.RunStatus{store.RunStatusQueued, store.RunStatusPausedWaitingHuman})
		})
	}
	return s.RunStore.UpdateRunStatusIf(ctx, id, status, runErr, from)
}

// TestResume_aQueuedRunIsClaimedBeforeItsPauseIsWrittenWithoutAPublication:
// a delivery whose publication time is unknown (a legacy message) still
// claims a queued run before the pause path writes: cancelled right before
// that claim, the run keeps nothing of it.
func TestResume_aQueuedRunIsClaimedBeforeItsPauseIsWrittenWithoutAPublication(t *testing.T) {
	base := tmpStore(t)
	ctx := context.Background()
	const runID = "run-cf-legacy"
	cfPausedAtGate(t, base, runID)
	_ = queueAttempt(t, base, runID, store.RunStatusPausedWaitingHuman)
	since := cfEventCount(t, base, runID)
	s := &cfCancelAtTheStatusClaim{RunStore: base, runID: runID}
	claimed := false
	err := cfGateEngine(s, WithOnResumeClaimed(func() { claimed = true })).Resume(ctx, runID, map[string]any{"decision": "go"})
	if !s.moved {
		t.Fatalf("the run was never cancelled before the claim (err=%v)", err)
	}
	if err == nil || claimed {
		t.Fatalf("a run cancelled before its claim: err=%v claimed=%v, want a refusal", err, claimed)
	}
	if writes := cfWritesSince(t, base, runID, since); len(writes) > 0 {
		t.Fatalf("a resume refused at its claim wrote on the cancelled run: %v", writes)
	}
}

// TestResume_aNewerAttemptReAsksTheGateItsOutrunDeliveryAnswered: the
// operator cancels a resume whose delivery is about to claim, then resumes
// the run without answers. The outrun delivery recorded nothing, so the newer
// attempt's own delivery asks the gate again instead of crossing it on the
// answer the cancelled resume carried.
func TestResume_aNewerAttemptReAsksTheGateItsOutrunDeliveryAnswered(t *testing.T) {
	base := tmpStore(t)
	ctx := context.Background()
	const runID = "run-cf-reask"
	cfPausedAtGate(t, base, runID)
	published := queueAttempt(t, base, runID, store.RunStatusPausedWaitingHuman)
	s := &cfNewerAttemptAtTheClaim{RunStore: base, t: t, runID: runID}
	if err := cfGateEngine(s, WithQueuedAttempt(published)).Resume(ctx, runID, map[string]any{"decision": "stale"}); !errors.Is(err, ErrResumeSuperseded) {
		t.Fatalf("the outrun delivery: %v, want ErrResumeSuperseded", err)
	}
	err := cfGateEngine(base, WithQueuedAttempt(s.newer.Add(time.Millisecond))).Resume(ctx, runID, nil)
	if a, aerr := base.LoadLatestArtifact(ctx, runID, "gate"); aerr == nil && a != nil && a.Data["decision"] == "stale" {
		t.Fatalf("the newer attempt crossed the gate on the outrun delivery's answer: %v (resume: %v)", a.Data, err)
	}
	if !errors.Is(err, ErrRunPaused) {
		t.Fatalf("the newer attempt's delivery without answers: %v, want the gate asked again (ErrRunPaused)", err)
	}
}

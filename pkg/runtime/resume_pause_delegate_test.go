package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
	"testing"
)

var errStoppedAfterFinish = errors.New("stop after committed handoff retirement")

type committedFinishStore struct{ handoffAnswerFaultStore }

func (s committedFinishStore) FinishResumePauseHandoff(ctx context.Context, id, h string, cp *store.Checkpoint) (bool, error) {
	ok, err := store.AsResumePauseHandoffStore(s.RunStore).FinishResumePauseHandoff(ctx, id, h, cp)
	if err != nil || !ok {
		return ok, err
	}
	return false, errStoppedAfterFinish
}
func TestDelegateHandoffCommitsSuccessorBeforeRetirement(t *testing.T) {
	ctx := t.Context()
	base := tmpStore(t)
	const id = "review-delegate-handoff"
	wf := interactionWorkflow(ir.InteractionHuman)
	exec := newStubExecutor()
	calls := 0
	var resumedInput map[string]any
	exec.on("worker", func(in map[string]any) (map[string]any, error) {
		calls++
		if calls == 1 {
			return nil, &model.ErrNeedsInteraction{NodeID: "worker", Questions: map[string]any{delegate.AskUserQuestionKey: "Which environment?"}, Backend: "claw", Conversation: json.RawMessage(`[{"role":"user","content":"hello"}]`), PendingToolUseID: "toolu_42"}
		}
		resumedInput = in
		return map[string]any{"text": "ok", "_tokens": 10}, nil
	})
	if err := New(wf, base, exec).Run(ctx, id, nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("run: %v", err)
	}
	wrapped := committedFinishStore{handoffAnswerFaultStore{codemodeStateWrappedStore{base}, "finish"}}
	if err := New(wf, wrapped, exec, WithResumeCodemodeQuota(store.CodemodeScratchLimits{Entries: 7})).Resume(ctx, id, map[string]any{delegate.AskUserQuestionKey: "staging"}); !errors.Is(err, errStoppedAfterFinish) {
		t.Fatalf("finish fault: %v", err)
	}
	r, err := base.LoadRun(ctx, id)
	if err != nil || r.Status != store.RunStatusRunning || r.ResumePauseHandoff != nil || calls != 2 {
		t.Fatalf("retirement state: %v, %+v, calls=%d", err, r, calls)
	}
	if ok, err := base.UpdateRunStatusIf(ctx, id, store.RunStatusFailedResumable, "orphan", []store.RunStatus{store.RunStatusRunning}); err != nil || !ok {
		t.Fatalf("park orphan: %v, %v", ok, err)
	}
	if err := New(wf, base, exec).Resume(ctx, id, nil); err != nil {
		t.Fatalf("orphan resume: %v", err)
	}
	if calls != 2 || resumedInput[delegate.ResumePendingToolUseIDKey] != "toolu_42" || resumedInput[delegate.ResumeAnswerKey] != "staging" {
		t.Fatalf("orphan lost committed delegated continuation: calls=%d pendingToolUse=%v answer=%v", calls, resumedInput[delegate.ResumePendingToolUseIDKey], resumedInput[delegate.ResumeAnswerKey])
	}
}

func TestDelegateHandoffPreservesContinuationAfterExecutorFailure(t *testing.T) {
	ctx := t.Context()
	base := tmpStore(t)
	const id = "review-delegate-fault"
	wf := interactionWorkflow(ir.InteractionHuman)
	exec := newStubExecutor()
	calls := 0
	var resumedInput map[string]any
	exec.on("worker", func(in map[string]any) (map[string]any, error) {
		calls++
		if calls == 1 {
			return nil, &model.ErrNeedsInteraction{NodeID: "worker", Questions: map[string]any{delegate.AskUserQuestionKey: "Which environment?"}, Backend: "claw", Conversation: json.RawMessage(`[{"role":"user","content":"hello"}]`), PendingToolUseID: "toolu_42"}
		}
		if calls == 2 {
			return nil, errors.New("deterministic transient delegate error")
		}
		resumedInput = in
		return map[string]any{"text": "ok", "_tokens": 10}, nil
	})
	if err := New(wf, base, exec).Run(ctx, id, nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("run: %v", err)
	}
	if err := New(wf, base, exec, WithResumeCodemodeQuota(store.CodemodeScratchLimits{Entries: 7})).Resume(ctx, id, map[string]any{delegate.AskUserQuestionKey: "staging"}); err == nil {
		t.Fatal("delegate fault not propagated")
	}
	r, err := base.LoadRun(ctx, id)
	if err != nil || r.Status != store.RunStatusFailedResumable {
		t.Fatalf("failure state: %v, %+v, calls=%d", err, r, calls)
	}
	if err := New(wf, base, exec).Resume(ctx, id, nil); err != nil {
		t.Fatalf("fault resume: %v", err)
	}
	if calls != 3 || resumedInput[delegate.ResumePendingToolUseIDKey] != "toolu_42" || resumedInput[delegate.ResumeAnswerKey] != "staging" {
		t.Fatalf("re-entry lost committed delegated continuation: calls=%d pendingToolUse=%v answer=%v", calls, resumedInput[delegate.ResumePendingToolUseIDKey], resumedInput[delegate.ResumeAnswerKey])
	}
}

var errReviewSecondPause = errors.New("review: injected second PauseRun failure")

type reviewSecondPauseFault struct{ handoffAnswerFaultStore }

func (s reviewSecondPauseFault) ReplaceResumePauseHandoff(context.Context, string, string, *store.Checkpoint) (bool, error) {
	return false, errReviewSecondPause
}

func TestDelegateHandoffSecondDelegatePauseFailureKeepsFirstAnswer(t *testing.T) {
	ctx := t.Context()
	base := tmpStore(t)
	const id = "review-second-pause"
	wf := interactionWorkflow(ir.InteractionHuman)
	stub := newStubExecutor()
	calls := 0
	stub.on("worker", func(in map[string]any) (map[string]any, error) {
		calls++
		switch calls {
		case 1:
			return nil, &model.ErrNeedsInteraction{NodeID: "worker", Backend: "claw", Conversation: json.RawMessage(`[{"role":"user","content":"first"}]`), PendingToolUseID: "first", Questions: map[string]any{delegate.AskUserQuestionKey: "First question"}}
		case 2:
			return nil, &model.ErrNeedsInteraction{NodeID: "worker", Backend: "claw", Conversation: json.RawMessage(`[{"role":"user","content":"second"}]`), PendingToolUseID: "second", Questions: map[string]any{delegate.AskUserQuestionKey: "Second question"}}
		default:
			if in[delegate.ResumePendingToolUseIDKey] != "first" || in[delegate.ResumeAnswerKey] != "first answer" {
				t.Fatalf("wrong replay: %#v", in)
			}
			return map[string]any{"text": "ok"}, nil
		}
	})
	if err := New(wf, base, stub).Run(ctx, id, nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("initial pause: %v", err)
	}
	fault := reviewSecondPauseFault{handoffAnswerFaultStore{codemodeStateWrappedStore{base}, "finish"}}
	if err := New(wf, fault, stub, WithResumeCodemodeQuota(store.CodemodeScratchLimits{Entries: 7})).Resume(ctx, id, map[string]any{delegate.AskUserQuestionKey: "first answer"}); !errors.Is(err, errReviewSecondPause) {
		t.Fatalf("expected PauseRun failure: %v", err)
	}
	r, err := base.LoadRun(ctx, id)
	if err != nil || r.Status != store.RunStatusFailedResumable || r.ResumePauseHandoff == nil || r.Checkpoint.BackendPendingToolUseID != "first" {
		t.Fatalf("expected old handoff parked: %+v %v", r, err)
	}
	original, err := base.LoadInteraction(ctx, id, r.ResumePauseHandoff.InteractionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := New(wf, base, stub).Resume(ctx, id, nil); err != nil {
		t.Fatalf("reclaim with original recorded answer: %v; record question=%v answer=%v", err, original.Questions[delegate.AskUserQuestionKey], original.Answers[delegate.AskUserQuestionKey])
	}
	if calls != 3 {
		t.Fatalf("calls=%d want 3", calls)
	}
}

func TestDelegateHandoffAwaitRecursionCannotCrossUnretiredHandoff(t *testing.T) {
	ctx := t.Context()
	base := tmpStore(t)
	const id = "review-await-recursion"
	wf := interactionWorkflow(ir.InteractionHuman)
	stub := newStubExecutor()
	calls := 0
	stub.on("worker", func(in map[string]any) (map[string]any, error) {
		calls++
		switch calls {
		case 1:
			return nil, &model.ErrNeedsInteraction{NodeID: "worker", Backend: "claw", Conversation: json.RawMessage(`[{"role":"user","content":"first"}]`), PendingToolUseID: "first", Questions: map[string]any{delegate.AskUserQuestionKey: "First question"}}
		case 2:
			return nil, &model.ErrNeedsInteraction{NodeID: "worker", Backend: "claw", Conversation: json.RawMessage(`[{"role":"user","content":"await"}]`), PendingToolUseID: "await", Questions: map[string]any{delegate.AwaitPendingInteractionsKey: delegate.AwaitPendingToQuestions([]delegate.PendingAsync{{InteractionID: "already-resolved"}})}}
		default:
			if in[delegate.ResumePendingToolUseIDKey] != "await" {
				t.Fatalf("did not exercise recursive await: %v", in[delegate.ResumePendingToolUseIDKey])
			}
			return map[string]any{"text": "ok"}, nil
		}
	})
	if err := New(wf, base, stub).Run(ctx, id, nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("initial pause: %v", err)
	}
	fault := handoffAnswerFaultStore{codemodeStateWrappedStore{base}, "finish"}
	result := New(wf, fault, stub, WithResumeCodemodeQuota(store.CodemodeScratchLimits{Entries: 7})).Resume(ctx, id, map[string]any{delegate.AskUserQuestionKey: "first answer"})
	if calls != 3 {
		t.Fatalf("calls=%d, expected recursive await: %v", calls, result)
	}
	if !errors.Is(result, errHandoffAnswerWrite) {
		r, _ := base.LoadRun(ctx, id)
		t.Fatalf("required finish fault was bypassed: result=%v status=%s handoff=%t", result, r.Status, r.ResumePauseHandoff != nil)
	}
}

func TestDelegateHandoffDelegateHandoffBudgetPauseKeepsConversation(t *testing.T) {
	ctx := t.Context()
	base := tmpStore(t)
	const id = "review-budget-pause"
	wf := interactionWorkflow(ir.InteractionHuman)
	wf.Budget = &ir.Budget{MaxCostUSD: 1, OnExceeded: "pause"}
	stub := newStubExecutor()
	calls := 0
	stub.on("worker", func(in map[string]any) (map[string]any, error) {
		calls++
		switch calls {
		case 1:
			return nil, &model.ErrNeedsInteraction{NodeID: "worker", Backend: "claw", Conversation: json.RawMessage(`[{"role":"user","content":"first"}]`), PendingToolUseID: "first", Questions: map[string]any{delegate.AskUserQuestionKey: "First question"}}
		case 2:
			if in[delegate.ResumePendingToolUseIDKey] != "first" {
				t.Fatalf("initial continuation missing")
			}
			return map[string]any{"text": "ok", "_cost_usd": 2.0}, nil
		default:
			if in[delegate.ResumePendingToolUseIDKey] != "first" || in[delegate.ResumeAnswerKey] != "first answer" {
				t.Fatalf("reclaim restarted worker without conversation and answer: %v %v", in[delegate.ResumePendingToolUseIDKey], in[delegate.ResumeAnswerKey])
			}
			return map[string]any{"text": "ok"}, nil
		}
	})
	if err := New(wf, base, stub).Run(ctx, id, nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("initial pause: %v", err)
	}
	result := New(wf, base, stub, WithResumeCodemodeQuota(store.CodemodeScratchLimits{Entries: 7})).Resume(ctx, id, map[string]any{delegate.AskUserQuestionKey: "first answer"})
	if !errors.Is(result, ErrRunPausedOperator) {
		t.Fatalf("expected budget pause: %v", result)
	}
	if err := base.PatchRunSteering(ctx, id, nil, &store.RunBudgetRaises{MaxCostUSD: 10.0}); err != nil {
		t.Fatal(err)
	}
	if err := New(wf, base, stub).Resume(ctx, id, nil); err != nil {
		t.Fatalf("resume with raised cap: %v", err)
	}
	if calls != 2 {
		t.Fatalf("calls=%d want 2; valid overbudget result should be checkpointed at successor", calls)
	}
}

func TestDelegateHandoffDrainWhileDelegateContinues(t *testing.T) {
	ctx := t.Context()
	drainCtx, drain := context.WithCancelCause(ctx)
	defer drain(nil)
	base := tmpStore(t)
	const id = "review-delegate-drain"
	wf := interactionWorkflow(ir.InteractionHuman)
	stub := newStubExecutor()
	calls := 0
	stub.on("worker", func(in map[string]any) (map[string]any, error) {
		calls++
		switch calls {
		case 1:
			return nil, &model.ErrNeedsInteraction{NodeID: "worker", Backend: "claw", Conversation: json.RawMessage(`[{"role":"user","content":"first"}]`), PendingToolUseID: "first", Questions: map[string]any{delegate.AskUserQuestionKey: "First question"}}
		case 2:
			drain(ErrRunInterrupted)
			return nil, drainCtx.Err()
		default:
			if in[delegate.ResumePendingToolUseIDKey] != "first" || in[delegate.ResumeAnswerKey] != "first answer" {
				t.Fatalf("lost envelope: %v %v", in[delegate.ResumePendingToolUseIDKey], in[delegate.ResumeAnswerKey])
			}
			return map[string]any{"text": "ok"}, nil
		}
	})
	if err := New(wf, base, stub).Run(ctx, id, nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("initial pause: %v", err)
	}
	result := New(wf, base, stub, WithResumeCodemodeQuota(store.CodemodeScratchLimits{Entries: 7})).Resume(drainCtx, id, map[string]any{delegate.AskUserQuestionKey: "first answer"})
	r, err := base.LoadRun(ctx, id)
	if err != nil || r.Status != store.RunStatusFailedResumable || r.ResumePauseHandoff == nil {
		t.Fatalf("park state: status=%s handoff=%t load=%v", r.Status, r.ResumePauseHandoff != nil, err)
	}
	if err := New(wf, base, stub).Resume(ctx, id, nil); err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if !errors.Is(result, ErrRunInterrupted) {
		t.Fatalf("reentry survived, but interruption classification lost: result=%v code=%s", result, r.FailureCode)
	}
}

func TestDelegateHandoffReclaimChecksBudgetBeforeDispatch(t *testing.T) {
	ctx := t.Context()
	base := tmpStore(t)
	const id = "review-budget-preflight"
	wf := interactionWorkflow(ir.InteractionHuman)
	wf.Budget = &ir.Budget{MaxCostUSD: 1, OnExceeded: "pause"}
	stub := newStubExecutor()
	calls := 0
	stub.on("worker", func(in map[string]any) (map[string]any, error) {
		calls++
		switch calls {
		case 1:
			return nil, &model.ErrNeedsInteraction{NodeID: "worker", Backend: "claw", Conversation: json.RawMessage(`[{"role":"user","content":"first"}]`), PendingToolUseID: "first", Questions: map[string]any{delegate.AskUserQuestionKey: "First question"}}
		case 2:
			return map[string]any{"_cost_usd": 2.0}, errors.New("injected transient error after spend")
		default:
			return map[string]any{"text": "ok"}, nil
		}
	})
	if err := New(wf, base, stub).Run(ctx, id, nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("initial pause: %v", err)
	}
	if err := New(wf, base, stub, WithResumeCodemodeQuota(store.CodemodeScratchLimits{Entries: 7})).Resume(ctx, id, map[string]any{delegate.AskUserQuestionKey: "first answer"}); err == nil {
		t.Fatal("expected transient error")
	}
	parked, err := base.LoadRun(ctx, id)
	if err != nil || parked.Checkpoint.BudgetCostUSD != 2 || parked.ResumePauseHandoff == nil {
		t.Fatalf("setup did not store overspent handoff: %v", err)
	}
	result := New(wf, base, stub).Resume(ctx, id, nil)
	if calls != 2 {
		t.Fatalf("executor dispatched at current spend=%.0f cap=%.0f before budget refusal: calls=%d result=%v", parked.Checkpoint.BudgetCostUSD, wf.Budget.MaxCostUSD, calls, result)
	}
	if !errors.Is(result, ErrBudgetExceeded) {
		t.Fatalf("want budget refusal, got %v", result)
	}
	if err := base.PatchRunSteering(ctx, id, nil, &store.RunBudgetRaises{MaxCostUSD: 10}); err != nil {
		t.Fatal(err)
	}
	if err := New(wf, base, stub).Resume(ctx, id, nil); err != nil {
		t.Fatalf("retry with raised cap: %v", err)
	}
	if calls != 3 {
		t.Fatalf("worker calls after budget raise = %d, want 3", calls)
	}
}

func TestDelegateHandoffPreservesSessionBlobUntilCommit(t *testing.T) {
	ctx := t.Context()
	base := tmpStore(t)
	const id = "review-delegate-persist"
	wf := interactionWorkflow(ir.InteractionHuman)
	wf.Nodes["worker"].(*ir.AgentNode).Session = ir.SessionPersist
	stub := newStubExecutor()
	calls := 0
	stub.on("worker", func(in map[string]any) (map[string]any, error) {
		calls++
		if calls == 1 {
			return nil, &model.ErrNeedsInteraction{NodeID: "worker", Backend: "test-backend", SessionID: "session-old", SessionStateBlob: []byte("pause fixture"), Questions: map[string]any{delegate.AskUserQuestionKey: "First question"}}
		}
		if in[delegate.SessionIDKey] != "session-old" {
			t.Fatalf("old session ID not passed to worker on call %d: %v", calls, in[delegate.SessionIDKey])
		}
		return map[string]any{"text": "ok", delegate.BackendNameKey: "test-backend", delegate.SessionIDKey: "session-new", delegate.SessionStateBlobKey: []byte("successor fixture")}, nil
	})
	if err := New(wf, base, stub).Run(ctx, id, nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("initial pause: %v", err)
	}
	before, _ := base.LoadRun(ctx, id)
	old := before.Checkpoint.BackendSessionStateRef
	if old == "" {
		t.Fatal("missing pause ref")
	}
	fault := handoffAnswerFaultStore{codemodeStateWrappedStore{base}, "finish"}
	if err := New(wf, fault, stub, WithResumeCodemodeQuota(store.CodemodeScratchLimits{Entries: 7})).Resume(ctx, id, map[string]any{delegate.AskUserQuestionKey: "first answer"}); !errors.Is(err, errHandoffAnswerWrite) {
		t.Fatalf("expected finish refusal: %v", err)
	}
	parked, err := base.LoadRun(ctx, id)
	if err != nil || parked.Checkpoint.BackendSessionStateRef != old || parked.Checkpoint.NodeSessions["worker"].StateRef != "" {
		t.Fatalf("slot published before commit: %v", err)
	}
	blobs := store.AsBackendSessionStore(base)
	if _, err := blobs.GetBackendSession(ctx, id, old); err != nil {
		t.Fatalf("old pause blob removed early: %v", err)
	}
	if err := New(wf, base, stub).Resume(ctx, id, nil); err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	done, err := base.LoadRun(ctx, id)
	if err != nil || done.Status != store.RunStatusFinished || done.ResumePauseHandoff != nil || done.Checkpoint.BackendSessionStateRef != "" {
		t.Fatalf("terminal continuation: %s %v", done.Status, err)
	}
	current := done.Checkpoint.NodeSessions["worker"].StateRef
	if b, err := blobs.GetBackendSession(ctx, id, current); err != nil || string(b) != "successor fixture" {
		t.Fatalf("new slot blob unreadable: %v", err)
	}
	if _, err := blobs.GetBackendSession(ctx, id, old); err == nil {
		t.Fatal("old pause blob not retired")
	}
}

func TestDelegateHandoffFinishFailureCarriesBookedSpend(t *testing.T) {
	ctx := t.Context()
	base := tmpStore(t)
	const id = "review-finish-spend"
	wf := interactionWorkflow(ir.InteractionHuman)
	wf.Budget = &ir.Budget{MaxCostUSD: 1, OnExceeded: "pause"}
	exec := newStubExecutor()
	calls := 0
	exec.on("worker", func(in map[string]any) (map[string]any, error) {
		calls++
		if calls == 1 {
			return nil, &model.ErrNeedsInteraction{NodeID: "worker", Backend: "claw", Questions: map[string]any{delegate.AskUserQuestionKey: "question"}}
		}
		return map[string]any{"text": "ok", "_cost_usd": 2.0}, nil
	})
	if err := New(wf, base, exec).Run(ctx, id, nil); !errors.Is(err, ErrRunPaused) {
		t.Fatal(err)
	}
	fault := handoffAnswerFaultStore{codemodeStateWrappedStore{base}, "finish"}
	if err := New(wf, fault, exec, WithResumeCodemodeQuota(store.CodemodeScratchLimits{Entries: 7})).Resume(ctx, id, map[string]any{delegate.AskUserQuestionKey: "answer"}); !errors.Is(err, errHandoffAnswerWrite) {
		t.Fatal(err)
	}
	parked, err := base.LoadRun(ctx, id)
	if err != nil || parked.ResumePauseHandoff == nil || parked.Status != store.RunStatusFailedResumable {
		t.Fatalf("expected original parked handoff: run=%v err=%v", parked, err)
	}
	retry := New(wf, base, exec).Resume(ctx, id, nil)
	if parked.Checkpoint.BudgetCostUSD != 2 || calls != 2 || !errors.Is(retry, ErrBudgetExceeded) {
		t.Fatalf("confirmed $2 delegate success then failed successor commit: durable cost=%g calls=%d retry=%v; want $2, 2, budget refusal", parked.Checkpoint.BudgetCostUSD, calls, retry)
	}
}

func TestDelegateHandoffBooksOnlyMarginalSessionCost(t *testing.T) {
	ctx := t.Context()
	base := tmpStore(t)
	const id = "cumulative-finish-spend"
	wf := interactionWorkflow(ir.InteractionHuman)
	wf.Budget = &ir.Budget{MaxCostUSD: 4, OnExceeded: "pause"}
	exec := newStubExecutor()
	calls := 0
	exec.on("worker", func(in map[string]any) (map[string]any, error) {
		calls++
		if calls == 1 {
			return nil, &model.ErrNeedsInteraction{NodeID: "worker", Backend: "claw", Questions: map[string]any{delegate.AskUserQuestionKey: "question"}}
		}
		return map[string]any{"text": "ok", "_cost_usd": float64(calls), "_cost_is_session_total": true, "_backend": "claude_code", "_session_id": "same-session"}, nil
	})
	if err := New(wf, base, exec).Run(ctx, id, nil); !errors.Is(err, ErrRunPaused) {
		t.Fatal(err)
	}
	fault := handoffAnswerFaultStore{codemodeStateWrappedStore{base}, "finish"}
	if err := New(wf, fault, exec, WithResumeCodemodeQuota(store.CodemodeScratchLimits{Entries: 7})).Resume(ctx, id, map[string]any{delegate.AskUserQuestionKey: "answer"}); !errors.Is(err, errHandoffAnswerWrite) {
		t.Fatal(err)
	}
	parked, err := base.LoadRun(ctx, id)
	if err != nil || parked.ResumePauseHandoff == nil || parked.Status != store.RunStatusFailedResumable {
		t.Fatalf("expected original parked handoff: run=%v err=%v", parked, err)
	}
	if err := New(wf, base, exec).Resume(ctx, id, nil); err != nil {
		t.Fatalf("in-budget retry: %v", err)
	}
	finished, err := base.LoadRun(ctx, id)
	if err != nil || calls != 3 || finished.Checkpoint.BudgetCostUSD != 3 {
		t.Fatalf("want cumulative cost $3 after three calls: run=%v calls=%d err=%v", finished, calls, err)
	}
}

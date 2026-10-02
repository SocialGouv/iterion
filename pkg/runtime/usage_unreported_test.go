package runtime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/cost"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A failed node whose only spend is a call the provider never reported is a
// bill of unknown size, not nothing: the count reaches the run budget. It
// moved no token or cost counter, so it takes no max_iterations slot — a
// spendless failure never did. Red when the failed-spend guard reads tokens
// and cost alone (count 0), or when the count is booked like spend (1
// iteration).
func TestFailedNodeSpend_AnUnreportedCallIsNotedWithoutAnIteration(t *testing.T) {
	wf := branchLocalLoopWorkflow()
	wf.Budget = &ir.Budget{MaxTokens: 10_000}
	engine := New(wf, tmpStore(t), newStubExecutor())
	shared := newSharedBudget(wf.Budget, engine.logger)
	rs := &runState{budget: shared, loopBudgetMarks: make(map[string]loopBudgetMark)}

	engine.recordFailedNodeSpend(rs, "agent", map[string]any{"_tokens": 0, cost.UnreportedCallsKey: 1})
	if _, _, iters, _, _, _, unreported := shared.Snapshot(); unreported != 1 || iters != 0 {
		t.Fatalf("noted %d unreported call(s) over %d iteration(s), want 1 over 0", unreported, iters)
	}
}

// An attempt the engine retries in place books no spend — the retry may
// continue its session — but its unreported calls are requests of their own:
// the count reaches the checkpoint and raises the advisory once. Red when the
// retry exit drops the count, or notes it without emitting the warning.
func TestInPlaceRetry_KeepsTheFailedAttemptsUnreportedCalls(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "unreported_retry",
		Entry: "agent",
		Nodes: map[string]ir.Node{
			"agent": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "agent"}},
			"done":  &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges:   []*ir.Edge{{From: "agent", To: "done"}},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
		Budget:  &ir.Budget{MaxTokens: 1_000_000},
	}
	attempts := 0
	exec := newStubExecutor()
	exec.on("agent", func(_ map[string]any) (map[string]any, error) {
		attempts++
		if attempts == 1 {
			return map[string]any{"_tokens": 0, cost.UnreportedCallsKey: 1}, errors.New("stream cut before the provider reported its usage")
		}
		return map[string]any{"ok": true, "_tokens": 100}, nil
	})
	dispatch := RecoveryDispatch(func(_ context.Context, _ error, prior func(ErrorCode) int) (RecoveryAction, ErrorCode) {
		if prior(ErrCodeRateLimited) > 0 {
			return RecoveryAction{Kind: RecoveryFailTerminal}, ErrCodeRateLimited
		}
		return RecoveryAction{Kind: RecoveryRetrySameNode}, ErrCodeRateLimited
	})
	st := &checkpointCapturingStore{RunStore: tmpStore(t)}
	if err := New(wf, st, exec, WithRecoveryDispatch(dispatch)).Run(context.Background(), "run-unreported-retry", nil); err != nil {
		t.Fatalf("the retry was supposed to carry the run through: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("expected a failed attempt and its retry, got %d attempt(s)", attempts)
	}
	cp := st.lastCheckpoint()
	if cp == nil || cp.BudgetUnreportedCalls != 1 || cp.BudgetTokensUsed != 100 {
		t.Fatalf("checkpoint = %+v, want the retry's 100 tokens and the failed attempt's unreported call", cp)
	}
	events, err := st.LoadEvents(context.Background(), "run-unreported-retry")
	if err != nil {
		t.Fatalf("load events: %v", err)
	}
	warned := 0
	for _, evt := range events {
		if evt.Type == store.EventBudgetWarning && evt.Data["dimension"] == "usage_unreported" {
			warned++
		}
	}
	if warned != 1 {
		t.Errorf("usage_unreported raised %d time(s), want once", warned)
	}
}

// The branch twin of the rule above: a failed branch node whose only spend is
// an unreported call is noted, takes no max_iterations slot, and still arms
// the branch's checkpoint flush — the count moved the run budget in memory.
// Red when the count is booked like spend, or the flush is left unarmed.
func TestFailedBranchSpend_AnUnreportedCallIsNotedWithoutAnIteration(t *testing.T) {
	wf := branchLocalLoopWorkflow()
	wf.Budget = &ir.Budget{MaxTokens: 10_000}
	engine := New(wf, tmpStore(t), newStubExecutor())
	shared := newSharedBudget(wf.Budget, engine.logger)
	rs := &runState{budget: shared, loopBudgetMarks: make(map[string]loopBudgetMark)}
	var branchCost float64
	result := &branchResult{}

	engine.recordFailedBranchSpend(context.Background(), rs, "run", "b1", "run/b1", "agent", map[string]any{cost.UnreportedCallsKey: 1}, &branchCost, result)
	if _, _, iters, _, _, _, unreported := shared.Snapshot(); unreported != 1 || iters != 0 {
		t.Errorf("noted %d unreported call(s) over %d iteration(s), want 1 over 0", unreported, iters)
	}
	if !result.spendUncheckpointed {
		t.Error("the count moved the run budget, but the branch's flush is not armed")
	}
}

// The success twin: a successful branch node whose only spend is an
// unreported call also arms the flush — the count moved the run budget in
// memory, and this branch writes no checkpoint of its own.
// Red when the dirty flag reads the tokens and cost alone.
func TestBranchSpend_AMarkerOnlySuccessArmsTheFlush(t *testing.T) {
	wf := branchLocalLoopWorkflow()
	wf.Budget = &ir.Budget{MaxTokens: 10_000}
	engine := New(wf, tmpStore(t), newStubExecutor())
	shared := newSharedBudget(wf.Budget, engine.logger)
	rs := &runState{budget: shared, loopBudgetMarks: make(map[string]loopBudgetMark)}
	var branchCost float64
	result := &branchResult{}

	engine.recordBranchSpend(context.Background(), rs, "run", "b1", "run/b1", "agent", map[string]any{cost.UnreportedCallsKey: 1}, &branchCost, result)
	if _, _, _, _, _, _, unreported := shared.Snapshot(); unreported != 1 {
		t.Errorf("noted %d unreported call(s), want 1", unreported)
	}
	if !result.spendUncheckpointed {
		t.Error("the count moved the run budget, but the branch's flush is not armed")
	}
}

// The note must speak when nothing else books: a retried attempt whose
// retry fails for good leaves no later booking to raise the advisory, so the
// retry exit itself is the last frame that can. Red when noteUnreported
// counts without raising.
func TestInPlaceRetry_TheNoteSpeaksWhenNothingElseBooks(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "unreported_retry_fail",
		Entry: "agent",
		Nodes: map[string]ir.Node{
			"agent": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "agent"}},
			"done":  &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges:   []*ir.Edge{{From: "agent", To: "done"}},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
		Budget:  &ir.Budget{MaxTokens: 1_000_000},
	}
	attempts := 0
	exec := newStubExecutor()
	exec.on("agent", func(_ map[string]any) (map[string]any, error) {
		attempts++
		if attempts == 1 {
			return map[string]any{cost.UnreportedCallsKey: 1}, errors.New("stream cut before the provider reported its usage")
		}
		return map[string]any{}, errors.New("still down")
	})
	dispatch := RecoveryDispatch(func(_ context.Context, _ error, prior func(ErrorCode) int) (RecoveryAction, ErrorCode) {
		if prior(ErrCodeRateLimited) > 0 {
			return RecoveryAction{Kind: RecoveryFailTerminal}, ErrCodeRateLimited
		}
		return RecoveryAction{Kind: RecoveryRetrySameNode}, ErrCodeRateLimited
	})
	st := &checkpointCapturingStore{RunStore: tmpStore(t)}
	if err := New(wf, st, exec, WithRecoveryDispatch(dispatch)).Run(context.Background(), "run-unreported-retry-fail", nil); err == nil {
		t.Fatal("the second failure was supposed to end the run")
	}
	cp := st.lastCheckpoint()
	if cp == nil || cp.BudgetUnreportedCalls != 1 || cp.BudgetTokensUsed != 0 {
		t.Fatalf("checkpoint = %+v, want the failed attempt's unreported call and no tokens", cp)
	}
	events, err := st.LoadEvents(context.Background(), "run-unreported-retry-fail")
	if err != nil {
		t.Fatalf("load events: %v", err)
	}
	warned := 0
	for _, evt := range events {
		if evt.Type == store.EventBudgetWarning && evt.Data["dimension"] == "usage_unreported" {
			warned++
		}
	}
	if warned != 1 {
		t.Errorf("usage_unreported raised %d time(s), want once, from the retry exit's note", warned)
	}
}

// A fan-out branch that fails with nothing but an unreported call: its count
// reaches the run, the checkpoint carries it, and a restart restores it.
// Red when the branch's failed-spend guard, its checkpoint flush, the
// checkpoint field or the resume restore drops it.
func TestFailedBranchSpend_AnUnreportedCallSurvivesARestart(t *testing.T) {
	wf := budgetFanOutWorkflow(&ir.Budget{MaxTokens: 1_000_000, MaxParallelBranches: 2})
	st := &checkpointCapturingStore{RunStore: tmpStore(t)}
	// The failing branch books only after its sibling checkpointed: its own
	// flush is then the only write that can carry what it booked.
	siblingBooked := make(chan struct{})
	var once sync.Once
	st.onSave = func(cp *store.Checkpoint) {
		if cp != nil && cp.BudgetTokensUsed == 1_000 {
			once.Do(func() { close(siblingBooked) })
		}
	}
	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]any) (map[string]any, error) { return map[string]any{"ok": true}, nil })
	exec.on("b", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"ok": true, "_tokens": 1_000}, nil
	})
	exec.on("a", func(_ map[string]any) (map[string]any, error) {
		select {
		case <-siblingBooked:
		case <-time.After(30 * time.Second):
			t.Error("the sibling never checkpointed its own spend; the ordering this test rests on is gone")
		}
		return map[string]any{"_tokens": 0, cost.UnreportedCallsKey: 1}, errors.New("stream cut before the provider reported its usage")
	})
	if err := New(wf, st, exec).Run(context.Background(), "run-branch-unreported", nil); err != nil {
		t.Fatalf("the best-effort join was supposed to carry the run: %v", err)
	}
	cp := st.lastCheckpoint()
	if cp == nil || cp.BudgetUnreportedCalls != 1 {
		t.Fatalf("checkpoint = %+v, want one unreported call carried", cp)
	}

	resumed := &runState{budget: newSharedBudget(wf.Budget, nil), loopBudgetMarks: make(map[string]loopBudgetMark)}
	restoreBudgetAccounting(resumed, cp)
	if _, _, _, _, _, _, unreported := resumed.budget.Snapshot(); unreported != 1 {
		t.Errorf("the restart restored %d unreported call(s), want 1", unreported)
	}
}

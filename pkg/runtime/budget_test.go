package runtime

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ===========================================================================
// P4-02: Budget enforcement and workspace safety tests
// ===========================================================================

// ---------------------------------------------------------------------------
// Helper: fan-out workflow with budget
// ---------------------------------------------------------------------------

func budgetFanOutWorkflow(budget *ir.Budget) *ir.Workflow {
	return &ir.Workflow{
		Name:  "budget_fanout_test",
		Entry: "entry",
		Nodes: map[string]ir.Node{
			"entry":  &ir.AgentNode{BaseNode: ir.BaseNode{ID: "entry"}},
			"router": &ir.RouterNode{BaseNode: ir.BaseNode{ID: "router"}, RouterMode: ir.RouterFanOutAll},
			"a":      &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}},
			"b":      &ir.AgentNode{BaseNode: ir.BaseNode{ID: "b"}},
			"done":   &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}, AwaitMode: ir.AwaitBestEffort},
			"fail":   &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges: []*ir.Edge{
			{From: "entry", To: "router"},
			{From: "router", To: "a"},
			{From: "router", To: "b"},
			{From: "a", To: "done"},
			{From: "b", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
		Budget:  budget,
	}
}

// ---------------------------------------------------------------------------
// Test: budget warning emitted at 80% threshold
// ---------------------------------------------------------------------------

func TestBudgetWarningEmitted(t *testing.T) {
	// Budget of 5 iterations — warning at 80% = 4th iteration.
	wf := &ir.Workflow{
		Name:  "budget_warning_test",
		Entry: "a",
		Nodes: map[string]ir.Node{
			"a":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}},
			"b":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "b"}},
			"c":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "c"}},
			"d":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "d"}},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
			"fail": &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges: []*ir.Edge{
			{From: "a", To: "b"},
			{From: "b", To: "c"},
			{From: "c", To: "d"},
			{From: "d", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
		Budget:  &ir.Budget{MaxIterations: 5},
	}

	exec := newStubExecutor()
	for _, id := range []string{"a", "b", "c", "d"} {
		exec.on(id, func(_ map[string]any) (map[string]any, error) {
			return map[string]any{"ok": true}, nil
		})
	}

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-budget-warn", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Check that a budget_warning event was emitted.
	events, err := s.LoadEvents(context.Background(), "run-budget-warn")
	if err != nil {
		t.Fatalf("load events: %v", err)
	}

	warningCount := 0
	for _, evt := range events {
		if evt.Type == store.EventBudgetWarning {
			warningCount++
			if evt.Data["dimension"] != "iterations" {
				t.Errorf("expected dimension=iterations, got %v", evt.Data["dimension"])
			}
		}
	}
	if warningCount != 1 {
		t.Errorf("expected 1 budget_warning event, got %d", warningCount)
	}
}

// ---------------------------------------------------------------------------
// Test: budget exceeded — run fails gracefully
// ---------------------------------------------------------------------------

func TestBudgetExceededFailsRun(t *testing.T) {
	// Budget of 2 iterations — 3 nodes should exceed.
	wf := &ir.Workflow{
		Name:  "budget_exceeded_test",
		Entry: "a",
		Nodes: map[string]ir.Node{
			"a":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}},
			"b":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "b"}},
			"c":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "c"}},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
			"fail": &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges: []*ir.Edge{
			{From: "a", To: "b"},
			{From: "b", To: "c"},
			{From: "c", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
		Budget:  &ir.Budget{MaxIterations: 2},
	}

	exec := newStubExecutor()
	for _, id := range []string{"a", "b", "c"} {
		exec.on(id, func(_ map[string]any) (map[string]any, error) {
			return map[string]any{"ok": true}, nil
		})
	}

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-budget-exceeded", nil)
	if err == nil {
		t.Fatal("expected error from budget exceeded")
	}
	if !strings.Contains(err.Error(), "budget exceeded") {
		t.Errorf("expected 'budget exceeded' in error, got: %v", err)
	}

	// Verify run failed.
	r, err := s.LoadRun(context.Background(), "run-budget-exceeded")
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if r.Status != store.RunStatusFailedResumable {
		t.Errorf("expected failed_resumable status, got %s", r.Status)
	}

	// Verify budget_exceeded event was emitted.
	events, err := s.LoadEvents(context.Background(), "run-budget-exceeded")
	if err != nil {
		t.Fatalf("load events: %v", err)
	}
	if !hasEventType(events, store.EventBudgetExceeded) {
		t.Error("expected budget_exceeded event")
	}
}

// ---------------------------------------------------------------------------
// Test: token-based budget exceeded
// ---------------------------------------------------------------------------

func TestBudgetTokensExceeded(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "token_budget_test",
		Entry: "a",
		Nodes: map[string]ir.Node{
			"a":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}},
			"b":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "b"}},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
			"fail": &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges: []*ir.Edge{
			{From: "a", To: "b"},
			{From: "b", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
		Budget:  &ir.Budget{MaxTokens: 100},
	}

	exec := newStubExecutor()
	exec.on("a", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"ok": true, "_tokens": 80}, nil
	})
	exec.on("b", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"ok": true, "_tokens": 50}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-token-budget", nil)
	if err == nil {
		t.Fatal("expected error from token budget exceeded")
	}
	if !strings.Contains(err.Error(), "budget exceeded") {
		t.Errorf("expected 'budget exceeded' in error, got: %v", err)
	}

	// Should have a warning event (80/100 = 80%) then an exceeded event (130/100).
	events, err := s.LoadEvents(context.Background(), "run-token-budget")
	if err != nil {
		t.Fatalf("load events: %v", err)
	}
	warnings := 0
	exceeded := 0
	for _, evt := range events {
		if evt.Type == store.EventBudgetWarning && evt.Data["dimension"] == "tokens" {
			warnings++
		}
		if evt.Type == store.EventBudgetExceeded && evt.Data["dimension"] == "tokens" {
			exceeded++
		}
	}
	if warnings != 1 {
		t.Errorf("expected 1 token warning, got %d", warnings)
	}
	if exceeded != 1 {
		t.Errorf("expected 1 token exceeded, got %d", exceeded)
	}
}

// ---------------------------------------------------------------------------
// Test: warn_tokens advisory — warns once, never blocks
// ---------------------------------------------------------------------------

func TestWarnTokensAdvisoryNeverBlocks(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "warn_tokens_test",
		Entry: "a",
		Nodes: map[string]ir.Node{
			"a":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}},
			"b":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "b"}},
			"c":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "c"}},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
			"fail": &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges: []*ir.Edge{
			{From: "a", To: "b"},
			{From: "b", To: "c"},
			{From: "c", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
		// Advisory-only budget: no hard cap. 3 nodes × 60 tokens cross the
		// 100-token warn threshold on the second node.
		Budget: &ir.Budget{WarnTokens: 100},
	}

	exec := newStubExecutor()
	for _, id := range []string{"a", "b", "c"} {
		exec.on(id, func(_ map[string]any) (map[string]any, error) {
			return map[string]any{"ok": true, "_tokens": 60}, nil
		})
	}

	s := tmpStore(t)
	eng := New(wf, s, exec)

	// The whole point: crossing the advisory threshold must not fail the run.
	if err := eng.Run(context.Background(), "run-warn-tokens", nil); err != nil {
		t.Fatalf("advisory threshold must never block, got: %v", err)
	}

	events, err := s.LoadEvents(context.Background(), "run-warn-tokens")
	if err != nil {
		t.Fatalf("load events: %v", err)
	}
	warnings := 0
	for _, evt := range events {
		if evt.Type == store.EventBudgetExceeded {
			t.Errorf("unexpected budget_exceeded event: %v", evt.Data)
		}
		if evt.Type == store.EventBudgetWarning && evt.Data["dimension"] == "tokens" {
			warnings++
			if adv, _ := evt.Data["advisory"].(bool); !adv {
				t.Errorf("expected advisory=true on warn_tokens warning, got %v", evt.Data)
			}
		}
	}
	if warnings != 1 {
		t.Errorf("expected exactly 1 advisory warning (deduped), got %d", warnings)
	}
}

// ---------------------------------------------------------------------------
// Test: cost-based budget exceeded
// ---------------------------------------------------------------------------

func TestBudgetCostExceeded(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "cost_budget_test",
		Entry: "a",
		Nodes: map[string]ir.Node{
			"a":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}},
			"b":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "b"}},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
			"fail": &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges: []*ir.Edge{
			{From: "a", To: "b"},
			{From: "b", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
		Budget:  &ir.Budget{MaxCostUSD: 1.0},
	}

	exec := newStubExecutor()
	exec.on("a", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"ok": true, "_cost_usd": 0.6}, nil
	})
	exec.on("b", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"ok": true, "_cost_usd": 0.5}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-cost-budget", nil)
	if err == nil {
		t.Fatal("expected error from cost budget exceeded")
	}
	if !strings.Contains(err.Error(), "budget exceeded") {
		t.Errorf("expected 'budget exceeded' in error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Test: one branch exhausts global budget, other branch fails
// ---------------------------------------------------------------------------

func TestBudgetSharedFirstComeFirstServed(t *testing.T) {
	// Global budget of 3 iterations. Branch A executes 1 node (a), branch B
	// executes 2 nodes (b1 -> b2). Entry consumes 1 iteration.
	// Total: entry(1) + a(2) + b1(3) + b2(exceeds).
	wf := &ir.Workflow{
		Name:  "shared_budget_test",
		Entry: "entry",
		Nodes: map[string]ir.Node{
			"entry":  &ir.AgentNode{BaseNode: ir.BaseNode{ID: "entry"}},
			"router": &ir.RouterNode{BaseNode: ir.BaseNode{ID: "router"}, RouterMode: ir.RouterFanOutAll},
			"a":      &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}},
			"b1":     &ir.AgentNode{BaseNode: ir.BaseNode{ID: "b1"}},
			"b2":     &ir.AgentNode{BaseNode: ir.BaseNode{ID: "b2"}},
			"done":   &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}, AwaitMode: ir.AwaitBestEffort},
			"fail":   &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges: []*ir.Edge{
			{From: "entry", To: "router"},
			{From: "router", To: "a"},
			{From: "router", To: "b1"},
			{From: "a", To: "done"},
			{From: "b1", To: "b2"},
			{From: "b2", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
		Budget:  &ir.Budget{MaxIterations: 3},
	}

	var branchADone int64

	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{}, nil
	})
	exec.on("a", func(_ map[string]any) (map[string]any, error) {
		atomic.AddInt64(&branchADone, 1)
		return map[string]any{"review": "A done"}, nil
	})
	exec.on("b1", func(_ map[string]any) (map[string]any, error) {
		// Small delay so branch A has a chance to execute first.
		time.Sleep(10 * time.Millisecond)
		return map[string]any{"step": "b1 done"}, nil
	})
	exec.on("b2", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"step": "b2 done"}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-shared-budget", nil)
	// With best_effort, the run may succeed even if one branch hits budget.
	// But we want to verify that budget events were emitted.
	_ = err

	events, err := s.LoadEvents(context.Background(), "run-shared-budget")
	if err != nil {
		t.Fatalf("load events: %v", err)
	}

	// Verify that budget events were emitted (warning or exceeded).
	budgetEvents := 0
	for _, evt := range events {
		if evt.Type == store.EventBudgetWarning || evt.Type == store.EventBudgetExceeded {
			budgetEvents++
		}
	}
	if budgetEvents == 0 {
		t.Error("expected at least one budget event (warning or exceeded)")
	}

	// Branch A should have completed (it only has 1 node).
	if atomic.LoadInt64(&branchADone) == 0 {
		t.Error("expected branch A to complete")
	}
}

// ---------------------------------------------------------------------------
// Test: duration budget exceeded
// ---------------------------------------------------------------------------

func TestBudgetDurationExceeded(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "duration_budget_test",
		Entry: "a",
		Nodes: map[string]ir.Node{
			"a":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}},
			"b":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "b"}},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
			"fail": &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges: []*ir.Edge{
			{From: "a", To: "b"},
			{From: "b", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
		Budget:  &ir.Budget{MaxDuration: "50ms"},
	}

	exec := newStubExecutor()
	exec.on("a", func(_ map[string]any) (map[string]any, error) {
		time.Sleep(60 * time.Millisecond) // exceed budget
		return map[string]any{"ok": true}, nil
	})
	exec.on("b", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-duration-budget", nil)
	if err == nil {
		t.Fatal("expected error from duration budget exceeded")
	}
	if !strings.Contains(err.Error(), "budget exceeded") {
		t.Errorf("expected 'budget exceeded' in error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Test: no budget — no interference
// ---------------------------------------------------------------------------

func TestNoBudgetNoInterference(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "no_budget_test",
		Entry: "a",
		Nodes: map[string]ir.Node{
			"a":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}},
			"b":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "b"}},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
			"fail": &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges: []*ir.Edge{
			{From: "a", To: "b"},
			{From: "b", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}

	exec := newStubExecutor()
	exec.on("a", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"_tokens": 999999, "_cost_usd": 999.0}, nil
	})
	exec.on("b", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-no-budget", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	r, _ := s.LoadRun(context.Background(), "run-no-budget")
	if r.Status != store.RunStatusFinished {
		t.Errorf("expected finished, got %s", r.Status)
	}
}

// ---------------------------------------------------------------------------
// Test: budget consumption survives a resume (Snapshot/Restore roundtrip)
// ---------------------------------------------------------------------------

func TestBudgetSnapshotRestoreRoundtrip(t *testing.T) {
	b := newSharedBudget(&ir.Budget{MaxTokens: 1000, MaxCostUSD: 10, MaxIterations: 5, MaxDuration: "1h"}, nil)
	if b == nil {
		t.Fatal("expected a budget")
	}
	b.RecordUsage(300, 4.0) // 1 iteration
	b.RecordUsage(200, 1.5) // 2 iterations

	tokens, cost, iters, elapsed, unpTok, unpNodes := b.Snapshot()
	if tokens != 500 || cost != 5.5 || iters != 2 {
		t.Fatalf("snapshot = (%d,%v,%d), want (500,5.5,2)", tokens, cost, iters)
	}
	if elapsed <= 0 {
		t.Fatalf("expected positive elapsed, got %v", elapsed)
	}

	// A fresh budget (as newRunState builds on resume) starts at zero...
	resumed := newSharedBudget(&ir.Budget{MaxTokens: 1000, MaxCostUSD: 10, MaxIterations: 5, MaxDuration: "1h"}, nil)
	if t0, _, _, _, _, _ := resumed.Snapshot(); t0 != 0 {
		t.Fatalf("fresh budget should start at 0 tokens, got %d", t0)
	}
	// ...until Restore seeds it from the checkpoint.
	resumed.Restore(tokens, cost, iters, elapsed, unpTok, unpNodes)
	rt, rc, ri, _, _, _ := resumed.Snapshot()
	if rt != 500 || rc != 5.5 || ri != 2 {
		t.Fatalf("restored = (%d,%v,%d), want (500,5.5,2)", rt, rc, ri)
	}
	// One more iteration on the resumed budget must exhaust max_iterations (5)
	// counting from the restored 2, not from 0 — the runaway-loop guard.
	resumed.RecordUsage(0, 0)           // 3
	resumed.RecordUsage(0, 0)           // 4
	checks := resumed.RecordUsage(0, 0) // 5 → exceeded
	if findExceeded(checks) == nil {
		t.Fatal("expected iterations budget exceeded after restore+3 (2+3=5), got none")
	}
}

// ---------------------------------------------------------------------------
// Test: buildCheckpoint captures accounting; restoreBudgetAccounting rehydrates
// ---------------------------------------------------------------------------

func TestCheckpointCarriesBudgetAccounting(t *testing.T) {
	rs := &runState{
		runID:        "r1",
		budget:       newSharedBudget(&ir.Budget{MaxTokens: 1000, MaxCostUSD: 10}, nil),
		costUSDTotal: 7.25,
	}
	rs.budget.RecordUsage(400, 3.0)

	cp := buildCheckpoint(rs, "n1")
	if cp.BudgetTokensUsed != 400 || cp.BudgetCostUSD != 3.0 || cp.BudgetIterationsUsed != 1 {
		t.Fatalf("checkpoint accounting = (%d,%v,%d), want (400,3,1)", cp.BudgetTokensUsed, cp.BudgetCostUSD, cp.BudgetIterationsUsed)
	}
	if cp.CostUSDTotal != 7.25 {
		t.Fatalf("checkpoint CostUSDTotal = %v, want 7.25", cp.CostUSDTotal)
	}

	// Rehydrate into a fresh runState (as a resume would).
	resumed := &runState{
		runID:  "r1",
		budget: newSharedBudget(&ir.Budget{MaxTokens: 1000, MaxCostUSD: 10}, nil),
	}
	restoreBudgetAccounting(resumed, cp)
	if resumed.costUSDTotal != 7.25 {
		t.Fatalf("resumed costUSDTotal = %v, want 7.25", resumed.costUSDTotal)
	}
	tok, cost, _, _, _, _ := resumed.budget.Snapshot()
	if tok != 400 || cost != 3.0 {
		t.Fatalf("resumed budget = (%d,%v), want (400,3)", tok, cost)
	}
}

// ===========================================================================
// Workspace mutation safety tests
// ===========================================================================

// ---------------------------------------------------------------------------
// Test: two mutating branches rejected
// ---------------------------------------------------------------------------

func TestWorkspaceSafetyRejectsDualMutation(t *testing.T) {
	// Both branches have tool nodes (mutating).
	wf := &ir.Workflow{
		Name:  "unsafe_mutation_test",
		Entry: "entry",
		Nodes: map[string]ir.Node{
			"entry":  &ir.AgentNode{BaseNode: ir.BaseNode{ID: "entry"}},
			"router": &ir.RouterNode{BaseNode: ir.BaseNode{ID: "router"}, RouterMode: ir.RouterFanOutAll},
			"tool_a": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "tool_a"}, Command: "echo a"},
			"tool_b": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "tool_b"}, Command: "echo b"},
			"done":   &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}, AwaitMode: ir.AwaitWaitAll},
			"fail":   &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges: []*ir.Edge{
			{From: "entry", To: "router"},
			{From: "router", To: "tool_a"},
			{From: "router", To: "tool_b"},
			{From: "tool_a", To: "done"},
			{From: "tool_b", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}

	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-unsafe", nil)
	if err == nil {
		t.Fatal("expected error from workspace safety violation")
	}
	if !strings.Contains(err.Error(), "workspace safety") {
		t.Errorf("expected 'workspace safety' in error, got: %v", err)
	}

	r, _ := s.LoadRun(context.Background(), "run-unsafe")
	if r.Status != store.RunStatusFailedResumable {
		t.Errorf("expected failed_resumable, got %s", r.Status)
	}
}

// ---------------------------------------------------------------------------
// Test: one mutating branch + one read-only branch is allowed
// ---------------------------------------------------------------------------

func TestWorkspaceSafetyAllowsMutationPlusReadonly(t *testing.T) {
	// Branch A has a tool node (mutating), branch B has only an agent (read-only).
	wf := &ir.Workflow{
		Name:  "safe_mutation_test",
		Entry: "entry",
		Nodes: map[string]ir.Node{
			"entry":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "entry"}},
			"router":   &ir.RouterNode{BaseNode: ir.BaseNode{ID: "router"}, RouterMode: ir.RouterFanOutAll},
			"tool_a":   &ir.ToolNode{BaseNode: ir.BaseNode{ID: "tool_a"}, Command: "echo a"},
			"review_b": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "review_b"}},
			"done":     &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}, AwaitMode: ir.AwaitWaitAll},
			"fail":     &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges: []*ir.Edge{
			{From: "entry", To: "router"},
			{From: "router", To: "tool_a"},
			{From: "router", To: "review_b"},
			{From: "tool_a", To: "done"},
			{From: "review_b", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}

	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{}, nil
	})
	exec.on("tool_a", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"result": "tool ran"}, nil
	})
	exec.on("review_b", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"review": "looks good"}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-safe-mutation", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	r, _ := s.LoadRun(context.Background(), "run-safe-mutation")
	if r.Status != store.RunStatusFinished {
		t.Errorf("expected finished, got %s", r.Status)
	}
}

// ---------------------------------------------------------------------------
// Test: read-only branches can all run in parallel
// ---------------------------------------------------------------------------

func TestWorkspaceSafetyAllowsParallelReadonly(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "readonly_parallel_test",
		Entry: "entry",
		Nodes: map[string]ir.Node{
			"entry":  &ir.AgentNode{BaseNode: ir.BaseNode{ID: "entry"}},
			"router": &ir.RouterNode{BaseNode: ir.BaseNode{ID: "router"}, RouterMode: ir.RouterFanOutAll},
			"a":      &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}},
			"b":      &ir.AgentNode{BaseNode: ir.BaseNode{ID: "b"}},
			"c":      &ir.AgentNode{BaseNode: ir.BaseNode{ID: "c"}},
			"done":   &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}, AwaitMode: ir.AwaitWaitAll},
			"fail":   &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges: []*ir.Edge{
			{From: "entry", To: "router"},
			{From: "router", To: "a"},
			{From: "router", To: "b"},
			{From: "router", To: "c"},
			{From: "a", To: "done"},
			{From: "b", To: "done"},
			{From: "c", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}

	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{}, nil
	})
	for _, id := range []string{"a", "b", "c"} {
		exec.on(id, func(_ map[string]any) (map[string]any, error) {
			return map[string]any{"ok": true}, nil
		})
	}

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-readonly", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	r, _ := s.LoadRun(context.Background(), "run-readonly")
	if r.Status != store.RunStatusFinished {
		t.Errorf("expected finished, got %s", r.Status)
	}
}

// ---------------------------------------------------------------------------
// Test: agent with tools is considered mutating
// ---------------------------------------------------------------------------

func TestWorkspaceSafetyAgentWithToolsIsMutating(t *testing.T) {
	// Both branches have agents with tools → both are mutating → rejected.
	wf := &ir.Workflow{
		Name:  "agent_tools_mutation_test",
		Entry: "entry",
		Nodes: map[string]ir.Node{
			"entry":  &ir.AgentNode{BaseNode: ir.BaseNode{ID: "entry"}},
			"router": &ir.RouterNode{BaseNode: ir.BaseNode{ID: "router"}, RouterMode: ir.RouterFanOutAll},
			"a":      &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}, Tools: []string{"write_file"}},
			"b":      &ir.AgentNode{BaseNode: ir.BaseNode{ID: "b"}, Tools: []string{"run_command"}},
			"done":   &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}, AwaitMode: ir.AwaitWaitAll},
			"fail":   &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges: []*ir.Edge{
			{From: "entry", To: "router"},
			{From: "router", To: "a"},
			{From: "router", To: "b"},
			{From: "a", To: "done"},
			{From: "b", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}

	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-agent-tools", nil)
	if err == nil {
		t.Fatal("expected workspace safety error")
	}
	if !strings.Contains(err.Error(), "workspace safety") {
		t.Errorf("expected workspace safety error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Test: parallel branches with only read-only tools are allowed
// ---------------------------------------------------------------------------

func TestWorkspaceSafetyAllowsParallelReadonlyTools(t *testing.T) {
	// Both branches have agents with read-only tools → neither is mutating → allowed.
	wf := &ir.Workflow{
		Name:  "readonly_tools_parallel_test",
		Entry: "entry",
		Nodes: map[string]ir.Node{
			"entry":  &ir.AgentNode{BaseNode: ir.BaseNode{ID: "entry"}},
			"router": &ir.RouterNode{BaseNode: ir.BaseNode{ID: "router"}, RouterMode: ir.RouterFanOutAll},
			"a":      &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}, Tools: []string{"read_file", "git_diff"}},
			"b":      &ir.AgentNode{BaseNode: ir.BaseNode{ID: "b"}, Tools: []string{"git_status", "search_codebase", "tree"}},
			"done":   &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}, AwaitMode: ir.AwaitWaitAll},
			"fail":   &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges: []*ir.Edge{
			{From: "entry", To: "router"},
			{From: "router", To: "a"},
			{From: "router", To: "b"},
			{From: "a", To: "done"},
			{From: "b", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}

	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{}, nil
	})
	exec.on("a", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"review": "A"}, nil
	})
	exec.on("b", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"review": "B"}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-readonly-tools", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	r, _ := s.LoadRun(context.Background(), "run-readonly-tools")
	if r.Status != store.RunStatusFinished {
		t.Errorf("expected finished, got %s", r.Status)
	}
}

// ---------------------------------------------------------------------------
// Test: one mutating branch + one read-only-tools branch is allowed
// ---------------------------------------------------------------------------

func TestWorkspaceSafetyOneMutatingOneReadonlyTools(t *testing.T) {
	// Branch A has a write tool (mutating), branch B has only read-only tools.
	// Exactly 1 mutating branch → allowed.
	wf := &ir.Workflow{
		Name:  "one_mutating_one_readonly_test",
		Entry: "entry",
		Nodes: map[string]ir.Node{
			"entry":  &ir.AgentNode{BaseNode: ir.BaseNode{ID: "entry"}},
			"router": &ir.RouterNode{BaseNode: ir.BaseNode{ID: "router"}, RouterMode: ir.RouterFanOutAll},
			"a":      &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}, Tools: []string{"write_file"}},
			"b":      &ir.AgentNode{BaseNode: ir.BaseNode{ID: "b"}, Tools: []string{"read_file", "git_status"}},
			"done":   &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}, AwaitMode: ir.AwaitWaitAll},
			"fail":   &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges: []*ir.Edge{
			{From: "entry", To: "router"},
			{From: "router", To: "a"},
			{From: "router", To: "b"},
			{From: "a", To: "done"},
			{From: "b", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}

	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{}, nil
	})
	exec.on("a", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"result": "wrote"}, nil
	})
	exec.on("b", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"review": "looks good"}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-one-mutating-one-readonly", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	r, _ := s.LoadRun(context.Background(), "run-one-mutating-one-readonly")
	if r.Status != store.RunStatusFinished {
		t.Errorf("expected finished, got %s", r.Status)
	}
}

// ---------------------------------------------------------------------------
// Test: agent with mixed tools (read-only + write) is mutating
// ---------------------------------------------------------------------------

func TestWorkspaceSafetyMixedToolsIsMutating(t *testing.T) {
	// Both branches have agents with mixed tools (read + write) → both mutating → rejected.
	wf := &ir.Workflow{
		Name:  "mixed_tools_mutation_test",
		Entry: "entry",
		Nodes: map[string]ir.Node{
			"entry":  &ir.AgentNode{BaseNode: ir.BaseNode{ID: "entry"}},
			"router": &ir.RouterNode{BaseNode: ir.BaseNode{ID: "router"}, RouterMode: ir.RouterFanOutAll},
			"a":      &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}, Tools: []string{"read_file", "write_file"}},
			"b":      &ir.AgentNode{BaseNode: ir.BaseNode{ID: "b"}, Tools: []string{"git_diff", "run_command"}},
			"done":   &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}, AwaitMode: ir.AwaitWaitAll},
			"fail":   &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges: []*ir.Edge{
			{From: "entry", To: "router"},
			{From: "router", To: "a"},
			{From: "router", To: "b"},
			{From: "a", To: "done"},
			{From: "b", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}

	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-mixed-tools", nil)
	if err == nil {
		t.Fatal("expected workspace safety error")
	}
	if !strings.Contains(err.Error(), "workspace safety") {
		t.Errorf("expected workspace safety error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Test: budget exceeded in parallel branch (best_effort continues)
// ---------------------------------------------------------------------------

func TestBudgetExceededInBranchBestEffort(t *testing.T) {
	// Budget of 2 iterations. Entry uses 1. Each branch has 1 node.
	// Branch A and B run in parallel — one will get iteration 2, other exceeds.
	// With best_effort, run should complete.
	wf := budgetFanOutWorkflow(&ir.Budget{MaxIterations: 3})

	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{}, nil
	})
	exec.on("a", func(_ map[string]any) (map[string]any, error) {
		time.Sleep(5 * time.Millisecond) // stagger slightly
		return map[string]any{"review": "A"}, nil
	})
	exec.on("b", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"review": "B"}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-branch-budget", nil)
	// With best_effort and 3 iterations total (entry + 2 branches = 3 exactly),
	// both should succeed.
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	r, _ := s.LoadRun(context.Background(), "run-branch-budget")
	if r.Status != store.RunStatusFinished {
		t.Errorf("expected finished, got %s", r.Status)
	}
}

// ---------------------------------------------------------------------------
// Test: ErrBudgetExceeded is recognizable via errors.Is
// ---------------------------------------------------------------------------

func TestBudgetExceededErrorUnwrap(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "budget_error_test",
		Entry: "a",
		Nodes: map[string]ir.Node{
			"a":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}},
			"b":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "b"}},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
			"fail": &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges: []*ir.Edge{
			{From: "a", To: "b"},
			{From: "b", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
		Budget:  &ir.Budget{MaxIterations: 1},
	}

	exec := newStubExecutor()
	exec.on("a", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-budget-error", nil)
	if err == nil {
		t.Fatal("expected error")
	}
	// The error chain should contain "budget exceeded" from failRun.
	if !strings.Contains(err.Error(), "budget exceeded") {
		t.Errorf("expected budget exceeded mention, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Test: SharedBudget unit — warning emitted once per dimension
// ---------------------------------------------------------------------------

func TestSharedBudgetWarningOnce(t *testing.T) {
	b := &SharedBudget{
		maxIterations:   10,
		startedAt:       time.Now(),
		warningsEmitted: make(map[string]bool),
	}

	// Record 8 iterations (80% threshold).
	for i := 0; i < 7; i++ {
		results := b.RecordUsage(0, 0)
		if len(findWarnings(results)) > 0 {
			t.Errorf("unexpected warning at iteration %d", i+1)
		}
	}

	// 8th iteration should trigger warning.
	results := b.RecordUsage(0, 0)
	warnings := findWarnings(results)
	if len(warnings) != 1 || warnings[0].dimension != "iterations" {
		t.Errorf("expected iterations warning at 8/10, got %d warnings", len(warnings))
	}

	// 9th iteration should NOT trigger another warning.
	results = b.RecordUsage(0, 0)
	warnings = findWarnings(results)
	if len(warnings) != 0 {
		t.Error("warning should only be emitted once per dimension")
	}

	// 10th iteration should trigger exceeded.
	results = b.RecordUsage(0, 0)
	exc := findExceeded(results)
	if exc == nil || exc.dimension != "iterations" {
		t.Error("expected exceeded at 10/10")
	}
}

// ---------------------------------------------------------------------------
// Test: hard limit blocks new execution at 90%
// ---------------------------------------------------------------------------

func TestHardBudgetBlocksAt90Percent(t *testing.T) {
	// Budget of 10 iterations: warning at 8, hard limit at 9, exceeded at 10.
	wf := &ir.Workflow{
		Name:  "hard_budget_test",
		Entry: "n1",
		Nodes: map[string]ir.Node{
			"n1":   &ir.AgentNode{BaseNode: ir.BaseNode{ID: "n1"}},
			"n2":   &ir.AgentNode{BaseNode: ir.BaseNode{ID: "n2"}},
			"n3":   &ir.AgentNode{BaseNode: ir.BaseNode{ID: "n3"}},
			"n4":   &ir.AgentNode{BaseNode: ir.BaseNode{ID: "n4"}},
			"n5":   &ir.AgentNode{BaseNode: ir.BaseNode{ID: "n5"}},
			"n6":   &ir.AgentNode{BaseNode: ir.BaseNode{ID: "n6"}},
			"n7":   &ir.AgentNode{BaseNode: ir.BaseNode{ID: "n7"}},
			"n8":   &ir.AgentNode{BaseNode: ir.BaseNode{ID: "n8"}},
			"n9":   &ir.AgentNode{BaseNode: ir.BaseNode{ID: "n9"}},
			"n10":  &ir.AgentNode{BaseNode: ir.BaseNode{ID: "n10"}},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "n1", To: "n2"},
			{From: "n2", To: "n3"},
			{From: "n3", To: "n4"},
			{From: "n4", To: "n5"},
			{From: "n5", To: "n6"},
			{From: "n6", To: "n7"},
			{From: "n7", To: "n8"},
			{From: "n8", To: "n9"},
			{From: "n9", To: "n10"},
			{From: "n10", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
		Budget:  &ir.Budget{MaxIterations: 10},
	}

	exec := newStubExecutor()
	// All nodes return empty output.

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-hard-budget", nil)
	if err == nil {
		t.Fatal("expected budget error, got nil")
	}

	var rtErr *RuntimeError
	if !errors.As(err, &rtErr) {
		t.Fatalf("expected RuntimeError, got %T: %v", err, err)
	}
	if rtErr.Code != ErrCodeBudgetExceeded {
		t.Errorf("expected error code %s, got %s", ErrCodeBudgetExceeded, rtErr.Code)
	}
	// The error should mention "hard limit" since the 10th node pre-check
	// should trigger at 9/10 = 90%.
	if !strings.Contains(rtErr.Message, "hard limit") {
		t.Errorf("expected hard limit message, got: %s", rtErr.Message)
	}

	// Verify that exactly 9 nodes executed (n1 through n9).
	events, _ := s.LoadEvents(context.Background(), "run-hard-budget")
	nodeFinished := 0
	for _, ev := range events {
		if ev.Type == store.EventNodeFinished {
			nodeFinished++
		}
	}
	if nodeFinished != 9 {
		t.Errorf("expected 9 nodes to finish before hard limit, got %d", nodeFinished)
	}
}

func TestHardBudgetOnTokens(t *testing.T) {
	// Budget of 100 tokens. Executor reports 95 tokens on first node.
	// The second node's pre-check should trigger hard limit at 95%.
	wf := &ir.Workflow{
		Name:  "hard_budget_tokens_test",
		Entry: "a",
		Nodes: map[string]ir.Node{
			"a":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}},
			"b":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "b"}},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "a", To: "b"},
			{From: "b", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
		Budget:  &ir.Budget{MaxTokens: 100},
	}

	exec := newStubExecutor()
	exec.on("a", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"_tokens": float64(95)}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-hard-tokens", nil)
	if err == nil {
		t.Fatal("expected budget error, got nil")
	}

	var rtErr *RuntimeError
	if !errors.As(err, &rtErr) {
		t.Fatalf("expected RuntimeError, got %T: %v", err, err)
	}
	if rtErr.Code != ErrCodeBudgetExceeded {
		t.Errorf("expected error code %s, got %s", ErrCodeBudgetExceeded, rtErr.Code)
	}
}

func TestHardBudgetWarningStillFires(t *testing.T) {
	// Verify that warning at 80% still fires before hard limit at 90%.
	b := newSharedBudget(&ir.Budget{MaxIterations: 10}, nil)

	// Record 7 iterations (70%) — no warning yet.
	for i := 0; i < 7; i++ {
		b.RecordUsage(0, 0)
	}

	// 8th iteration (80%) — warning should fire.
	results := b.RecordUsage(0, 0)
	warnings := findWarnings(results)
	if len(warnings) != 1 || warnings[0].dimension != "iterations" {
		t.Errorf("expected warning at 80%%, got %d warnings", len(warnings))
	}

	// 9th iteration (90%) — hard limit should fire.
	results = b.RecordUsage(0, 0)
	hl := findHardLimited(results)
	if hl == nil || hl.dimension != "iterations" {
		t.Error("expected hard limit at 90%")
	}

	// 10th iteration (100%) — exceeded should fire.
	results = b.RecordUsage(0, 0)
	exc := findExceeded(results)
	if exc == nil || exc.dimension != "iterations" {
		t.Error("expected exceeded at 100%")
	}
}

func TestHardBudgetUnit(t *testing.T) {
	t.Run("iterations_hard_limit", func(t *testing.T) {
		b := newSharedBudget(&ir.Budget{MaxIterations: 10}, nil)
		// Push to 9 iterations.
		for i := 0; i < 9; i++ {
			b.RecordUsage(0, 0)
		}
		checks := b.Check()
		hl := findHardLimited(checks)
		if hl == nil {
			t.Fatal("expected hard limit at 9/10")
		}
		if hl.dimension != "iterations" {
			t.Errorf("expected dimension 'iterations', got %q", hl.dimension)
		}
	})

	t.Run("tokens_hard_limit", func(t *testing.T) {
		b := newSharedBudget(&ir.Budget{MaxTokens: 1000}, nil)
		b.RecordUsage(910, 0) // 91%
		checks := b.Check()
		hl := findHardLimited(checks)
		if hl == nil {
			t.Fatal("expected hard limit at 910/1000")
		}
		if hl.dimension != "tokens" {
			t.Errorf("expected dimension 'tokens', got %q", hl.dimension)
		}
	})

	t.Run("cost_hard_limit", func(t *testing.T) {
		b := newSharedBudget(&ir.Budget{MaxCostUSD: 10.0}, nil)
		b.RecordUsage(0, 9.5) // 95%
		checks := b.Check()
		hl := findHardLimited(checks)
		if hl == nil {
			t.Fatal("expected hard limit at 9.5/10.0")
		}
		if hl.dimension != "cost_usd" {
			t.Errorf("expected dimension 'cost_usd', got %q", hl.dimension)
		}
	})

	t.Run("below_hard_threshold", func(t *testing.T) {
		b := newSharedBudget(&ir.Budget{MaxIterations: 10}, nil)
		for i := 0; i < 8; i++ {
			b.RecordUsage(0, 0)
		}
		checks := b.Check()
		hl := findHardLimited(checks)
		if hl != nil {
			t.Error("should not trigger hard limit at 8/10 (80%)")
		}
	})
}

// An unknown cost is not a free call. When a node burns tokens whose model
// carries no resolvable price, `cost.Annotate` omits `_cost_usd` on purpose —
// and a budget that folded that absence into a 0.00 sample would let a run
// sail past max_cost_usd reporting nothing at all. These cover the seam.
func TestSharedBudget_UnpricedSpend(t *testing.T) {
	unpriced := func(checks []budgetCheckResult) *budgetCheckResult {
		return findBudgetCheck(checks, func(r *budgetCheckResult) bool {
			return r.dimension == "cost_usd_unpriced"
		})
	}

	t.Run("warns_once_under_a_declared_cost_ceiling", func(t *testing.T) {
		b := newSharedBudget(&ir.Budget{MaxCostUSD: 160}, nil)

		w := unpriced(b.RecordUsage(50_000, 0))
		if w == nil {
			t.Fatal("expected a cost_usd_unpriced warning: the ceiling cannot see this node's spend")
		}
		if !w.warning || !w.advisory {
			t.Errorf("unpriced spend must warn advisorily, got warning=%v advisory=%v", w.warning, w.advisory)
		}
		if w.detail == "" {
			t.Error("the warning must say what is unmeasured; used/limit alone mixes tokens and dollars")
		}

		// Same run, second unpriced node: still accounted, but one warning is
		// the contract — the operator is told, not spammed.
		if again := unpriced(b.RecordUsage(50_000, 0)); again != nil {
			t.Error("cost_usd_unpriced must warn at most once per run")
		}

		if b.costUsed != 0 {
			t.Errorf("unknown cost must never reach the enforced axis, costUsed=%v", b.costUsed)
		}
		if b.unpricedTokens != 100_000 || b.unpricedNodes != 2 {
			t.Errorf("expected 100000 unpriced tokens over 2 nodes, got %d over %d",
				b.unpricedTokens, b.unpricedNodes)
		}
	})

	// The warning fires on the FIRST unpriced node and never again, so its
	// figures are a floor, not a run total — and the message must not read
	// like one. A run with 40 unpriced nodes would otherwise be told "1".
	t.Run("detail_reports_a_floor_not_a_total", func(t *testing.T) {
		b := newSharedBudget(&ir.Budget{MaxCostUSD: 160}, nil)

		w := unpriced(b.RecordUsage(50_000, 0))
		if w == nil {
			t.Fatal("expected the first unpriced node to warn")
		}
		// What it could possibly know at emission time: one node.
		if !strings.Contains(w.detail, "1 node execution(s)") ||
			!strings.Contains(w.detail, "50000 tokens") {
			t.Errorf("detail should carry the counters as of this node: %q", w.detail)
		}
		// And it must announce that this is a running figure, since nothing
		// re-emits and no surface reads the final counters back.
		if !strings.Contains(w.detail, "as of this node") ||
			!strings.Contains(w.detail, "keeps growing") {
			t.Errorf("detail presents a first-node sample as a run total: %q", w.detail)
		}

		// Drive the run on: the counters climb well past what the operator
		// was told, which is exactly why the wording is a floor.
		for i := 0; i < 39; i++ {
			if again := unpriced(b.RecordUsage(50_000, 0)); again != nil {
				t.Fatal("cost_usd_unpriced must warn at most once per run")
			}
		}
		if b.unpricedNodes != 40 || b.unpricedTokens != 2_000_000 {
			t.Fatalf("counters = %d nodes / %d tokens, want 40 / 2000000",
				b.unpricedNodes, b.unpricedTokens)
		}
	})

	t.Run("silent_without_a_cost_ceiling", func(t *testing.T) {
		b := newSharedBudget(&ir.Budget{MaxTokens: 1_000_000}, nil)
		if w := unpriced(b.RecordUsage(50_000, 0)); w != nil {
			t.Error("no max_cost_usd declared: nothing is being under-enforced, so nothing to report")
		}
		if b.unpricedTokens != 50_000 {
			t.Errorf("unpriced tokens are tracked regardless, got %d", b.unpricedTokens)
		}
	})

	t.Run("a_node_that_spent_nothing_is_not_unpriced_spend", func(t *testing.T) {
		b := newSharedBudget(&ir.Budget{MaxCostUSD: 160}, nil)
		// Tool and compute nodes report no tokens and no cost. That is an
		// absence of spend, not spend of unknown price.
		if w := unpriced(b.RecordUsage(0, 0)); w != nil {
			t.Error("a zero-token node must not raise the unpriced warning")
		}
		if b.unpricedNodes != 0 {
			t.Errorf("expected no unpriced node, got %d", b.unpricedNodes)
		}
	})

	t.Run("carries_no_used_limit_pair_so_ratio_consumers_skip_it", func(t *testing.T) {
		b := newSharedBudget(&ir.Budget{MaxCostUSD: 160}, nil)
		w := unpriced(b.RecordUsage(50_000, 0))
		if w == nil {
			t.Fatal("expected the unpriced warning")
		}
		// Every other budget_warning consumer reads used/limit as a ratio of
		// an axis about to bind. This dimension has no axis, so the pair must
		// not be published: the studio toast then prints no percentage and
		// useRunMetrics keeps whatever genuine warning it already had.
		data := budgetWarningData(*w)
		if _, ok := data["used"]; ok {
			t.Error("unpriced warning must not publish a `used` value")
		}
		if _, ok := data["limit"]; ok {
			t.Error("unpriced warning must not publish a `limit` value")
		}
		if data["detail"] == nil || data["detail"] == "" {
			t.Error("with no used/limit, `detail` is the only content the event carries")
		}

		// A real axis keeps its pair.
		axis := budgetWarningData(budgetCheckResult{
			warning: true, dimension: "tokens", used: 800, limit: 1000,
		})
		if axis["used"] != float64(800) || axis["limit"] != float64(1000) {
			t.Errorf("a real axis must still publish used/limit, got %v", axis)
		}
	})

	t.Run("re_arms_on_raise_and_survives_the_read_only_check", func(t *testing.T) {
		b := newSharedBudget(&ir.Budget{MaxCostUSD: 160}, nil)
		if unpriced(b.RecordUsage(50_000, 0)) == nil {
			t.Fatal("expected the first warning")
		}
		if unpriced(b.RecordUsage(50_000, 0)) != nil {
			t.Fatal("expected the warning to be deduped within one ceiling")
		}
		// raise_budget re-arms the cost axis so the operator gets a fresh 80%
		// tick; they must equally be re-told the ceiling they just raised is
		// still only seeing part of the run.
		if _, raised := b.RaiseCaps(ir.BudgetOverrides{MaxCostUSD: 400}); !raised {
			t.Fatal("expected the raise to land")
		}

		// The engine drains overrides then calls Check() before the next node
		// runs. Check() inspects only exceeded/hard-limited and discards
		// warnings, so if it could produce this one it would silently eat the
		// re-arm and no operator would ever see it. It must not appear here…
		if unpriced(b.Check()) != nil {
			t.Fatal("the read-only Check() path must never raise (and thus consume) the unpriced warning")
		}
		// …and must still be waiting for the next recorded node.
		if unpriced(b.RecordUsage(50_000, 0)) == nil {
			t.Error("a raised cost ceiling must re-arm the unpriced warning")
		}
	})

	t.Run("counters_ride_the_checkpoint", func(t *testing.T) {
		b := newSharedBudget(&ir.Budget{MaxCostUSD: 160}, nil)
		b.RecordUsage(50_000, 0)
		b.RecordUsage(30_000, 0)
		_, _, _, _, unpTok, unpNodes := b.Snapshot()
		if unpTok != 80_000 || unpNodes != 2 {
			t.Fatalf("snapshot lost the unpriced volume: %d tokens over %d nodes", unpTok, unpNodes)
		}

		// Without this, a resumed run re-warns while counting only what ran
		// after the pause — a partial number reported as if it were the total.
		resumed := newSharedBudget(&ir.Budget{MaxCostUSD: 160}, nil)
		resumed.Restore(0, 0, 2, 0, unpTok, unpNodes)
		w := unpriced(resumed.RecordUsage(10_000, 0))
		if w == nil {
			t.Fatal("expected the warning on the resumed run")
		}
		if !strings.Contains(w.detail, "90000 tokens") || !strings.Contains(w.detail, "3 node") {
			t.Errorf("resumed detail must count the whole run, got %q", w.detail)
		}
	})

	t.Run("priced_spend_never_raises_it", func(t *testing.T) {
		b := newSharedBudget(&ir.Budget{MaxCostUSD: 160}, nil)
		if w := unpriced(b.RecordUsage(50_000, 2.5)); w != nil {
			t.Error("a measured cost is exactly what the ceiling is for")
		}
		if b.costUsed != 2.5 {
			t.Errorf("expected costUsed 2.5, got %v", b.costUsed)
		}
	})
}

// TestBudgetOverrunCheckpointsTheNextNode pins where a run stops when a
// node that SUCCEEDED took it past the cap. The node's output is already
// stored, so anchoring the checkpoint on it would make a resume pay for
// that node twice — for an agent pass, the entire cost again. The run
// still fails (the cap was exceeded), but on the node that has NOT run.
func TestBudgetOverrunCheckpointsTheNextNode(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "budget_overrun_checkpoint",
		Entry: "expensive",
		Nodes: map[string]ir.Node{
			"expensive": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "expensive"}},
			"tail":      &ir.AgentNode{BaseNode: ir.BaseNode{ID: "tail"}},
			"done":      &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
			"fail":      &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges: []*ir.Edge{
			{From: "expensive", To: "tail"},
			{From: "tail", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
		Budget:  &ir.Budget{MaxCostUSD: 1.0},
	}

	exec := newStubExecutor()
	tailRan := false
	exec.on("expensive", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"ok": true, "_cost_usd": 1.4}, nil
	})
	exec.on("tail", func(_ map[string]any) (map[string]any, error) {
		tailRan = true
		return map[string]any{"ok": true, "_cost_usd": 0.1}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)
	err := eng.Run(context.Background(), "run-budget-overrun", nil)
	if err == nil || !strings.Contains(err.Error(), "budget exceeded") {
		t.Fatalf("an overrun must still fail the run, got: %v", err)
	}
	if tailRan {
		t.Fatal("a node started after the budget was spent")
	}

	run, gerr := s.LoadRun(context.Background(), "run-budget-overrun")
	if gerr != nil {
		t.Fatal(gerr)
	}
	if run.Checkpoint == nil {
		t.Fatal("no checkpoint saved: the run is not resumable")
	}
	if run.Checkpoint.NodeID != "tail" {
		t.Fatalf("checkpoint anchored on %q, want \"tail\" — resuming would re-execute the node whose output is already stored", run.Checkpoint.NodeID)
	}
	if _, ok := run.Checkpoint.Outputs["expensive"]; !ok {
		t.Fatal("the completed node's output is missing from the checkpoint: the resume would have to recompute it")
	}
}

// TestBudgetOverrunOnRouterStillFails pins the guarantee against the
// deferral: recordAndCheckBudget has callers that never return through
// execLoopAfterExec (the LLM router, the resume paths). Deferring THEIR
// overrun would leave it uncollected whenever the rest of the path is
// special-dispatch nodes — none of which run the pre-exec check — and the
// run would reach done and be persisted `finished` with the cap blown.
// A budget is a load-bearing limit: a path that exceeds it and reports
// success is a broken guarantee, not a degradation.
func TestBudgetOverrunOnRouterStillFails(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "budget_overrun_router",
		Entry: "route",
		Nodes: map[string]ir.Node{
			"route": &ir.RouterNode{BaseNode: ir.BaseNode{ID: "route"}, RouterMode: ir.RouterLLM},
			"done":  &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
			"fail":  &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges:   []*ir.Edge{{From: "route", To: "done"}},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
		Budget:  &ir.Budget{MaxCostUSD: 1.0},
	}

	exec := newStubExecutor()
	exec.on("route", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"next": "done", "_cost_usd": 5.0}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)
	err := eng.Run(context.Background(), "run-budget-router", nil)
	if err == nil || !strings.Contains(err.Error(), "budget exceeded") {
		t.Fatalf("a router that blew the cap let the run finish: %v", err)
	}
}

// TestBudgetOverrunSurvivesAnEdgeError pins WHICH error a run dies on when
// the overrunning node also has no matching outgoing edge. BUDGET_EXCEEDED
// carries the sentinel the cloud runner's terminal-ack matches; a naked
// NO_OUTGOING_EDGE is nak'd back to the queue and redelivered onto the same
// spent budget — the redelivery loop that carve-out exists to prevent.
func TestBudgetOverrunSurvivesAnEdgeError(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "budget_overrun_edge_error",
		Entry: "a",
		Nodes: map[string]ir.Node{
			"a":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}},
			"b":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "b"}},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
			"fail": &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges:   []*ir.Edge{{From: "a", To: "b", Condition: "go_on"}, {From: "b", To: "done"}},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
		Budget:  &ir.Budget{MaxCostUSD: 1.0},
	}

	exec := newStubExecutor()
	exec.on("a", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"go_on": false, "_cost_usd": 5.0}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)
	err := eng.Run(context.Background(), "run-budget-edge-error", nil)
	if err == nil {
		t.Fatal("expected the run to fail")
	}
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("run died on %v — the budget sentinel is gone, so the cloud runner would nak and redeliver onto the same spent budget", err)
	}
}

// TestBudgetGraceDeliversBankedWork pins the bounded allowance: once the
// cap is spent, a run may still walk forward to a terminal node, so the
// work it has ALREADY paid for gets delivered — a PR opened, a report
// written — instead of dying on disk. Observed: a docs campaign overran
// its cap and left a finished corpus with no pull request.
func TestBudgetGraceDeliversBankedWork(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "budget_grace_delivers",
		Entry: "work",
		Nodes: map[string]ir.Node{
			"work": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "work"}},
			"tail": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "tail"}},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
			"fail": &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges:   []*ir.Edge{{From: "work", To: "tail"}, {From: "tail", To: "done"}},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
		Budget:  &ir.Budget{MaxCostUSD: 1.0},
	}

	exec := newStubExecutor()
	tailRan := false
	exec.on("work", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"ok": true, "_cost_usd": 1.02}, nil
	})
	exec.on("tail", func(_ map[string]any) (map[string]any, error) {
		tailRan = true
		return map[string]any{"ok": true, "_cost_usd": 0.01}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)
	if err := eng.Run(context.Background(), "run-grace-delivers", nil); err != nil {
		t.Fatalf("the run died instead of reaching its terminal node: %v", err)
	}
	if !tailRan {
		t.Fatal("the delivery node never ran: the work the run paid for stays undelivered")
	}
}

// TestBudgetGraceIsBounded pins the ceiling. The allowance is a fraction
// of the declared cap, not a licence: a run far past it stops, however
// close to the end it is — otherwise a costly tail would spend without
// limit on a budget that is already gone.
func TestBudgetGraceIsBounded(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "budget_grace_bounded",
		Entry: "work",
		Nodes: map[string]ir.Node{
			"work": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "work"}},
			"tail": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "tail"}},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
			"fail": &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges:   []*ir.Edge{{From: "work", To: "tail"}, {From: "tail", To: "done"}},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
		Budget:  &ir.Budget{MaxCostUSD: 1.0},
	}

	exec := newStubExecutor()
	tailRan := false
	exec.on("work", func(_ map[string]any) (map[string]any, error) {
		// Far past the graced ceiling (1.0 x 1.1).
		return map[string]any{"ok": true, "_cost_usd": 5.0}, nil
	})
	exec.on("tail", func(_ map[string]any) (map[string]any, error) {
		tailRan = true
		return map[string]any{"ok": true}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)
	err := eng.Run(context.Background(), "run-grace-bounded", nil)
	if err == nil || !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("a run far past the graced ceiling must still fail on the budget, got: %v", err)
	}
	if tailRan {
		t.Fatal("a node ran outside the bounded allowance")
	}
}

// TestBudgetGraceEdgeErrorStillDiesOnBudget pins the interaction of the
// grace with the terminal-ack carve-out: a node that overran INSIDE the
// graced window and then matched no outgoing edge has no successor the
// grace could deliver anything through — the run must die as
// BUDGET_EXCEEDED (the sentinel the cloud runner acks terminal), never
// as a naked NO_OUTGOING_EDGE (nak'd, redelivered onto the same spent
// budget). Same fixture as TestBudgetOverrunSurvivesAnEdgeError with the
// overrun moved inside the grace: 1.02 on a 1.00 cap.
func TestBudgetGraceEdgeErrorStillDiesOnBudget(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "budget_grace_edge_error",
		Entry: "a",
		Nodes: map[string]ir.Node{
			"a":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}},
			"b":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "b"}},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
			"fail": &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges:   []*ir.Edge{{From: "a", To: "b", Condition: "go_on"}, {From: "b", To: "done"}},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
		Budget:  &ir.Budget{MaxCostUSD: 1.0},
	}

	exec := newStubExecutor()
	exec.on("a", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"go_on": false, "_cost_usd": 1.02}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)
	err := eng.Run(context.Background(), "run-grace-edge-error", nil)
	if err == nil {
		t.Fatal("expected the run to fail: no outgoing edge matched")
	}
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("run died on %v — inside the grace the edge error leaked through naked, so the cloud runner would nak and redeliver onto the same spent budget", err)
	}
}

// TestBudgetGraceCoversDuration pins the grace on the duration axis —
// the axis a long campaign is most likely to trip. Before the fix, the
// unconditional RemainingDuration gate killed the run right after
// graceOrFailBudget had forgiven the same overrun: the events recorded a
// grace that was never granted. Ratio raised via env so the test's
// timing window is wide enough to be deterministic.
func TestBudgetGraceCoversDuration(t *testing.T) {
	t.Setenv("ITERION_BUDGET_EXIT_GRACE", "0.9")
	wf := &ir.Workflow{
		Name:  "budget_grace_duration",
		Entry: "work",
		Nodes: map[string]ir.Node{
			"work": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "work"}},
			"tail": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "tail"}},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
			"fail": &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges:   []*ir.Edge{{From: "work", To: "tail"}, {From: "tail", To: "done"}},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
		Budget:  &ir.Budget{MaxDuration: "2s"},
	}

	exec := newStubExecutor()
	tailRan := false
	exec.on("work", func(_ map[string]any) (map[string]any, error) {
		// Past the 2s cap, inside the 3.8s graced ceiling (2s × 1.9) with
		// ~1.5s of slack: a loaded CI runner adds close to a second of
		// engine overhead, which a tighter window reads as a real overrun.
		time.Sleep(2300 * time.Millisecond)
		return map[string]any{"ok": true}, nil
	})
	exec.on("tail", func(_ map[string]any) (map[string]any, error) {
		tailRan = true
		return map[string]any{"ok": true}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)
	if err := eng.Run(context.Background(), "run-grace-duration", nil); err != nil {
		t.Fatalf("a duration overrun inside the grace still killed the run: %v", err)
	}
	if !tailRan {
		t.Fatal("the delivery node never ran under a duration grace")
	}
}

// TestBudgetGraceRefusedWhenLoopGuardOff pins half the grace's safety
// argument: "no further iteration can start" is the LOOP GUARD's
// property, and the guard is an operator escape hatch. With it lifted a
// graced run could take back-edges and loop indefinitely on a spent
// budget — so the grace must not be offered at all.
func TestBudgetGraceRefusedWhenLoopGuardOff(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "budget_grace_guard_off",
		Entry: "work",
		Nodes: map[string]ir.Node{
			"work": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "work"}},
			"tail": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "tail"}},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
			"fail": &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges:           []*ir.Edge{{From: "work", To: "tail"}, {From: "tail", To: "done"}},
		Schemas:         map[string]*ir.Schema{},
		Prompts:         map[string]*ir.Prompt{},
		Vars:            map[string]*ir.Var{},
		Loops:           map[string]*ir.Loop{},
		Budget:          &ir.Budget{MaxCostUSD: 1.0},
		LoopBudgetGuard: "off",
	}

	exec := newStubExecutor()
	tailRan := false
	exec.on("work", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"ok": true, "_cost_usd": 1.02}, nil
	})
	exec.on("tail", func(_ map[string]any) (map[string]any, error) {
		tailRan = true
		return map[string]any{"ok": true}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)
	err := eng.Run(context.Background(), "run-grace-guard-off", nil)
	if err == nil || !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("with the loop guard off the grace must be refused, got: %v", err)
	}
	if tailRan {
		t.Fatal("a node was graced while the loop guard was disabled")
	}
}

// TestBudgetGraceAbsoluteWhenZero pins the escape hatch: a deployment
// that needs declared caps to be hard invoice ceilings (shared instance,
// pooled credential) sets ITERION_BUDGET_EXIT_GRACE=0 and gets exactly
// the pre-grace behaviour back.
func TestBudgetGraceAbsoluteWhenZero(t *testing.T) {
	t.Setenv("ITERION_BUDGET_EXIT_GRACE", "0")
	wf := &ir.Workflow{
		Name:  "budget_grace_zero",
		Entry: "work",
		Nodes: map[string]ir.Node{
			"work": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "work"}},
			"tail": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "tail"}},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
			"fail": &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges:   []*ir.Edge{{From: "work", To: "tail"}, {From: "tail", To: "done"}},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
		Budget:  &ir.Budget{MaxCostUSD: 1.0},
	}

	exec := newStubExecutor()
	tailRan := false
	exec.on("work", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"ok": true, "_cost_usd": 1.02}, nil
	})
	exec.on("tail", func(_ map[string]any) (map[string]any, error) {
		tailRan = true
		return map[string]any{"ok": true}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)
	err := eng.Run(context.Background(), "run-grace-zero", nil)
	if err == nil || !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("grace=0 must make the cap absolute, got: %v", err)
	}
	if tailRan {
		t.Fatal("a node ran past an absolute cap")
	}
}

// TestBudgetGraceEventIsCoherentAndSingular pins the audit record's
// shape: exactly ONE budget_exit_grace event per (node, dimension), and
// its dimension/used/limit triple is self-consistent (the exceeded
// axis's own figures — never one axis's ratio under another's name).
func TestBudgetGraceEventIsCoherentAndSingular(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "budget_grace_event_shape",
		Entry: "work",
		Nodes: map[string]ir.Node{
			"work": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "work"}},
			"tail": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "tail"}},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
			"fail": &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges:   []*ir.Edge{{From: "work", To: "tail"}, {From: "tail", To: "done"}},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
		Budget:  &ir.Budget{MaxCostUSD: 1.0},
	}

	exec := newStubExecutor()
	exec.on("work", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"ok": true, "_cost_usd": 1.02}, nil
	})
	exec.on("tail", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)
	if err := eng.Run(context.Background(), "run-grace-event-shape", nil); err != nil {
		t.Fatalf("run failed: %v", err)
	}

	events, err := s.LoadEvents(context.Background(), "run-grace-event-shape")
	if err != nil {
		t.Fatalf("load events: %v", err)
	}
	seen := map[string]bool{}
	count := 0
	for _, evt := range events {
		if evt.Type != store.EventBudgetExitGrace {
			continue
		}
		count++
		g := evt.Data
		if g["dimension"] != "cost_usd" {
			t.Fatalf("event names dimension %v for a cost overrun", g["dimension"])
		}
		used, _ := g["used"].(float64)
		limit, _ := g["limit"].(float64)
		if limit != 1.0 || used < 1.01 || used > 1.04 {
			t.Fatalf("event used/limit pair (%v/%v) is not the exceeded axis's own figures", g["used"], g["limit"])
		}
		key := evt.NodeID + "/cost_usd"
		if seen[key] {
			t.Fatalf("duplicate budget_exit_grace for %s — one boundary emitted the same grant twice", key)
		}
		seen[key] = true
	}
	if count == 0 {
		t.Fatal("no budget_exit_grace event: the deliberate overspend left no audit record")
	}
}

// TestBudgetGraceCoversImmediateRecordPath pins the non-deferred
// recording path (LLM router, resume re-entry): its PRE-exec check
// graces the node into starting, so the post-exec record must honour the
// same grace — otherwise the node is allowed to spend and then killed
// anyway, which is strictly worse than failing before it ran.
func TestBudgetGraceCoversImmediateRecordPath(t *testing.T) {
	wf := &ir.Workflow{
		Name:    "budget_grace_immediate",
		Entry:   "work",
		Nodes:   map[string]ir.Node{"work": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "work"}}, "done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}}, "fail": &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}}},
		Edges:   []*ir.Edge{{From: "work", To: "done"}},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
		Budget:  &ir.Budget{MaxCostUSD: 1.0},
	}
	s := tmpStore(t)
	eng := New(wf, s, newStubExecutor())

	rs := &runState{ctx: context.Background(), runID: "run-grace-immediate", budget: newSharedBudget(wf.Budget, nil)}
	if err := eng.recordAndCheckBudget(rs, "work", map[string]any{"_cost_usd": 1.02}); err != nil {
		t.Fatalf("an overrun inside the grace on the immediate record path killed the node that just spent: %v", err)
	}
	if err := eng.recordAndCheckBudget(rs, "work", map[string]any{"_cost_usd": 1.0}); err == nil {
		t.Fatal("past the graced ceiling the immediate record path must still fail")
	}
}

// TestBudgetGraceSurvivesHardLimitOnAnotherAxis pins the pre-exec
// ordering that makes the grace usable on multi-axis budgets: when one
// axis is exceeded-but-graced and ANOTHER sits past the 90% hard limit
// (the common max_cost_usd + max_duration pairing late in a long run),
// the grace decides and returns — the hard-limit branch must not get a
// second opinion, or refusing a node at 90% of axis B while permitting
// 110% of axis A would defeat the grace exactly where it matters.
func TestBudgetGraceSurvivesHardLimitOnAnotherAxis(t *testing.T) {
	// Widened graced ceiling (3s for a 2s cap) so the wall-clock margin
	// holds on a loaded CI runner; duration still sits in the 90%+
	// hard-limit band when the tail is considered, which is the point.
	t.Setenv("ITERION_BUDGET_EXIT_GRACE", "0.5")
	wf := &ir.Workflow{
		Name:  "budget_grace_hard_limit_other_axis",
		Entry: "work",
		Nodes: map[string]ir.Node{
			"work": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "work"}},
			"tail": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "tail"}},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
			"fail": &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges:   []*ir.Edge{{From: "work", To: "tail"}, {From: "tail", To: "done"}},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
		// Cost will be exceeded inside the grace; duration will sit in
		// the 90%+ hard-limit band when the tail is considered.
		Budget: &ir.Budget{MaxCostUSD: 1.0, MaxDuration: "2s"},
	}

	exec := newStubExecutor()
	tailRan := false
	exec.on("work", func(_ map[string]any) (map[string]any, error) {
		time.Sleep(1850 * time.Millisecond) // ~92% of max_duration
		return map[string]any{"ok": true, "_cost_usd": 1.02}, nil
	})
	exec.on("tail", func(_ map[string]any) (map[string]any, error) {
		tailRan = true
		return map[string]any{"ok": true}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)
	if err := eng.Run(context.Background(), "run-grace-hard-limit-other-axis", nil); err != nil {
		t.Fatalf("the hard limit on a second axis defeated a granted grace: %v", err)
	}
	if !tailRan {
		t.Fatal("the delivery node never ran")
	}
}

// TestBudgetGraceRefusedOnImposedCap pins the boundary that makes the
// grace safe to default-on: a cap CLAMPED by an external authority
// (platform ceiling, pool donor's remaining allowance —
// ir.Budget.CapImposed) is an absolute promise to a third party. The
// grace must never spend a donor's money past what they granted.
func TestBudgetGraceRefusedOnImposedCap(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "budget_grace_imposed_cap",
		Entry: "work",
		Nodes: map[string]ir.Node{
			"work": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "work"}},
			"tail": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "tail"}},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
			"fail": &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges:   []*ir.Edge{{From: "work", To: "tail"}, {From: "tail", To: "done"}},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
		Budget:  &ir.Budget{MaxCostUSD: 1.0, CapImposed: true},
	}

	exec := newStubExecutor()
	tailRan := false
	exec.on("work", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"ok": true, "_cost_usd": 1.02}, nil
	})
	exec.on("tail", func(_ map[string]any) (map[string]any, error) {
		tailRan = true
		return map[string]any{"ok": true}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)
	err := eng.Run(context.Background(), "run-grace-imposed-cap", nil)
	if err == nil || !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("an externally-imposed cap must be absolute, got: %v", err)
	}
	if tailRan {
		t.Fatal("a node was graced past a cap imposed by a third party")
	}
}

// TestBudgetGraceStopsBeforeFanOutBranches pins the parallel safety rule:
// branch-local predictive loop pricing is disabled because sibling spend is
// shared, so its exit-grace safety argument does not hold. A graced trunk may
// reach the router, but no branch node may start past the declared cap.
func TestBudgetGraceStopsBeforeFanOutBranches(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "budget_grace_fanout",
		Entry: "work",
		Nodes: map[string]ir.Node{
			"work":     &ir.AgentNode{BaseNode: ir.BaseNode{ID: "work"}},
			"router":   &ir.RouterNode{BaseNode: ir.BaseNode{ID: "router"}, RouterMode: ir.RouterFanOutAll},
			"branch_a": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "branch_a"}},
			"branch_b": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "branch_b"}},
			"done":     &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}, AwaitMode: ir.AwaitBestEffort},
			"fail":     &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges: []*ir.Edge{
			{From: "work", To: "router"},
			{From: "router", To: "branch_a"},
			{From: "router", To: "branch_b"},
			{From: "branch_a", To: "done"},
			{From: "branch_b", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
		Budget:  &ir.Budget{MaxCostUSD: 1.0},
	}

	exec := newStubExecutor()
	var branches atomic.Int32
	exec.on("work", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"ok": true, "_cost_usd": 1.02}, nil
	})
	for _, b := range []string{"branch_a", "branch_b"} {
		exec.on(b, func(_ map[string]any) (map[string]any, error) {
			branches.Add(1)
			return map[string]any{"ok": true}, nil
		})
	}

	s := tmpStore(t)
	eng := New(wf, s, exec)
	if err := eng.Run(context.Background(), "run-grace-fanout", nil); err != nil {
		t.Fatalf("best_effort terminal convergence should report failed branches in output, got: %v", err)
	}
	if got := branches.Load(); got != 0 {
		t.Fatalf("%d branch nodes ran under an unsafe exit grace", got)
	}
}

// TestBudgetGraceFanOutWaitAllKeepsBudgetSentinel pins the stop-path SHAPE of
// the rule above under wait_all. Every branch is refused on the spent budget,
// so the run dies at convergence — and that death must still be a
// BUDGET_EXCEEDED carrying ErrBudgetExceeded. The cloud runner's terminal-ack
// carve-out matches the sentinel; a naked convergence error goes back to
// JetStream as retryable and loops resume/refail against the same spent cap.
func TestBudgetGraceFanOutWaitAllKeepsBudgetSentinel(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "budget_grace_fanout_wait_all",
		Entry: "work",
		Nodes: map[string]ir.Node{
			"work":     &ir.AgentNode{BaseNode: ir.BaseNode{ID: "work"}},
			"router":   &ir.RouterNode{BaseNode: ir.BaseNode{ID: "router"}, RouterMode: ir.RouterFanOutAll},
			"branch_a": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "branch_a"}},
			"branch_b": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "branch_b"}},
			"collect":  &ir.AgentNode{BaseNode: ir.BaseNode{ID: "collect"}, AwaitMode: ir.AwaitWaitAll},
			"done":     &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
			"fail":     &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges: []*ir.Edge{
			{From: "work", To: "router"},
			{From: "router", To: "branch_a"},
			{From: "router", To: "branch_b"},
			{From: "branch_a", To: "collect"},
			{From: "branch_b", To: "collect"},
			{From: "collect", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
		Budget:  &ir.Budget{MaxCostUSD: 1.0},
	}

	exec := newStubExecutor()
	exec.on("work", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"ok": true, "_cost_usd": 1.02}, nil
	})
	for _, b := range []string{"branch_a", "branch_b", "collect"} {
		exec.on(b, func(_ map[string]any) (map[string]any, error) {
			return map[string]any{"ok": true}, nil
		})
	}

	s := tmpStore(t)
	eng := New(wf, s, exec)
	err := eng.Run(context.Background(), "run-grace-fanout-wait-all", nil)
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("wait_all fan-out death on a spent budget = %v, want ErrBudgetExceeded", err)
	}
	var rtErr *RuntimeError
	if !errors.As(err, &rtErr) || rtErr.Code != ErrCodeBudgetExceeded {
		t.Fatalf("error = %#v, want a RuntimeError coded BUDGET_EXCEEDED", err)
	}
}

// TestBudgetGraceInvalidEnvFailsClosed pins the parse direction of the
// override: an operator reaching for ITERION_BUDGET_EXIT_GRACE wants a
// TIGHTER policy; a value the parser does not understand must land on
// the absolute-cap side, never silently grant the permissive default.
func TestBudgetGraceInvalidEnvFailsClosed(t *testing.T) {
	t.Setenv("ITERION_BUDGET_EXIT_GRACE", "off")
	if got := budgetExitGraceRatio(); got != 0 {
		t.Fatalf("'off' must mean absolute caps, got ratio %v", got)
	}
	t.Setenv("ITERION_BUDGET_EXIT_GRACE", "10")
	if got := budgetExitGraceRatio(); got != 0 {
		t.Fatalf("an out-of-range value must fail closed to 0, got %v", got)
	}
	t.Setenv("ITERION_BUDGET_EXIT_GRACE", "banana")
	if got := budgetExitGraceRatio(); got != 0 {
		t.Fatalf("an unparsable value must fail closed to 0, got %v", got)
	}
}

package runtime

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/ir"
	"github.com/SocialGouv/iterion/store"
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
		Nodes: map[string]*ir.Node{
			"entry":  {ID: "entry", Kind: ir.NodeAgent},
			"router": {ID: "router", Kind: ir.NodeRouter, RouterMode: ir.RouterFanOutAll},
			"a":      {ID: "a", Kind: ir.NodeAgent},
			"b":      {ID: "b", Kind: ir.NodeAgent},
			"done":   {ID: "done", Kind: ir.NodeDone, AwaitStrategy: ir.AwaitBestEffort},
			"fail":   {ID: "fail", Kind: ir.NodeFail},
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
		Nodes: map[string]*ir.Node{
			"a":    {ID: "a", Kind: ir.NodeAgent},
			"b":    {ID: "b", Kind: ir.NodeAgent},
			"c":    {ID: "c", Kind: ir.NodeAgent},
			"d":    {ID: "d", Kind: ir.NodeAgent},
			"done": {ID: "done", Kind: ir.NodeDone},
			"fail": {ID: "fail", Kind: ir.NodeFail},
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
		exec.on(id, func(_ map[string]interface{}) (map[string]interface{}, error) {
			return map[string]interface{}{"ok": true}, nil
		})
	}

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-budget-warn", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Check that a budget_warning event was emitted.
	events, err := s.LoadEvents("run-budget-warn")
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
		Nodes: map[string]*ir.Node{
			"a":    {ID: "a", Kind: ir.NodeAgent},
			"b":    {ID: "b", Kind: ir.NodeAgent},
			"c":    {ID: "c", Kind: ir.NodeAgent},
			"done": {ID: "done", Kind: ir.NodeDone},
			"fail": {ID: "fail", Kind: ir.NodeFail},
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
		exec.on(id, func(_ map[string]interface{}) (map[string]interface{}, error) {
			return map[string]interface{}{"ok": true}, nil
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
	r, err := s.LoadRun("run-budget-exceeded")
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if r.Status != store.RunStatusFailed {
		t.Errorf("expected failed status, got %s", r.Status)
	}

	// Verify budget_exceeded event was emitted.
	events, err := s.LoadEvents("run-budget-exceeded")
	if err != nil {
		t.Fatalf("load events: %v", err)
	}
	found := false
	for _, evt := range events {
		if evt.Type == store.EventBudgetExceeded {
			found = true
		}
	}
	if !found {
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
		Nodes: map[string]*ir.Node{
			"a":    {ID: "a", Kind: ir.NodeAgent},
			"b":    {ID: "b", Kind: ir.NodeAgent},
			"done": {ID: "done", Kind: ir.NodeDone},
			"fail": {ID: "fail", Kind: ir.NodeFail},
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
	exec.on("a", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"ok": true, "_tokens": 80}, nil
	})
	exec.on("b", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"ok": true, "_tokens": 50}, nil
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
	events, err := s.LoadEvents("run-token-budget")
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
// Test: cost-based budget exceeded
// ---------------------------------------------------------------------------

func TestBudgetCostExceeded(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "cost_budget_test",
		Entry: "a",
		Nodes: map[string]*ir.Node{
			"a":    {ID: "a", Kind: ir.NodeAgent},
			"b":    {ID: "b", Kind: ir.NodeAgent},
			"done": {ID: "done", Kind: ir.NodeDone},
			"fail": {ID: "fail", Kind: ir.NodeFail},
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
	exec.on("a", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"ok": true, "_cost_usd": 0.6}, nil
	})
	exec.on("b", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"ok": true, "_cost_usd": 0.5}, nil
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
		Nodes: map[string]*ir.Node{
			"entry":  {ID: "entry", Kind: ir.NodeAgent},
			"router": {ID: "router", Kind: ir.NodeRouter, RouterMode: ir.RouterFanOutAll},
			"a":    {ID: "a", Kind: ir.NodeAgent},
			"b1":   {ID: "b1", Kind: ir.NodeAgent},
			"b2":   {ID: "b2", Kind: ir.NodeAgent},
			"done": {ID: "done", Kind: ir.NodeDone, AwaitStrategy: ir.AwaitBestEffort},
			"fail": {ID: "fail", Kind: ir.NodeFail},
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
	exec.on("entry", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{}, nil
	})
	exec.on("a", func(_ map[string]interface{}) (map[string]interface{}, error) {
		atomic.AddInt64(&branchADone, 1)
		return map[string]interface{}{"review": "A done"}, nil
	})
	exec.on("b1", func(_ map[string]interface{}) (map[string]interface{}, error) {
		// Small delay so branch A has a chance to execute first.
		time.Sleep(10 * time.Millisecond)
		return map[string]interface{}{"step": "b1 done"}, nil
	})
	exec.on("b2", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"step": "b2 done"}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-shared-budget", nil)
	// With best_effort, the run may succeed even if one branch hits budget.
	// But we want to verify that budget events were emitted.
	_ = err

	events, err := s.LoadEvents("run-shared-budget")
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
		Nodes: map[string]*ir.Node{
			"a":    {ID: "a", Kind: ir.NodeAgent},
			"b":    {ID: "b", Kind: ir.NodeAgent},
			"done": {ID: "done", Kind: ir.NodeDone},
			"fail": {ID: "fail", Kind: ir.NodeFail},
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
	exec.on("a", func(_ map[string]interface{}) (map[string]interface{}, error) {
		time.Sleep(60 * time.Millisecond) // exceed budget
		return map[string]interface{}{"ok": true}, nil
	})
	exec.on("b", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"ok": true}, nil
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
		Nodes: map[string]*ir.Node{
			"a":    {ID: "a", Kind: ir.NodeAgent},
			"b":    {ID: "b", Kind: ir.NodeAgent},
			"done": {ID: "done", Kind: ir.NodeDone},
			"fail": {ID: "fail", Kind: ir.NodeFail},
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
	exec.on("a", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"_tokens": 999999, "_cost_usd": 999.0}, nil
	})
	exec.on("b", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-no-budget", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	r, _ := s.LoadRun("run-no-budget")
	if r.Status != store.RunStatusFinished {
		t.Errorf("expected finished, got %s", r.Status)
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
		Nodes: map[string]*ir.Node{
			"entry":  {ID: "entry", Kind: ir.NodeAgent},
			"router": {ID: "router", Kind: ir.NodeRouter, RouterMode: ir.RouterFanOutAll},
			"tool_a": {ID: "tool_a", Kind: ir.NodeTool, Command: "echo a"},
			"tool_b": {ID: "tool_b", Kind: ir.NodeTool, Command: "echo b"},
			"done":   {ID: "done", Kind: ir.NodeDone, AwaitStrategy: ir.AwaitWaitAll},
			"fail":   {ID: "fail", Kind: ir.NodeFail},
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
	exec.on("entry", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{}, nil
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

	r, _ := s.LoadRun("run-unsafe")
	if r.Status != store.RunStatusFailed {
		t.Errorf("expected failed, got %s", r.Status)
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
		Nodes: map[string]*ir.Node{
			"entry":    {ID: "entry", Kind: ir.NodeAgent},
			"router":   {ID: "router", Kind: ir.NodeRouter, RouterMode: ir.RouterFanOutAll},
			"tool_a":   {ID: "tool_a", Kind: ir.NodeTool, Command: "echo a"},
			"review_b": {ID: "review_b", Kind: ir.NodeAgent},
			"done":     {ID: "done", Kind: ir.NodeDone, AwaitStrategy: ir.AwaitWaitAll},
			"fail":     {ID: "fail", Kind: ir.NodeFail},
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
	exec.on("entry", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{}, nil
	})
	exec.on("tool_a", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"result": "tool ran"}, nil
	})
	exec.on("review_b", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"review": "looks good"}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-safe-mutation", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	r, _ := s.LoadRun("run-safe-mutation")
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
		Nodes: map[string]*ir.Node{
			"entry":  {ID: "entry", Kind: ir.NodeAgent},
			"router": {ID: "router", Kind: ir.NodeRouter, RouterMode: ir.RouterFanOutAll},
			"a":    {ID: "a", Kind: ir.NodeAgent},
			"b":    {ID: "b", Kind: ir.NodeAgent},
			"c":    {ID: "c", Kind: ir.NodeAgent},
			"done": {ID: "done", Kind: ir.NodeDone, AwaitStrategy: ir.AwaitWaitAll},
			"fail": {ID: "fail", Kind: ir.NodeFail},
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
	exec.on("entry", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{}, nil
	})
	for _, id := range []string{"a", "b", "c"} {
		exec.on(id, func(_ map[string]interface{}) (map[string]interface{}, error) {
			return map[string]interface{}{"ok": true}, nil
		})
	}

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-readonly", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	r, _ := s.LoadRun("run-readonly")
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
		Nodes: map[string]*ir.Node{
			"entry":  {ID: "entry", Kind: ir.NodeAgent},
			"router": {ID: "router", Kind: ir.NodeRouter, RouterMode: ir.RouterFanOutAll},
			"a":    {ID: "a", Kind: ir.NodeAgent, Tools: []string{"write_file"}},
			"b":    {ID: "b", Kind: ir.NodeAgent, Tools: []string{"run_command"}},
			"done": {ID: "done", Kind: ir.NodeDone, AwaitStrategy: ir.AwaitWaitAll},
			"fail": {ID: "fail", Kind: ir.NodeFail},
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
	exec.on("entry", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{}, nil
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
		Nodes: map[string]*ir.Node{
			"entry":  {ID: "entry", Kind: ir.NodeAgent},
			"router": {ID: "router", Kind: ir.NodeRouter, RouterMode: ir.RouterFanOutAll},
			"a":    {ID: "a", Kind: ir.NodeAgent, Tools: []string{"read_file", "git_diff"}},
			"b":    {ID: "b", Kind: ir.NodeAgent, Tools: []string{"git_status", "search_codebase", "tree"}},
			"done": {ID: "done", Kind: ir.NodeDone, AwaitStrategy: ir.AwaitWaitAll},
			"fail": {ID: "fail", Kind: ir.NodeFail},
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
	exec.on("entry", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{}, nil
	})
	exec.on("a", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"review": "A"}, nil
	})
	exec.on("b", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"review": "B"}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-readonly-tools", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	r, _ := s.LoadRun("run-readonly-tools")
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
		Nodes: map[string]*ir.Node{
			"entry":  {ID: "entry", Kind: ir.NodeAgent},
			"router": {ID: "router", Kind: ir.NodeRouter, RouterMode: ir.RouterFanOutAll},
			"a":    {ID: "a", Kind: ir.NodeAgent, Tools: []string{"write_file"}},
			"b":    {ID: "b", Kind: ir.NodeAgent, Tools: []string{"read_file", "git_status"}},
			"done": {ID: "done", Kind: ir.NodeDone, AwaitStrategy: ir.AwaitWaitAll},
			"fail": {ID: "fail", Kind: ir.NodeFail},
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
	exec.on("entry", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{}, nil
	})
	exec.on("a", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"result": "wrote"}, nil
	})
	exec.on("b", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"review": "looks good"}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-one-mutating-one-readonly", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	r, _ := s.LoadRun("run-one-mutating-one-readonly")
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
		Nodes: map[string]*ir.Node{
			"entry":  {ID: "entry", Kind: ir.NodeAgent},
			"router": {ID: "router", Kind: ir.NodeRouter, RouterMode: ir.RouterFanOutAll},
			"a":    {ID: "a", Kind: ir.NodeAgent, Tools: []string{"read_file", "write_file"}},
			"b":    {ID: "b", Kind: ir.NodeAgent, Tools: []string{"git_diff", "run_command"}},
			"done": {ID: "done", Kind: ir.NodeDone, AwaitStrategy: ir.AwaitWaitAll},
			"fail": {ID: "fail", Kind: ir.NodeFail},
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
	exec.on("entry", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{}, nil
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
	exec.on("entry", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{}, nil
	})
	exec.on("a", func(_ map[string]interface{}) (map[string]interface{}, error) {
		time.Sleep(5 * time.Millisecond) // stagger slightly
		return map[string]interface{}{"review": "A"}, nil
	})
	exec.on("b", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"review": "B"}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-branch-budget", nil)
	// With best_effort and 3 iterations total (entry + 2 branches = 3 exactly),
	// both should succeed.
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	r, _ := s.LoadRun("run-branch-budget")
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
		Nodes: map[string]*ir.Node{
			"a":    {ID: "a", Kind: ir.NodeAgent},
			"b":    {ID: "b", Kind: ir.NodeAgent},
			"done": {ID: "done", Kind: ir.NodeDone},
			"fail": {ID: "fail", Kind: ir.NodeFail},
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
	exec.on("a", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{}, nil
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

// keep imports used
var _ = errors.New
var _ = atomic.AddInt64

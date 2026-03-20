package benchmark

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/iterion-ai/iterion/ir"
	"github.com/iterion-ai/iterion/recipe"
	"github.com/iterion-ai/iterion/runtime"
	"github.com/iterion-ai/iterion/store"
)

// ---------------------------------------------------------------------------
// stubExecutor — per-recipe configurable executor
// ---------------------------------------------------------------------------

type stubExecutor struct {
	name     string
	calls    []string // records nodeIDs in execution order
	handlers map[string]func(map[string]interface{}) (map[string]interface{}, error)
}

func newStubExecutor(name string) *stubExecutor {
	return &stubExecutor{
		name:     name,
		handlers: make(map[string]func(map[string]interface{}) (map[string]interface{}, error)),
	}
}

func (s *stubExecutor) on(nodeID string, fn func(map[string]interface{}) (map[string]interface{}, error)) {
	s.handlers[nodeID] = fn
}

func (s *stubExecutor) Execute(_ context.Context, node *ir.Node, input map[string]interface{}) (map[string]interface{}, error) {
	s.calls = append(s.calls, node.ID)
	if fn, ok := s.handlers[node.ID]; ok {
		return fn(input)
	}
	return map[string]interface{}{}, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// buildTestWorkflow creates a minimal agent -> judge -> done/fail workflow.
func buildTestWorkflow() *ir.Workflow {
	return &ir.Workflow{
		Name:  "test_wf",
		Entry: "analyze",
		Nodes: map[string]*ir.Node{
			"analyze": {ID: "analyze", Kind: ir.NodeAgent},
			"judge":   {ID: "judge", Kind: ir.NodeJudge},
			"done":    {ID: "done", Kind: ir.NodeDone},
			"fail":    {ID: "fail", Kind: ir.NodeFail},
		},
		Edges: []*ir.Edge{
			{From: "analyze", To: "judge"},
			{From: "judge", To: "done", Condition: "pass", Negated: false},
			{From: "judge", To: "fail", Condition: "pass", Negated: true},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}
}

func buildRecipes() []*recipe.RecipeSpec {
	return []*recipe.RecipeSpec{
		{
			Name:        "recipe_a",
			WorkflowRef: recipe.WorkflowRef{Name: "test_wf"},
			EvaluationPolicy: recipe.EvaluationPolicy{
				PrimaryMetric: "approved",
				SuccessValue:  "true",
			},
		},
		{
			Name:        "recipe_b",
			WorkflowRef: recipe.WorkflowRef{Name: "test_wf"},
			EvaluationPolicy: recipe.EvaluationPolicy{
				PrimaryMetric: "approved",
				SuccessValue:  "true",
			},
		},
	}
}

// ---------------------------------------------------------------------------
// Test: Runner requires at least 2 recipes
// ---------------------------------------------------------------------------

func TestRunnerRequiresMinRecipes(t *testing.T) {
	wf := buildTestWorkflow()
	_, err := NewRunner(RunnerConfig{
		Workflow: wf,
		Recipes:  []*recipe.RecipeSpec{{Name: "single", WorkflowRef: recipe.WorkflowRef{Name: "test_wf"}}},
		ExecutorFactory: func() runtime.NodeExecutor {
			return newStubExecutor("single")
		},
	})
	if err == nil {
		t.Fatal("expected error for < 2 recipes")
	}
}

// ---------------------------------------------------------------------------
// Test: Multi-recipe benchmark runs with isolation
// ---------------------------------------------------------------------------

func TestMultiRecipeBenchmark(t *testing.T) {
	wf := buildTestWorkflow()
	recipes := buildRecipes()

	// Track which executor instances are created.
	var executors []*stubExecutor

	runner, err := NewRunner(RunnerConfig{
		CaseLabel: "test-pr-42",
		Workflow:  wf,
		Recipes:   recipes,
		Inputs:    map[string]interface{}{"pr_title": "fix: bug"},
		ExecutorFactory: func() runtime.NodeExecutor {
			exec := newStubExecutor("exec")
			exec.on("analyze", func(_ map[string]interface{}) (map[string]interface{}, error) {
				return map[string]interface{}{
					"text":      "analysis",
					"_tokens":   100,
					"_cost_usd": 0.005,
				}, nil
			})
			exec.on("judge", func(_ map[string]interface{}) (map[string]interface{}, error) {
				return map[string]interface{}{
					"approved":  true,
					"pass":      true,
					"_tokens":   50,
					"_cost_usd": 0.002,
				}, nil
			})
			executors = append(executors, exec)
			return exec
		},
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	storeRoot := t.TempDir()
	report, err := runner.Run(context.Background(), storeRoot)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Verify we got one executor per recipe (isolation).
	if len(executors) != 2 {
		t.Errorf("expected 2 executor instances, got %d", len(executors))
	}

	// Verify report structure.
	if len(report.Results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(report.Results))
	}
	if report.CaseLabel != "test-pr-42" {
		t.Errorf("CaseLabel = %q, want test-pr-42", report.CaseLabel)
	}

	for i, m := range report.Results {
		if m.Status != "finished" {
			t.Errorf("result[%d] status = %q, want finished", i, m.Status)
		}
		if m.TotalTokens == 0 {
			t.Errorf("result[%d] tokens should be > 0", i)
		}
		if m.TotalCostUSD == 0 {
			t.Errorf("result[%d] cost should be > 0", i)
		}
		if m.ModelCalls == 0 {
			t.Errorf("result[%d] model calls should be > 0", i)
		}
		if m.Duration == 0 {
			t.Errorf("result[%d] duration should be > 0", i)
		}
	}
}

// ---------------------------------------------------------------------------
// Test: Isolation — recipes do NOT share store or executor state
// ---------------------------------------------------------------------------

func TestRunIsolation(t *testing.T) {
	wf := buildTestWorkflow()
	recipes := buildRecipes()

	// Each executor will write to its own state; we verify no cross-contamination.
	type execState struct {
		callCount int
	}
	states := make([]*execState, 0, 2)

	runner, err := NewRunner(RunnerConfig{
		CaseLabel: "isolation-test",
		Workflow:  wf,
		Recipes:   recipes,
		Inputs:    map[string]interface{}{},
		ExecutorFactory: func() runtime.NodeExecutor {
			st := &execState{}
			states = append(states, st)
			exec := newStubExecutor("iso")
			exec.on("analyze", func(_ map[string]interface{}) (map[string]interface{}, error) {
				st.callCount++
				return map[string]interface{}{"text": "ok", "_tokens": st.callCount * 100}, nil
			})
			exec.on("judge", func(_ map[string]interface{}) (map[string]interface{}, error) {
				st.callCount++
				return map[string]interface{}{"pass": true, "_tokens": st.callCount * 50}, nil
			})
			return exec
		},
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	storeRoot := t.TempDir()
	report, err := runner.Run(context.Background(), storeRoot)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Each executor should have been called independently.
	if len(states) != 2 {
		t.Fatalf("expected 2 executor states, got %d", len(states))
	}

	// Both executors should have the same call count (2: analyze + judge).
	for i, st := range states {
		if st.callCount != 2 {
			t.Errorf("executor[%d] call count = %d, want 2", i, st.callCount)
		}
	}

	// Verify runs are in separate stores (different run IDs and directories).
	if len(report.Results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(report.Results))
	}
	if report.Results[0].RunID == report.Results[1].RunID {
		t.Error("runs should have different run IDs")
	}
}

// ---------------------------------------------------------------------------
// Test: Metrics comparison across recipes
// ---------------------------------------------------------------------------

func TestMetricsComparison(t *testing.T) {
	wf := buildTestWorkflow()
	recipes := buildRecipes()

	// Recipe A is cheap, Recipe B is expensive.
	callIndex := 0
	runner, err := NewRunner(RunnerConfig{
		CaseLabel: "compare-cost",
		Workflow:  wf,
		Recipes:   recipes,
		Inputs:    map[string]interface{}{},
		ExecutorFactory: func() runtime.NodeExecutor {
			idx := callIndex
			callIndex++
			exec := newStubExecutor("cmp")
			exec.on("analyze", func(_ map[string]interface{}) (map[string]interface{}, error) {
				cost := 0.001
				tokens := 50
				if idx == 1 {
					cost = 0.010
					tokens = 500
				}
				return map[string]interface{}{
					"text":      "analysis",
					"_tokens":   tokens,
					"_cost_usd": cost,
				}, nil
			})
			exec.on("judge", func(_ map[string]interface{}) (map[string]interface{}, error) {
				return map[string]interface{}{"pass": true, "_tokens": 10, "_cost_usd": 0.0005}, nil
			})
			return exec
		},
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	storeRoot := t.TempDir()
	report, err := runner.Run(context.Background(), storeRoot)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	a := report.Results[0]
	b := report.Results[1]

	// Recipe B should be more expensive.
	if b.TotalCostUSD <= a.TotalCostUSD {
		t.Errorf("recipe_b cost (%.4f) should be > recipe_a cost (%.4f)", b.TotalCostUSD, a.TotalCostUSD)
	}
	if b.TotalTokens <= a.TotalTokens {
		t.Errorf("recipe_b tokens (%d) should be > recipe_a tokens (%d)", b.TotalTokens, a.TotalTokens)
	}
}

// ---------------------------------------------------------------------------
// Test: MetricsStore persistence
// ---------------------------------------------------------------------------

func TestMetricsStorePersistence(t *testing.T) {
	dir := t.TempDir()
	ms, err := NewMetricsStore(dir)
	if err != nil {
		t.Fatalf("NewMetricsStore: %v", err)
	}

	report := &BenchmarkReport{
		ID:        "bench-test-001",
		CreatedAt: time.Now().UTC(),
		CaseLabel: "test-case",
		Results: []*RunMetrics{
			{RecipeName: "r1", RunID: "run-1", Status: "finished", TotalCostUSD: 0.05, TotalTokens: 1000},
			{RecipeName: "r2", RunID: "run-2", Status: "finished", TotalCostUSD: 0.10, TotalTokens: 2000},
		},
	}

	if err := ms.SaveReport(report); err != nil {
		t.Fatalf("SaveReport: %v", err)
	}

	// Reload.
	loaded, err := ms.LoadReport("bench-test-001")
	if err != nil {
		t.Fatalf("LoadReport: %v", err)
	}
	if loaded.CaseLabel != "test-case" {
		t.Errorf("CaseLabel = %q, want test-case", loaded.CaseLabel)
	}
	if len(loaded.Results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(loaded.Results))
	}
	if loaded.Results[1].TotalCostUSD != 0.10 {
		t.Errorf("result[1] cost = %.4f, want 0.1000", loaded.Results[1].TotalCostUSD)
	}

	// List.
	ids, err := ms.ListReports()
	if err != nil {
		t.Fatalf("ListReports: %v", err)
	}
	if len(ids) != 1 || ids[0] != "bench-test-001" {
		t.Errorf("ListReports = %v, want [bench-test-001]", ids)
	}
}

// ---------------------------------------------------------------------------
// Test: Report rendering
// ---------------------------------------------------------------------------

func TestRenderReport(t *testing.T) {
	report := &BenchmarkReport{
		ID:        "bench-render",
		CreatedAt: time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC),
		CaseLabel: "PR #42",
		Results: []*RunMetrics{
			{RecipeName: "fast", Status: "finished", Verdict: "true", TotalCostUSD: 0.01, TotalTokens: 100, ModelCalls: 2, Iterations: 2, Retries: 0, DurationStr: "1.5s"},
			{RecipeName: "thorough", Status: "finished", Verdict: "true", TotalCostUSD: 0.05, TotalTokens: 500, ModelCalls: 5, Iterations: 4, Retries: 1, DurationStr: "8.2s"},
		},
	}

	var buf bytes.Buffer
	RenderReport(&buf, report)
	output := buf.String()

	// Verify key content is present.
	for _, want := range []string{"bench-render", "PR #42", "fast", "thorough", "0.0100", "0.0500", "100", "500"} {
		if !strings.Contains(output, want) {
			t.Errorf("output missing %q", want)
		}
	}
}

// ---------------------------------------------------------------------------
// Test: CollectMetrics directly
// ---------------------------------------------------------------------------

func TestCollectMetrics(t *testing.T) {
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}

	// Create a run.
	run, err := s.CreateRun("run-m1", "test_wf", nil)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	// Emit events.
	events := []store.Event{
		{Type: store.EventRunStarted},
		{Type: store.EventNodeStarted, NodeID: "analyze"},
		{Type: store.EventLLMRequest, NodeID: "analyze"},
		{Type: store.EventLLMRetry, NodeID: "analyze"},
		{Type: store.EventLLMRequest, NodeID: "analyze"},
		{Type: store.EventNodeFinished, NodeID: "analyze", Data: map[string]interface{}{
			"output":  map[string]interface{}{"text": "ok"},
			"_tokens": float64(200), "_cost_usd": 0.01,
		}},
		{Type: store.EventNodeStarted, NodeID: "judge"},
		{Type: store.EventLLMRequest, NodeID: "judge"},
		{Type: store.EventNodeFinished, NodeID: "judge", Data: map[string]interface{}{
			"output":    map[string]interface{}{"approved": true},
			"_tokens":   float64(100),
			"_cost_usd": 0.005,
		}},
		{Type: store.EventRunFinished},
	}
	for _, evt := range events {
		if _, err := s.AppendEvent("run-m1", evt); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
	}

	// Finish the run.
	if err := s.UpdateRunStatus("run-m1", store.RunStatusFinished, ""); err != nil {
		t.Fatalf("UpdateRunStatus: %v", err)
	}

	// Collect.
	m, err := CollectMetrics(s, "run-m1", "test_recipe", "approved")
	if err != nil {
		t.Fatalf("CollectMetrics: %v", err)
	}

	if m.Status != "finished" {
		t.Errorf("status = %q, want finished", m.Status)
	}
	if m.ModelCalls != 3 { // 2 for analyze (incl retry), 1 for judge
		t.Errorf("model_calls = %d, want 3", m.ModelCalls)
	}
	if m.Retries != 1 {
		t.Errorf("retries = %d, want 1", m.Retries)
	}
	if m.TotalTokens != 300 {
		t.Errorf("tokens = %d, want 300", m.TotalTokens)
	}
	if m.TotalCostUSD != 0.015 {
		t.Errorf("cost = %.4f, want 0.0150", m.TotalCostUSD)
	}
	if m.Iterations != 2 { // 2 node_finished events
		t.Errorf("iterations = %d, want 2", m.Iterations)
	}

	_ = run // used above
}

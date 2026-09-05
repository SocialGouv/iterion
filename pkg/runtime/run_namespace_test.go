package runtime

import (
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// budgetedWorkflow is a one-node workflow carrying every enforceable cap,
// so the run namespace has something to report on each axis.
func budgetedWorkflow() *ir.Workflow {
	return &ir.Workflow{
		Name:  "budgeted",
		Entry: "n",
		Nodes: map[string]ir.Node{
			"n": &ir.ComputeNode{BaseNode: ir.BaseNode{ID: "n"}},
		},
		Budget: &ir.Budget{
			MaxCostUSD:    12.5,
			MaxTokens:     40_000,
			MaxIterations: 7,
			MaxDuration:   "90m",
		},
	}
}

// TestRunNamespaceExposesBudget covers #738: a compute node's expression
// must be able to read the run's consumption and its EFFECTIVE caps, so a
// phase-budget guard stops mirroring the `budget:` block through
// hand-maintained vars.
func TestRunNamespaceExposesBudget(t *testing.T) {
	wf := budgetedWorkflow()
	eng := New(wf, tmpStore(t), newStubExecutor())
	rs := eng.newRunState("run-ns", nil)
	rs.budget.RecordUsage(1_200, 3.25)

	ctx := eng.exprContext(rs, nil)
	if ctx.Run == nil {
		t.Fatal("expr context has no run resolver")
	}

	if got := ctx.Run([]string{"id"}); got != "run-ns" {
		t.Errorf("run.id = %v, want run-ns", got)
	}
	if got := ctx.Run([]string{"cost_usd"}); got != 3.25 {
		t.Errorf("run.cost_usd = %v (%T), want 3.25", got, got)
	}
	if got := ctx.Run([]string{"tokens"}); got != int64(1_200) {
		t.Errorf("run.tokens = %v (%T), want 1200", got, got)
	}
	if got := ctx.Run([]string{"iterations"}); got != int64(1) {
		t.Errorf("run.iterations = %v (%T), want 1", got, got)
	}
	if got := ctx.Run([]string{"max_cost_usd"}); got != 12.5 {
		t.Errorf("run.max_cost_usd = %v (%T), want 12.5", got, got)
	}
	if got := ctx.Run([]string{"max_tokens"}); got != int64(40_000) {
		t.Errorf("run.max_tokens = %v (%T), want 40000", got, got)
	}
	if got := ctx.Run([]string{"max_iterations"}); got != int64(7) {
		t.Errorf("run.max_iterations = %v (%T), want 7", got, got)
	}
	if got := ctx.Run([]string{"max_duration_seconds"}); got != float64(5_400) {
		t.Errorf("run.max_duration_seconds = %v (%T), want 5400", got, got)
	}
	elapsed, ok := ctx.Run([]string{"elapsed_seconds"}).(float64)
	if !ok || elapsed < 0 {
		t.Errorf("run.elapsed_seconds = %v (%T), want a non-negative float", elapsed, elapsed)
	}
	// An unknown member keeps the namespace's historical behaviour:
	// unresolved (nil), never an error and never a zero that reads as a
	// measured value.
	if got := ctx.Run([]string{"no_such_member"}); got != nil {
		t.Errorf("run.no_such_member = %v, want nil", got)
	}
}

// TestRunNamespaceCapsFollowLiveRaises covers the "EFFECTIVE caps" half of
// #738: a raise_budget steering command (and the CLI/recipe overrides that
// land the same way) must move `run.max_*`, or a guard reads a ceiling the
// run no longer has.
func TestRunNamespaceCapsFollowLiveRaises(t *testing.T) {
	wf := budgetedWorkflow()
	eng := New(wf, tmpStore(t), newStubExecutor())
	rs := eng.newRunState("run-raise", nil)

	if _, raised := rs.budget.RaiseCaps(ir.BudgetOverrides{MaxCostUSD: 40, MaxDuration: "3h"}); !raised {
		t.Fatal("RaiseCaps did not raise")
	}
	ctx := eng.exprContext(rs, nil)
	if got := ctx.Run([]string{"max_cost_usd"}); got != 40.0 {
		t.Errorf("run.max_cost_usd = %v, want 40 after a live raise", got)
	}
	if got := ctx.Run([]string{"max_duration_seconds"}); got != float64(10_800) {
		t.Errorf("run.max_duration_seconds = %v, want 10800 after a live raise", got)
	}
}

// TestRunNamespaceWithoutBudgetBlock documents the no-`budget:` shape: the
// caps read 0 (= unbounded) and elapsed still advances, because a run's
// wall-clock is measured by the engine, not by the budget tracker the
// workflow declined to declare.
func TestRunNamespaceWithoutBudgetBlock(t *testing.T) {
	wf := budgetedWorkflow()
	wf.Budget = nil
	eng := New(wf, tmpStore(t), newStubExecutor())
	rs := eng.newRunState("run-nobudget", nil)
	if rs.budget != nil {
		t.Fatal("precondition: a workflow with no budget block must have no tracker")
	}
	rs.startedAt = time.Now().Add(-30 * time.Second)

	ctx := eng.exprContext(rs, nil)
	if got := ctx.Run([]string{"max_cost_usd"}); got != float64(0) {
		t.Errorf("run.max_cost_usd = %v, want 0 (unbounded)", got)
	}
	elapsed, ok := ctx.Run([]string{"elapsed_seconds"}).(float64)
	if !ok || elapsed < 29 {
		t.Errorf("run.elapsed_seconds = %v (%T), want >= 29", elapsed, elapsed)
	}
}

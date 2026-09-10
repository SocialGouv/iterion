package runtime

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/expr"
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

// TestRunNamespaceReachesDataMappings covers #791: the DataMapping resolver
// — a fail node's `message:`, an edge `with`, an `emit` payload, a subbot
// `with:` — is the fourth consumer of the `run.*` namespace. It resolved
// `run.id` alone, so every budget member rendered as an empty string with
// no diagnostic (C029/C036 accept the reference; the resolver dropped it).
func TestRunNamespaceReachesDataMappings(t *testing.T) {
	wf := budgetedWorkflow()
	eng := New(wf, tmpStore(t), newStubExecutor())
	rs := eng.newRunState("run-map", nil)
	rs.budget.RecordUsage(1_200, 3.25)

	// One vocabulary, four consumers: every member the namespace publishes
	// must resolve through the mapping path exactly as through the expr path.
	for _, member := range RunNamespaceMembers {
		ref := &ir.Ref{Kind: ir.RefRun, Path: []string{member}, Raw: "{{run." + member + "}}"}
		got := eng.resolveRef(ref, rs.scope())
		want := resolveRunPath(rs, []string{member})
		if member == "elapsed_seconds" {
			// A live reading: two lookups never agree to the digit.
			if _, ok := got.(float64); !ok {
				t.Errorf("mapping {{run.elapsed_seconds}} = %#v, want a float64", got)
			}
			continue
		}
		if got != want {
			t.Errorf("mapping {{run.%s}} = %#v, want %#v (what the expr path resolves)", member, got, want)
		}
	}
	// An unknown member stays unresolved on this path too — never a zero a
	// guard could mistake for a measurement.
	if got := eng.resolveRef(&ir.Ref{Kind: ir.RefRun, Path: []string{"no_such_member"}}, rs.scope()); got != nil {
		t.Errorf("mapping {{run.no_such_member}} = %#v, want nil", got)
	}

	// A whole-reference mapping keeps the value typed, so an edge `with` can
	// carry a cap into an int field.
	if got := eng.resolveMapping(mapping(t, "{{run.max_tokens}}"), rs.scope()); got != int64(40_000) {
		t.Errorf("{{run.max_tokens}} through a mapping = %#v, want int64(40000)", got)
	}

	// The fail-node message is where #791 was observed: the figure that
	// caused the refusal is the one the operator must read.
	out := eng.failOutcome(rs, &ir.FailNode{
		BaseNode: ir.BaseNode{ID: "refuse"},
		Message:  mapping(t, "spent {{run.cost_usd}} of {{run.max_cost_usd}} USD after {{run.elapsed_seconds}}s"),
	})
	const wantPrefix = "spent 3.25 of 12.5 USD after "
	if !strings.HasPrefix(out.reason, wantPrefix) || strings.HasSuffix(out.reason, "after s") {
		t.Errorf("fail message rendered %q, want %q<elapsed>s", out.reason, wantPrefix)
	}
}

// TestFailMessageRendersRunNamespaceOnTheRun is the operator-visible half of
// #791: the rendered message is what `run.Error` carries, on a real run.
func TestFailMessageRendersRunNamespaceOnTheRun(t *testing.T) {
	stopExpr, err := expr.Parse("true")
	if err != nil {
		t.Fatalf("parse compute expr: %v", err)
	}
	wf := &ir.Workflow{
		Name:  "fail_message_run_ns",
		Entry: "guard",
		Nodes: map[string]ir.Node{
			"guard": &ir.ComputeNode{
				BaseNode: ir.BaseNode{ID: "guard"},
				Exprs:    []*ir.ComputeExpr{{Key: "stop", AST: stopExpr, Raw: "true"}},
			},
			"refuse": &ir.FailNode{
				BaseNode: ir.BaseNode{ID: "refuse"},
				Code:     "PLAN_BUDGET_EXHAUSTED",
				Message:  mapping(t, "cap={{run.max_cost_usd}} tokens_cap={{run.max_tokens}}"),
			},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "guard", To: "refuse", Condition: "stop"},
			{From: "guard", To: "done", IsElse: true},
		},
		Budget:  &ir.Budget{MaxCostUSD: 12.5, MaxTokens: 40_000},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}
	s := tmpStore(t)
	if err := New(wf, s, newStubExecutor()).Run(context.Background(), "run-fail-msg", nil); err == nil {
		t.Fatal("run succeeded; the graph routes to a fail node")
	}
	run, err := s.LoadRun(context.Background(), "run-fail-msg")
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if want := "cap=12.5 tokens_cap=40000"; run.Error != want {
		t.Errorf("run.Error = %q, want %q", run.Error, want)
	}
}

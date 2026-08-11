package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// campaignShapedWorkflow builds the graph every v2 campaign bot uses: a
// costly pass, a gate that decides, a back-edge for another pass, and a
// delivery tail reached BOTH when the gate converges and when the loop
// stops. maxCost bounds the run; the pass never converges, so what ends
// the loop is the only question the test asks.
func campaignShapedWorkflow(maxCost float64) *ir.Workflow {
	return &ir.Workflow{
		Name:  "campaign_shaped",
		Entry: "pass",
		Nodes: map[string]ir.Node{
			"pass":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "pass"}},
			"gate":    &ir.ComputeNode{BaseNode: ir.BaseNode{ID: "gate"}},
			"deliver": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "deliver"}},
			"done":    &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "pass", To: "gate"},
			{From: "gate", To: "deliver", Condition: "converged"},
			{From: "gate", To: "pass", LoopName: "continuation"},
			{From: "gate", To: "deliver"},
			{From: "deliver", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops: map[string]*ir.Loop{
			"continuation": {Name: "continuation", MaxIterations: 10},
		},
		Budget: &ir.Budget{MaxCostUSD: maxCost},
	}
}

// TestLoopBudgetGuard_FallsThroughToDeliveryTail is the composition the
// guard exists for: a campaign loop whose next pass costs more than the
// budget has left must leave through its own exit path — running the
// delivery tail that publishes what it banked — instead of dying
// mid-pass on the hard cap with the tail unreached.
//
// $10 cap, $4 a pass: pass 1 leaves $6 (affordable), pass 2 leaves $2
// (not), so the back-edge is declined at the second crossing.
func TestLoopBudgetGuard_FallsThroughToDeliveryTail(t *testing.T) {
	wf := campaignShapedWorkflow(10.0)

	var passes, delivered int
	exec := newStubExecutor()
	exec.on("pass", func(_ map[string]any) (map[string]any, error) {
		passes++
		return map[string]any{"ok": true, "_cost_usd": 4.0}, nil
	})
	exec.on("gate", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"converged": false}, nil
	})
	exec.on("deliver", func(_ map[string]any) (map[string]any, error) {
		delivered++
		return map[string]any{"published": true}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)
	if err := eng.Run(context.Background(), "run-loop-budget-guard", nil); err != nil {
		t.Fatalf("run should finish through the delivery tail, got: %v", err)
	}

	if passes != 2 {
		t.Errorf("ran %d passes, want 2 (the third is unaffordable)", passes)
	}
	if delivered != 1 {
		t.Fatalf("delivery tail ran %d times, want 1 — the banked work was stranded", delivered)
	}

	run, err := s.LoadRun(context.Background(), "run-loop-budget-guard")
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if run.Status != store.RunStatusFinished {
		t.Errorf("status = %q, want %q", run.Status, store.RunStatusFinished)
	}

	// The decline is loud: an operator reading the events must be able to
	// tell "stopped early, on purpose" from "converged".
	events, err := s.LoadEvents(context.Background(), "run-loop-budget-guard")
	if err != nil {
		t.Fatalf("load events: %v", err)
	}
	var guard map[string]any
	for _, ev := range events {
		if ev.Type == store.EventBudgetWarning && ev.Data["reason"] == "loop_budget_guard" {
			guard = ev.Data
		}
	}
	if guard == nil {
		t.Fatal("no budget_warning{reason: loop_budget_guard} event — the early exit is silent")
	}
	if guard["dimension"] != "cost_usd" {
		t.Errorf("guard dimension = %v, want cost_usd", guard["dimension"])
	}
	if guard["loop"] != "continuation" {
		t.Errorf("guard loop = %v, want continuation", guard["loop"])
	}
}

// TestLoopBudgetGuard_OffRestoresTheStrandingFailure is the control: it
// re-introduces the defect through the documented escape hatch and
// checks the test above is not passing vacuously. With the guard off the
// same workflow starts a pass it cannot pay for, dies on the hard cap,
// and never reaches its delivery tail.
func TestLoopBudgetGuard_OffRestoresTheStrandingFailure(t *testing.T) {
	t.Setenv("ITERION_LOOP_BUDGET_GUARD", "off")

	wf := campaignShapedWorkflow(10.0)

	var delivered int
	exec := newStubExecutor()
	exec.on("pass", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"ok": true, "_cost_usd": 4.0}, nil
	})
	exec.on("gate", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"converged": false}, nil
	})
	exec.on("deliver", func(_ map[string]any) (map[string]any, error) {
		delivered++
		return map[string]any{"published": true}, nil
	})

	eng := New(wf, tmpStore(t), exec)
	err := eng.Run(context.Background(), "run-loop-budget-guard-off", nil)
	if err == nil {
		t.Fatal("expected the unguarded run to die on the cost budget")
	}
	if !strings.Contains(err.Error(), "budget exceeded") {
		t.Errorf("error = %v, want a budget-exceeded failure", err)
	}
	if delivered != 0 {
		t.Errorf("delivery tail ran %d times unguarded, want 0 (that is the defect)", delivered)
	}
}

// TestLoopBudgetShortfall_PricesOneIterationAtATime covers the pricing
// rule directly: each crossing is priced by the distance to the previous
// mark, not by everything the run has spent.
func TestLoopBudgetShortfall_PricesOneIterationAtATime(t *testing.T) {
	wf := campaignShapedWorkflow(10.0)
	eng := newEngineWith(t, wf)
	rs := eng.newRunState("r", nil)
	rs.budget = newSharedBudget(wf.Budget, eng.logger)

	// Crossing 1 after a $4 pass: $6 left covers another $4.
	rs.budget.RecordUsage(0, 4.0)
	if dim, _, _ := eng.loopBudgetShortfall("continuation", rs); dim != "" {
		t.Fatalf("first crossing reported a %q shortfall with $6 left for a $4 pass", dim)
	}

	// Crossing 2 after a second $4 pass: $2 left cannot cover $4. The
	// price is the LAST pass ($4), not the run total ($8).
	rs.budget.RecordUsage(0, 4.0)
	dim, need, have := eng.loopBudgetShortfall("continuation", rs)
	if dim != "cost_usd" {
		t.Fatalf("second crossing = %q, want a cost_usd shortfall", dim)
	}
	if need != 4.0 {
		t.Errorf("priced the next pass at %.2f, want 4.00 (the last one, not the total)", need)
	}
	if have != 2.0 {
		t.Errorf("remaining = %.2f, want 2.00", have)
	}
}

// TestLoopBudgetShortfall_ResumeRebasesTheBaseline guards the resume
// path: a run that resumes with most of its budget already spent must
// price its next pass by the pass it just ran, not by the consumption it
// inherited from the checkpoint — which would decline the back-edge on
// the first crossing of every resumed run.
func TestLoopBudgetShortfall_ResumeRebasesTheBaseline(t *testing.T) {
	wf := campaignShapedWorkflow(100.0)
	eng := newEngineWith(t, wf)
	rs := eng.newRunState("r", nil)
	rs.budget = newSharedBudget(wf.Budget, eng.logger)

	// Resume carrying $70 of prior spend, then run a $5 pass. $30 left
	// covers another $5 — but only if the inherited $70 is not counted
	// as this pass's price.
	restoreBudgetAccounting(rs, &store.Checkpoint{BudgetCostUSD: 70.0})
	rs.budget.RecordUsage(0, 5.0)

	if dim, need, have := eng.loopBudgetShortfall("continuation", rs); dim != "" {
		t.Fatalf("resumed run declined its back-edge: %s needs %.2f, has %.2f — the baseline was not rebased", dim, need, have)
	}
}

// TestLoopBudgetShortfall_IgnoresUnenforcedAxes checks the guard only
// speaks for caps that exist: a workflow that budgets cost alone must
// never be stopped by a token or duration figure nobody bounded.
func TestLoopBudgetShortfall_IgnoresUnenforcedAxes(t *testing.T) {
	wf := campaignShapedWorkflow(1000.0)
	eng := newEngineWith(t, wf)
	rs := eng.newRunState("r", nil)
	rs.budget = newSharedBudget(wf.Budget, eng.logger)

	// A pass burning a large token count against an unlimited token axis.
	rs.budget.RecordUsage(5_000_000, 1.0)
	if dim, _, _ := eng.loopBudgetShortfall("continuation", rs); dim != "" {
		t.Fatalf("reported a %q shortfall on an axis the workflow does not cap", dim)
	}
}

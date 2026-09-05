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
// stops. maxTokens bounds the run; the pass never converges, so what
// ends the loop is the only question the test asks.
func campaignShapedWorkflow(maxTokens int) *ir.Workflow {
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
			"continuation": {
				Name:          "continuation",
				MaxIterations: 10,
				Entries:       map[string]bool{"pass": true},
				Body:          map[string]bool{"pass": true, "gate": true},
			},
		},
		Budget: &ir.Budget{MaxTokens: maxTokens},
	}
}

// costingExecutor wires a stub whose `pass` burns a fixed token count,
// whose `gate` never converges, and whose `deliver` records that the
// delivery tail was reached. The counters it returns are the assertions.
func costingExecutor(passTokens int) (exec *stubExecutor, passes, delivered *int) {
	p, d := 0, 0
	exec = newStubExecutor()
	exec.on("pass", func(_ map[string]any) (map[string]any, error) {
		p++
		return map[string]any{"ok": true, "_tokens": passTokens}, nil
	})
	exec.on("gate", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"converged": false}, nil
	})
	exec.on("deliver", func(_ map[string]any) (map[string]any, error) {
		d++
		return map[string]any{"published": true}, nil
	})
	return exec, &p, &d
}

// TestLoopBudgetGuard_FallsThroughToDeliveryTail is the composition the
// guard exists for: a campaign loop whose next pass would exhaust the
// budget must leave through its own exit path — running the delivery
// tail that publishes what it banked — instead of dying mid-pass with
// the tail unreached.
func TestLoopBudgetGuard_FallsThroughToDeliveryTail(t *testing.T) {
	wf := campaignShapedWorkflow(10_000)
	exec, passes, delivered := costingExecutor(3_000)

	s := tmpStore(t)
	eng := New(wf, s, exec)
	if err := eng.Run(context.Background(), "run-loop-budget-guard", nil); err != nil {
		t.Fatalf("run should finish through the delivery tail, got: %v", err)
	}

	if *passes < 2 {
		t.Errorf("ran %d passes, want at least 2 — the guard stopped the loop too early", *passes)
	}
	if *delivered != 1 {
		t.Fatalf("delivery tail ran %d times, want 1 — the banked work was stranded", *delivered)
	}

	run, err := s.LoadRun(context.Background(), "run-loop-budget-guard")
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if run.Status != store.RunStatusFinished {
		t.Errorf("status = %q, want %q", run.Status, store.RunStatusFinished)
	}

	// The decline is loud, and carries what every budget_warning consumer
	// (run report, alert manager) reads.
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
	for _, key := range []string{"dimension", "loop", "remaining", "needed", "used", "limit"} {
		if _, ok := guard[key]; !ok {
			t.Errorf("guard event is missing %q", key)
		}
	}
	if guard["dimension"] != "tokens" || guard["loop"] != "continuation" {
		t.Errorf("guard = %v/%v, want tokens/continuation", guard["dimension"], guard["loop"])
	}
}

// TestLoopBudgetGuard_OffRestoresTheStrandingFailure is the control: it
// re-introduces the defect through the documented escape hatch and
// checks the test above is not passing vacuously. With the guard off the
// same workflow starts a pass it cannot pay for, dies on the budget, and
// never reaches its delivery tail.
func TestLoopBudgetGuard_OffRestoresTheStrandingFailure(t *testing.T) {
	t.Setenv("ITERION_LOOP_BUDGET_GUARD", "off")

	wf := campaignShapedWorkflow(10_000)
	exec, _, delivered := costingExecutor(3_000)

	eng := New(wf, tmpStore(t), exec)
	err := eng.Run(context.Background(), "run-loop-budget-guard-off", nil)
	if err == nil {
		t.Fatal("expected the unguarded run to die on the token budget")
	}
	if !strings.Contains(err.Error(), "budget") {
		t.Errorf("error = %v, want a budget failure", err)
	}
	if *delivered != 0 {
		t.Errorf("delivery tail ran %d times unguarded, want 0 (that is the defect)", *delivered)
	}
}

// sequentialLoopsWorkflow puts an expensive prologue in front of a loop,
// so the loop is ENTERED long after the run started spending. This is
// secured-renovacy's shape: a Phase-2 review loop whose first crossing
// happens after all of Phase 1.
func sequentialLoopsWorkflow(maxTokens int) *ir.Workflow {
	return &ir.Workflow{
		Name:  "sequential_loops",
		Entry: "prologue",
		Nodes: map[string]ir.Node{
			"prologue": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "prologue"}},
			"pass":     &ir.AgentNode{BaseNode: ir.BaseNode{ID: "pass"}},
			"gate":     &ir.ComputeNode{BaseNode: ir.BaseNode{ID: "gate"}},
			"deliver":  &ir.ToolNode{BaseNode: ir.BaseNode{ID: "deliver"}},
			"done":     &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "prologue", To: "pass"},
			{From: "pass", To: "gate"},
			{From: "gate", To: "deliver", Condition: "converged"},
			{From: "gate", To: "pass", LoopName: "phase2"},
			{From: "gate", To: "deliver"},
			{From: "deliver", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops: map[string]*ir.Loop{
			"phase2": {
				Name:          "phase2",
				MaxIterations: 10,
				Entries:       map[string]bool{"pass": true},
				Body:          map[string]bool{"pass": true, "gate": true},
			},
		},
		Budget: &ir.Budget{MaxTokens: maxTokens},
	}
}

// TestLoopBudgetGuard_LateEnteredLoopIsPricedFromItsEntry is the
// regression for the pricing baseline. A loop entered after an expensive
// prologue must be priced by ITS OWN iterations, not by everything the
// run spent before it existed — otherwise any loop first crossed past
// the halfway mark of a budget is declined outright and never iterates,
// which silently turns a review loop into a single-shot body.
func TestLoopBudgetGuard_LateEnteredLoopIsPricedFromItsEntry(t *testing.T) {
	wf := sequentialLoopsWorkflow(20_000)

	var prologues, passes, delivered int
	exec := newStubExecutor()
	// The prologue alone burns 60% of the budget.
	exec.on("prologue", func(_ map[string]any) (map[string]any, error) {
		prologues++
		return map[string]any{"ok": true, "_tokens": 12_000}, nil
	})
	// Each loop iteration is cheap — several fit in what is left.
	exec.on("pass", func(_ map[string]any) (map[string]any, error) {
		passes++
		return map[string]any{"ok": true, "_tokens": 1_000}, nil
	})
	exec.on("gate", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"converged": false}, nil
	})
	exec.on("deliver", func(_ map[string]any) (map[string]any, error) {
		delivered++
		return map[string]any{"published": true}, nil
	})

	eng := New(wf, tmpStore(t), exec)
	if err := eng.Run(context.Background(), "run-late-loop", nil); err != nil {
		t.Fatalf("run should finish through the delivery tail, got: %v", err)
	}

	if prologues != 1 {
		t.Fatalf("prologue ran %d times, want 1", prologues)
	}
	if passes < 3 {
		t.Errorf("the late-entered loop iterated %d times, want at least 3 — it was priced by the prologue instead of by its own body", passes)
	}
	if delivered != 1 {
		t.Errorf("delivery tail ran %d times, want 1", delivered)
	}
}

// TestLoopBudgetGuard_ReEnteredLoopReBasesItsPrice covers the nested
// shape: an inner loop re-entered on each outer iteration must be
// re-priced at each entry. Carrying the mark from the previous instance
// prices its first crossing by everything that ran in between — the
// whole of the outer iteration — and declines it.
func TestLoopBudgetGuard_ReEnteredLoopReBasesItsPrice(t *testing.T) {
	wf := sequentialLoopsWorkflow(20_000)
	eng := New(wf, tmpStore(t), newStubExecutor())
	rs := eng.newRunState("r", nil)
	rs.budget = newSharedBudget(wf.Budget, eng.logger)
	eng.baselineUnpricedLoops(rs)

	// An outer iteration runs and the inner loop is entered: the entry
	// re-bases its price on what has been consumed by then.
	rs.budget.RecordUsage(9_000, 0)
	markLoopBudget(rs, "phase2")

	// One cheap inner iteration, then its first crossing of this instance.
	rs.budget.RecordUsage(1_000, 0)
	if v := eng.loopBudgetShortfall("phase2", rs); v != nil {
		t.Fatalf("re-entered loop declined its first crossing: %s priced at %.0f with %.0f left — the mark was not re-based at entry",
			v.dimension, v.spent, v.remaining)
	}
}

// TestLoopBudgetGuard_ConditionalBackEdgeIsNotPricedWhenItDoesNotMatch
// covers a conditional back-edge (secured-renovacy's `when not
// was_batch as package_loop(50)`). On a crossing where the condition is
// false the edge was never a candidate, so it must not be reported as a
// loop the budget could not fund — an operator reading that would be
// told the wrong thing about a pure routing decision.
func TestLoopBudgetGuard_ConditionalBackEdgeIsNotPricedWhenItDoesNotMatch(t *testing.T) {
	wf := campaignShapedWorkflow(10_000)
	// Make the back-edge conditional on `again`, which the gate denies.
	for _, e := range wf.Edges {
		if e.LoopName == "continuation" {
			e.Condition = "again"
		}
	}

	var delivered int
	exec := newStubExecutor()
	exec.on("pass", func(_ map[string]any) (map[string]any, error) {
		// Spend enough that an unconditional guard WOULD decline.
		return map[string]any{"ok": true, "_tokens": 8_000}, nil
	})
	exec.on("gate", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"converged": false, "again": false}, nil
	})
	exec.on("deliver", func(_ map[string]any) (map[string]any, error) {
		delivered++
		return map[string]any{"published": true}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)
	if err := eng.Run(context.Background(), "run-cond-backedge", nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if delivered != 1 {
		t.Fatalf("delivery tail ran %d times, want 1", delivered)
	}

	events, err := s.LoadEvents(context.Background(), "run-cond-backedge")
	if err != nil {
		t.Fatalf("load events: %v", err)
	}
	for _, ev := range events {
		if ev.Type == store.EventBudgetWarning && ev.Data["reason"] == "loop_budget_guard" {
			t.Fatal("reported a loop-budget decline for a back-edge whose `when` was false — it was never a candidate")
		}
	}
}

// TestLoopBudgetGuard_MarksSurviveResume is the regression for the
// resume path. The checkpoint is written after EVERY successful node,
// including a loop's cheap decision node, so a resume can land there and
// cross the back-edge immediately. Without the persisted price that
// crossing measures nothing, reports no shortfall, and launches a full
// pass on an almost-spent budget — the exact stranding the guard exists
// to prevent, on the documented "raise the cap + resume" recovery path.
func TestLoopBudgetGuard_MarksSurviveResume(t *testing.T) {
	wf := campaignShapedWorkflow(10_000)
	eng := New(wf, tmpStore(t), newStubExecutor())

	// A run that has spent 8k of its 10k, with the loop last priced at 5k
	// (so one iteration costs 3k).
	origin := eng.newRunState("r", nil)
	origin.budget = newSharedBudget(wf.Budget, eng.logger)
	origin.budget.RecordUsage(5_000, 0)
	markLoopBudget(origin, "continuation")
	origin.budget.RecordUsage(3_000, 0)

	cp := buildCheckpoint(origin, "gate")
	if len(cp.LoopBudgetMarks) == 0 {
		t.Fatal("checkpoint carries no loop budget marks")
	}

	// Resume: consumption and prices are restored together.
	resumed := eng.newRunState("r", nil)
	resumed.budget = newSharedBudget(wf.Budget, eng.logger)
	restoreBudgetAccounting(resumed, cp)
	eng.baselineUnpricedLoops(resumed)

	v := eng.loopBudgetShortfall("continuation", resumed)
	if v == nil {
		t.Fatal("resumed run reported no shortfall: it would launch a 3k pass with 2k left, stranding the work")
	}
	if v.dimension != "tokens" {
		t.Errorf("shortfall dimension = %q, want tokens", v.dimension)
	}
}

// TestLoopBudgetGuard_UnpricedLoopIsNotDeclined states the rule for a
// loop with no measurement: report nothing rather than guess. Guessing
// from run start is what charges a loop for work that predates it.
func TestLoopBudgetGuard_UnpricedLoopIsNotDeclined(t *testing.T) {
	wf := campaignShapedWorkflow(10_000)
	eng := New(wf, tmpStore(t), newStubExecutor())
	rs := eng.newRunState("r", nil)
	rs.budget = newSharedBudget(wf.Budget, eng.logger)

	rs.budget.RecordUsage(9_500, 0)
	if v := eng.loopBudgetShortfall("continuation", rs); v != nil {
		t.Fatalf("declined an unmeasured loop on a %s guess", v.dimension)
	}
}

// TestLoopBudgetGuard_IgnoresUnenforcedAxes checks the guard only speaks
// for caps that exist: a workflow that budgets tokens alone must never
// be stopped by a cost figure nobody bounded.
func TestLoopBudgetGuard_IgnoresUnenforcedAxes(t *testing.T) {
	wf := campaignShapedWorkflow(1_000_000)
	eng := New(wf, tmpStore(t), newStubExecutor())
	rs := eng.newRunState("r", nil)
	rs.budget = newSharedBudget(wf.Budget, eng.logger)
	eng.baselineUnpricedLoops(rs)

	// A pass burning real dollars against an unlimited cost axis.
	rs.budget.RecordUsage(1_000, 500.0)
	if v := eng.loopBudgetShortfall("continuation", rs); v != nil {
		t.Fatalf("reported a %q shortfall on an axis the workflow does not cap", v.dimension)
	}
}

// TestLoopBudgetGuard_PrecedenceChain walks the resolution order the rest
// of the engine uses — run override → workflow block → env → built-in on.
// Each layer must be able to overrule every layer below it, and an unset
// layer must defer rather than decide.
func TestLoopBudgetGuard_PrecedenceChain(t *testing.T) {
	cases := []struct {
		name     string
		override string // CLI / launch
		workflow string // `loop_budget_guard:` in the workflow block
		env      string // ITERION_LOOP_BUDGET_GUARD
		want     bool
	}{
		{"nothing set: on by default", "", "", "", true},
		{"env alone turns it off", "", "", "off", false},
		{"env alone, 0 spelling", "", "", "0", false},
		{"workflow off beats an unset env", "", "off", "", false},
		{"workflow off beats env on", "", "off", "on", false},
		{"workflow on beats env off", "", "on", "off", true},
		{"override off beats workflow on", "off", "on", "on", false},
		{"override on beats workflow off", "on", "off", "off", true},
		{"unset override defers to the workflow", "", "off", "on", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ITERION_LOOP_BUDGET_GUARD", tc.env)
			wf := campaignShapedWorkflow(10_000)
			wf.LoopBudgetGuard = tc.workflow
			eng := New(wf, tmpStore(t), newStubExecutor(), WithLoopBudgetGuard(tc.override))
			if got := eng.loopBudgetGuardEnabled(); got != tc.want {
				t.Errorf("guard enabled = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestLoopBudgetGuard_WorkflowOffIsHonouredEndToEnd checks the DSL switch
// reaches the decision and not just the resolver: a workflow that opts out
// runs at its cap head-on and strands its delivery tail, exactly as the
// env escape hatch does.
func TestLoopBudgetGuard_WorkflowOffIsHonouredEndToEnd(t *testing.T) {
	wf := campaignShapedWorkflow(10_000)
	wf.LoopBudgetGuard = "off"
	exec, _, delivered := costingExecutor(3_000)

	eng := New(wf, tmpStore(t), exec)
	if err := eng.Run(context.Background(), "run-wf-guard-off", nil); err == nil {
		t.Fatal("expected the opted-out run to die on the token budget")
	}
	if *delivered != 0 {
		t.Errorf("delivery tail ran %d times with the guard off, want 0", *delivered)
	}
}

// TestValidateLoopBudgetGuardMode covers the CLI boundary: empty inherits,
// on|off are accepted, and anything else is refused rather than silently
// read as "inherit" — which would keep a guard the operator asked to lift.
func TestValidateLoopBudgetGuardMode(t *testing.T) {
	for _, ok := range []string{"", "on", "off", " ON ", "Off"} {
		if err := ValidateLoopBudgetGuardMode(ok); err != nil {
			t.Errorf("ValidateLoopBudgetGuardMode(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"0", "true", "no", "disabled", "onn"} {
		if err := ValidateLoopBudgetGuardMode(bad); err == nil {
			t.Errorf("ValidateLoopBudgetGuardMode(%q) accepted an unspellable mode", bad)
		}
	}
}

// TestLoopBudgetVerdict_DurationDisplayIsInSeconds guards the operator
// signal: durations are tracked in nanoseconds, and the event is the
// only thing telling someone why their run stopped iterating.
func TestLoopBudgetVerdict_DurationDisplayIsInSeconds(t *testing.T) {
	v := loopBudgetVerdict{dimension: "duration", spent: 4.5e12, remaining: 3.6e12, used: 3.6e12, limit: 7.2e12}
	spent, remaining, _, _, unit := v.display()
	if unit != "seconds" {
		t.Errorf("unit = %q, want seconds", unit)
	}
	if spent != 4500 || remaining != 3600 {
		t.Errorf("display = %.0f/%.0f, want 4500/3600 seconds", spent, remaining)
	}

	plain := loopBudgetVerdict{dimension: "tokens", spent: 3000, remaining: 2000}
	if s, r, _, _, u := plain.display(); u != "" || s != 3000 || r != 2000 {
		t.Errorf("non-duration axis was converted: %.0f/%.0f %q", s, r, u)
	}
}

// lateLoopWorkflow: a costly prelude, then a verify/gate pair shared by a
// loop whose head is `act` — the body is entered at `verify`, off its head,
// exactly how a campaign bot's extension loop is entered after the lot's
// own pass. maxTokens bounds the run.
func lateLoopWorkflow(maxTokens int) *ir.Workflow {
	return &ir.Workflow{
		Name:  "late_loop",
		Entry: "pre",
		Nodes: map[string]ir.Node{
			"pre":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "pre"}},
			"verify": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "verify"}},
			// A tool node, so the stub executor's verdict (needs_act) reaches
			// the conditional loop edge — a bare compute node yields nothing.
			"gate": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "gate"}},
			"act":  &ir.ToolNode{BaseNode: ir.BaseNode{ID: "act"}},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "pre", To: "verify"},
			{From: "verify", To: "gate"},
			{From: "gate", To: "done", Condition: "converged"},
			{From: "gate", To: "act", Condition: "needs_act", LoopName: "act_loop"},
			{From: "act", To: "verify"},
			{From: "gate", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops: map[string]*ir.Loop{
			"act_loop": {
				Name:          "act_loop",
				MaxIterations: 1,
				Entries:       map[string]bool{"act": true},
				Body:          map[string]bool{"act": true, "verify": true, "gate": true},
			},
		},
		Budget: &ir.Budget{MaxTokens: maxTokens},
	}
}

// TestLoopBudgetGuard_BodyEnteredOffItsHeadIsPricedFromThatEntry: a loop
// whose body is entered at a node that is not its head (the verify a
// sibling loop shares) must be priced from that entry, not from run
// start. Priced from run start, the prelude's whole cost is what the
// guard charges the loop's FIRST crossing — and declines it, so the act
// the gate asked for never runs. Measured on a lot whose extension edge
// was skipped with "last one took 29.21", the run's entire spend.
func TestLoopBudgetGuard_BodyEnteredOffItsHeadIsPricedFromThatEntry(t *testing.T) {
	wf := lateLoopWorkflow(12_000)
	exec := newStubExecutor()
	acts, gates := 0, 0
	exec.on("pre", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"ok": true, "_tokens": 6_000}, nil
	})
	exec.on("verify", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	})
	exec.on("gate", func(_ map[string]any) (map[string]any, error) {
		gates++
		if acts == 0 {
			return map[string]any{"converged": false, "needs_act": true}, nil
		}
		return map[string]any{"converged": true, "needs_act": false}, nil
	})
	exec.on("act", func(_ map[string]any) (map[string]any, error) {
		acts++
		return map[string]any{"ok": true, "_tokens": 1_000}, nil
	})
	eng := New(wf, tmpStore(t), exec)
	if err := eng.Run(context.Background(), "late", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if acts != 1 {
		t.Fatalf("act ran %d times, want 1: the loop's first crossing was priced at the prelude's cost and declined", acts)
	}
	if gates != 2 {
		t.Fatalf("gate ran %d times, want 2", gates)
	}
}

// TestBaselineUnpricedLoops_ReBasesAStaleRunStartMarkOfALateLoop: a
// checkpoint written by an engine that priced entries only at a loop's
// head carries a run-start zero for a loop entered off its head. Restored
// as is, that zero prices the loop's first crossing at everything the run
// spent. The session baseline re-bases it at the resume point; a loop
// that holds the workflow entry keeps its zero, which is its true price;
// a measured mark is never touched.
func TestBaselineUnpricedLoops_ReBasesAStaleRunStartMarkOfALateLoop(t *testing.T) {
	wf := lateLoopWorkflow(12_000)
	wf.Loops["outer"] = &ir.Loop{Name: "outer", MaxIterations: 3, Entries: map[string]bool{"pre": true}, Body: map[string]bool{"pre": true, "verify": true, "gate": true}}
	wf.Loops["measured"] = &ir.Loop{Name: "measured", MaxIterations: 3, Entries: map[string]bool{"act": true}, Body: map[string]bool{"act": true, "verify": true}}
	eng := New(wf, tmpStore(t), newStubExecutor())
	rs := eng.newRunState("r", nil)
	rs.budget = newSharedBudget(wf.Budget, eng.logger)
	rs.budget.RecordUsage(8_000, 0)
	restoreLoopBudgetMarks(rs, map[string]map[string]float64{
		"act_loop": {"tokens": 0, "cost_usd": 0, "duration": 500_000},
		"outer":    {"tokens": 0, "cost_usd": 0, "duration": 500_000},
		"measured": {"tokens": 5_000, "cost_usd": 0, "duration": 3e12},
	})
	eng.baselineUnpricedLoops(rs)
	if got := rs.loopBudgetMarks["act_loop"]["tokens"]; got != 8_000 {
		t.Fatalf("act_loop mark = %v tokens, want 8000: the stale run-start mark must be re-based at the resume point", got)
	}
	if got := rs.loopBudgetMarks["outer"]["tokens"]; got != 0 {
		t.Fatalf("outer mark = %v tokens, want 0: a loop holding the entry keeps its run-start price", got)
	}
	if got := rs.loopBudgetMarks["measured"]["tokens"]; got != 5_000 {
		t.Fatalf("measured mark = %v tokens, want 5000: a measured mark is never touched", got)
	}
	if v := eng.loopBudgetShortfall("act_loop", rs); v != nil {
		t.Fatalf("act_loop reports a shortfall right after re-basing: %+v", v)
	}
}

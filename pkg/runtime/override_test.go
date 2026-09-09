package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// loopWorkflow builds the canonical fix→verify bounded-loop fixture with
// the given retry cap.
func loopWorkflow(maxIter int) *ir.Workflow {
	return &ir.Workflow{
		Name:  "steer_loop_test",
		Entry: "fix",
		Nodes: map[string]ir.Node{
			"fix":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "fix"}},
			"verify": &ir.JudgeNode{BaseNode: ir.BaseNode{ID: "verify"}},
			"done":   &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
			"fail":   &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges: []*ir.Edge{
			{From: "fix", To: "verify"},
			{From: "verify", To: "done", Condition: "pass"},
			{From: "verify", To: "fix", Condition: "pass", Negated: true, LoopName: "retry"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops: map[string]*ir.Loop{
			"retry": {Name: "retry", MaxIterations: maxIter},
		},
	}
}

func TestSharedBudget_RaiseCaps(t *testing.T) {
	newB := func() *SharedBudget {
		return newSharedBudget(&ir.Budget{MaxTokens: 1000, MaxCostUSD: 10, MaxIterations: 5, MaxDuration: "1h"}, nil)
	}

	t.Run("raise only strictly greater", func(t *testing.T) {
		b := newB()
		eff, raised := b.RaiseCaps(ir.BudgetOverrides{MaxTokens: 2000, MaxCostUSD: 5})
		if !raised {
			t.Fatal("raised = false, want true (tokens went up)")
		}
		if eff.MaxTokens != 2000 {
			t.Fatalf("MaxTokens = %d, want 2000", eff.MaxTokens)
		}
		if eff.MaxCostUSD != 10 {
			t.Fatalf("MaxCostUSD = %v, want 10 (lower value must be ignored)", eff.MaxCostUSD)
		}
	})

	t.Run("equal or lower is noop", func(t *testing.T) {
		b := newB()
		_, raised := b.RaiseCaps(ir.BudgetOverrides{MaxTokens: 1000, MaxCostUSD: 9.99, MaxIterations: 5})
		if raised {
			t.Fatal("raised = true, want false")
		}
		if _, ever := b.Raises(); ever {
			t.Fatal("everRaised must stay false on noop")
		}
	})

	t.Run("unlimited axis is never constrained", func(t *testing.T) {
		b := newSharedBudget(&ir.Budget{MaxCostUSD: 10}, nil) // tokens/iterations/duration unlimited
		eff, raised := b.RaiseCaps(ir.BudgetOverrides{MaxTokens: 500})
		if raised {
			t.Fatal("raising an unlimited axis must be a noop")
		}
		if eff.MaxTokens != 0 {
			t.Fatalf("MaxTokens = %d, want 0 (still unlimited)", eff.MaxTokens)
		}
	})

	t.Run("re-arms warnings on raised axis", func(t *testing.T) {
		b := newB()
		b.mu.Lock()
		b.warningsEmitted["tokens"] = true
		b.warningsEmitted["cost_usd"] = true
		b.mu.Unlock()
		_, raised := b.RaiseCaps(ir.BudgetOverrides{MaxTokens: 4000})
		if !raised {
			t.Fatal("want raised")
		}
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.warningsEmitted["tokens"] {
			t.Fatal("tokens warning must be re-armed")
		}
		if !b.warningsEmitted["cost_usd"] {
			t.Fatal("untouched axis must keep its warning state")
		}
	})

	t.Run("duration raise", func(t *testing.T) {
		b := newB()
		eff, raised := b.RaiseCaps(ir.BudgetOverrides{MaxDuration: "4h"})
		if !raised || eff.MaxDuration != "4h0m0s" {
			t.Fatalf("duration raise = (%v, %v)", eff.MaxDuration, raised)
		}
	})

	t.Run("nil safe", func(t *testing.T) {
		var b *SharedBudget
		if _, raised := b.RaiseCaps(ir.BudgetOverrides{MaxTokens: 1}); raised {
			t.Fatal("nil budget must be a noop")
		}
	})
}

func TestBumpLoop_ChannelEndToEnd(t *testing.T) {
	// Cap 2 → without the bump the loop exhausts before verify ever
	// passes (pass needs 4 fix calls) and the run fails. The +2 grant
	// delivered through the override channel lets it converge.
	wf := loopWorkflow(2)
	callCount := 0
	exec := newStubExecutor()
	exec.on("fix", func(_ map[string]any) (map[string]any, error) {
		callCount++
		return map[string]any{"n": callCount}, nil
	})
	exec.on("verify", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"pass": callCount >= 4}, nil
	})

	s := tmpStore(t)
	ch := make(chan *OverrideMsg, 1)
	msg := NewBumpLoopOverride("retry", 2, "test-operator")
	ch <- msg // drained at the first execLoop boundary

	eng := New(wf, s, exec, WithOverrideChannel(ch))
	if err := eng.Run(context.Background(), "run-bump", nil); err != nil {
		t.Fatalf("run: %v", err)
	}

	res, err := msg.Await(context.Background(), time.Second)
	if err != nil {
		t.Fatalf("await: %v", err)
	}
	if res.Err != nil || res.Noop {
		t.Fatalf("result = %+v", res)
	}
	if res.Effective["effective_max"] != 4 {
		t.Fatalf("effective_max = %v, want 4", res.Effective["effective_max"])
	}
	if callCount != 4 {
		t.Fatalf("fix calls = %d, want 4 (2 base + 2 granted)", callCount)
	}

	r, err := s.LoadRun(context.Background(), "run-bump")
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if r.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want finished", r.Status)
	}
	if r.LoopOverrides["retry"] != 2 {
		t.Fatalf("persisted LoopOverrides = %v, want retry:2", r.LoopOverrides)
	}

	events, err := s.LoadEvents(context.Background(), "run-bump")
	if err != nil {
		t.Fatal(err)
	}
	var steered *store.Event
	for i := range events {
		if events[i].Type == store.EventRunSteered {
			steered = events[i]
			break
		}
	}
	if steered == nil {
		t.Fatal("no run_steered event persisted")
	}
	if steered.Data["command"] != "bump_loop" || steered.Data["target"] != "retry" || steered.Data["operator"] != "test-operator" {
		t.Fatalf("run_steered data = %+v", steered.Data)
	}
}

func TestRaiseBudget_ChannelEndToEnd(t *testing.T) {
	// Iteration budget 2 over a 3-node chain → would exceed; the raise
	// to 10 delivered before the first node lets it finish.
	wf := &ir.Workflow{
		Name:  "steer_budget_test",
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
	ch := make(chan *OverrideMsg, 1)
	msg := NewRaiseBudgetOverride(ir.BudgetOverrides{MaxIterations: 10}, "")
	ch <- msg

	eng := New(wf, s, exec, WithOverrideChannel(ch))
	if err := eng.Run(context.Background(), "run-raise", nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	res, err := msg.Await(context.Background(), time.Second)
	if err != nil || res.Err != nil || res.Noop {
		t.Fatalf("await = (%+v, %v)", res, err)
	}

	r, err := s.LoadRun(context.Background(), "run-raise")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want finished", r.Status)
	}
	if r.BudgetRaises == nil || r.BudgetRaises.MaxIterations != 10 {
		t.Fatalf("persisted BudgetRaises = %+v, want MaxIterations 10", r.BudgetRaises)
	}
}

func TestApplyOverride_TruthfulErrors(t *testing.T) {
	wf := loopWorkflow(3)
	eng := New(wf, tmpStore(t), newStubExecutor())
	rs := eng.newRunState("r-truth", nil)
	rs.ctx = context.Background()

	t.Run("unknown loop 400", func(t *testing.T) {
		res := eng.applyOverride(rs, NewBumpLoopOverride("nope", 2, ""))
		var ule *UnknownLoopError
		if !errors.As(res.Err, &ule) {
			t.Fatalf("err = %v, want UnknownLoopError", res.Err)
		}
		if len(ule.Available) != 1 || ule.Available[0] != "retry" {
			t.Fatalf("Available = %v", ule.Available)
		}
	})

	t.Run("non-positive delta 400", func(t *testing.T) {
		res := eng.applyOverride(rs, NewBumpLoopOverride("retry", 0, ""))
		if !errors.Is(res.Err, ErrInvalidOverride) {
			t.Fatalf("err = %v, want ErrInvalidOverride", res.Err)
		}
	})

	t.Run("no budget 409", func(t *testing.T) {
		res := eng.applyOverride(rs, NewRaiseBudgetOverride(ir.BudgetOverrides{MaxTokens: 10}, ""))
		if !errors.Is(res.Err, ErrNoBudgetDeclared) {
			t.Fatalf("err = %v, want ErrNoBudgetDeclared", res.Err)
		}
	})

	t.Run("empty raise 400", func(t *testing.T) {
		res := eng.applyOverride(rs, NewRaiseBudgetOverride(ir.BudgetOverrides{}, ""))
		if !errors.Is(res.Err, ErrInvalidOverride) {
			t.Fatalf("err = %v, want ErrInvalidOverride", res.Err)
		}
	})
}

func TestApplySteeringState_ResumeReapplies(t *testing.T) {
	wf := loopWorkflow(2)
	wf.Budget = &ir.Budget{MaxTokens: 1000}
	eng := New(wf, tmpStore(t), newStubExecutor())
	rs := eng.newRunState("r-reseed", nil)

	r := &store.Run{
		ID:            "r-reseed",
		LoopOverrides: map[string]int{"retry": 3},
		BudgetRaises:  &store.RunBudgetRaises{MaxTokens: 5000},
	}
	eng.applySteeringState(rs, r)

	if got := eng.resolveLoopMax(wf.Loops["retry"], rs); got != 5 {
		t.Fatalf("resolveLoopMax = %d, want 5 (2 declared + 3 granted)", got)
	}
	caps, ever := rs.budget.Raises()
	if !ever || caps.MaxTokens != 5000 {
		t.Fatalf("budget caps = (%+v, %v), want MaxTokens 5000 re-applied", caps, ever)
	}
}

func TestOverrideAwait_Timeout(t *testing.T) {
	msg := NewBumpLoopOverride("x", 1, "")
	if _, err := msg.Await(context.Background(), 30*time.Millisecond); err == nil {
		t.Fatal("want timeout error when nothing acks")
	}
}

// TestRaiseBudget_ArrivesInTimeForTheNodeItMustSave pins the case the API
// already answers "queued … it is not lost": the operator raises the cap while
// the run is busy INSIDE the long node whose completion trips it. That node's
// overrun is consumed at the same boundary the grant lands on, so a drain done
// only at the top of the loop arrives one edge too late — and the run dies with
// the grant sitting unapplied in the channel.
func TestRaiseBudget_ArrivesInTimeForTheNodeItMustSave(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "steer_budget_late_test",
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
		Budget:  &ir.Budget{MaxCostUSD: 10},
	}

	ch := make(chan *OverrideMsg, 1)
	msg := NewRaiseBudgetOverride(ir.BudgetOverrides{MaxCostUSD: 1000}, "")

	exec := newStubExecutor()
	exec.on("a", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	})
	exec.on("b", func(_ map[string]any) (map[string]any, error) {
		// Posted from INSIDE the node, which is what "the run is busy in a
		// long node" means. The spend is 10x the cap and past the 10% exit
		// grace, so nothing but the raise itself can carry the run forward.
		ch <- msg
		return map[string]any{"ok": true, "_cost_usd": 100.0}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec, WithOverrideChannel(ch))
	if err := eng.Run(context.Background(), "run-raise-late", nil); err != nil {
		t.Fatalf("run: %v", err)
	}

	r, err := s.LoadRun(context.Background(), "run-raise-late")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want finished — the raise was drained after the "+
			"overrun it was posted to lift, so the grant could not act on the "+
			"only case it exists for", r.Status)
	}
	if r.BudgetRaises == nil || r.BudgetRaises.MaxCostUSD != 1000 {
		t.Fatalf("persisted BudgetRaises = %+v, want MaxCostUSD 1000", r.BudgetRaises)
	}
	res, err := msg.Await(context.Background(), time.Second)
	if err != nil || res.Err != nil || res.Noop {
		t.Fatalf("await = (%+v, %v) — the grant must report itself applied", res, err)
	}
}

// uncoveredOverrunWorkflow builds the fixture the three tests below share:
// `a` → `over` → `done`, where `over` blows the caps and posts a raise from
// inside itself. `done` is the successor ON PURPOSE — a DoneNode is dispatched
// by execLoopDispatchSpecial, which runs NO pre-exec budget check, so it is the
// successor that turns "the pending overrun was dropped" into "the run finished
// over its cap" instead of merely deferring the stop by one node.
func uncoveredOverrunWorkflow(name string, budget *ir.Budget) *ir.Workflow {
	return &ir.Workflow{
		Name:  name,
		Entry: "a",
		Nodes: map[string]ir.Node{
			"a":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}},
			"over": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "over"}},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "a", To: "over"},
			{From: "over", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
		Budget:  budget,
	}
}

// runUncoveredOverrun executes uncoveredOverrunWorkflow with `over` posting
// `raise` and returning `usage`, and hands back the run's events plus its error.
func runUncoveredOverrun(t *testing.T, runID string, budget *ir.Budget, raise ir.BudgetOverrides, usage map[string]any) ([]*store.Event, error) {
	t.Helper()

	ch := make(chan *OverrideMsg, 1)
	msg := NewRaiseBudgetOverride(raise, "")

	exec := newStubExecutor()
	exec.on("a", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	})
	exec.on("over", func(_ map[string]any) (map[string]any, error) {
		ch <- msg
		return usage, nil
	})

	s := tmpStore(t)
	eng := New(uncoveredOverrunWorkflow(runID, budget), s, exec, WithOverrideChannel(ch))
	err := eng.Run(context.Background(), runID, nil)

	// The raise must actually have LANDED, or these tests are vacuous: an axis
	// at 0 is unlimited, RaiseCaps skips it, and "the stop survived" would mean
	// "no raise ever happened" instead of "the raise did not cover it". Noop is
	// the budget's own report of that, so the trap is closed by assertion here
	// rather than by everyone remembering it at each call site.
	res, aerr := msg.Await(context.Background(), time.Second)
	if aerr != nil || res.Err != nil {
		t.Fatalf("await = (%+v, %v) — the raise never reached the run", res, aerr)
	}
	if res.Noop {
		t.Fatalf("the raise changed no cap, so this run says nothing about whether an "+
			"UNCOVERED overrun survives: %+v", res)
	}

	events, lerr := s.LoadEvents(context.Background(), runID)
	if lerr != nil {
		t.Fatalf("load events: %v", lerr)
	}
	return events, err
}

// lastEventData returns the data of the last event of the given type, or nil.
func lastEventData(events []*store.Event, typ store.EventType) map[string]any {
	var data map[string]any
	for _, evt := range events {
		if evt.Type == typ {
			data = evt.Data
		}
	}
	return data
}

// assertStoppedOn fails unless the run died as BUDGET_EXCEEDED on `dimension`
// with the given live figures — the whole point being that a stop the raise did
// not cover must still be there, and must name numbers that are true NOW.
func assertStoppedOn(t *testing.T, err error, events []*store.Event, dimension string, used, limit float64) {
	t.Helper()
	exceeded := lastEventData(events, store.EventBudgetExceeded)
	if err == nil {
		t.Fatalf("the run finished with %s still over its cap — a raise the operator never asked to cover it dropped the stop", dimension)
	}
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("err = %v, want a BUDGET_EXCEEDED stop on %s", err, dimension)
	}
	if exceeded == nil {
		t.Fatalf("no budget_exceeded event — the sole audit record that the cap bound")
	}
	if exceeded["dimension"] != dimension {
		t.Errorf("budget_exceeded names %v, want %s", exceeded["dimension"], dimension)
	}
	if got, _ := exceeded["used"].(float64); got != used {
		t.Errorf("used = %v, want %v", exceeded["used"], used)
	}
	if got, _ := exceeded["limit"].(float64); got != limit {
		t.Errorf("limit = %v, want %v (the cap as it now stands, not the one the overrun was measured against)", exceeded["limit"], limit)
	}
}

// TestRaiseBudget_KeepsAnOverrunTheRaiseNeverCovered is the counterweight to
// TestRaiseBudget_ArrivesInTimeForTheNodeItMustSave: a raise that DOES cover
// the spend carries the run forward, and one that does NOT must leave the stop
// standing. Three ways a raise fails to cover an overrun, plus the case where
// it lands inside the exit grace and the run walks on but has to SAY so. All
// four used to pass silently: the clear was keyed on "did ANY axis move", never
// on "is the run still over".
func TestRaiseBudget_KeepsAnOverrunTheRaiseNeverCovered(t *testing.T) {
	t.Run("another axis entirely", func(t *testing.T) {
		// Tokens are raised; cost — the axis that actually blew — is not.
		// MaxTokens must be a real cap with usage UNDER it: an axis at 0 is
		// unlimited, RaiseCaps skips it, and the test would pass vacuously
		// on a raise that never landed.
		events, err := runUncoveredOverrun(t, "run-raise-wrong-axis",
			&ir.Budget{MaxCostUSD: 10, MaxTokens: 1000},
			ir.BudgetOverrides{MaxTokens: 5000},
			map[string]any{"ok": true, "_tokens": 5, "_cost_usd": 100.0})
		assertStoppedOn(t, err, events, "cost_usd", 100, 10)
	})

	t.Run("right axis, still under water", func(t *testing.T) {
		// The operator raised the axis that blew, but not far enough. The
		// stop stands — and now names the cap as it stands (20), because a
		// limit the operator has already replaced is not a useful number to
		// be shown when deciding how much more to grant.
		events, err := runUncoveredOverrun(t, "run-raise-insufficient",
			&ir.Budget{MaxCostUSD: 10},
			ir.BudgetOverrides{MaxCostUSD: 20},
			map[string]any{"ok": true, "_cost_usd": 100.0})
		assertStoppedOn(t, err, events, "cost_usd", 100, 20)
	})

	t.Run("a raise that lands inside the exit grace still audits the overspend", func(t *testing.T) {
		// The other direction, and the reason the stop is RE-DERIVED rather
		// than merely kept: raising 10 -> 95 against a spend of 100 leaves the
		// run inside the 10% exit grace (100 < 104.5), so it should walk
		// forward and finish. What it must not do is finish SILENTLY —
		// dropping the pending overrun outright skips graceOrFailBudget, and
		// with it the budget_exit_grace event that is the only record a run
		// deliberately spent past its declared cap. "Visible in the events,
		// not discovered on the invoice" is what that event is for.
		events, err := runUncoveredOverrun(t, "run-raise-into-grace",
			&ir.Budget{MaxCostUSD: 10},
			ir.BudgetOverrides{MaxCostUSD: 95},
			map[string]any{"ok": true, "_cost_usd": 100.0})
		if err != nil {
			t.Fatalf("a raise that puts the run inside its grace must let it finish: %v", err)
		}
		grace := lastEventData(events, store.EventBudgetExitGrace)
		if grace == nil {
			t.Fatal("the run spent past its cap under the grace and recorded it nowhere")
		}
		if grace["dimension"] != "cost_usd" {
			t.Errorf("budget_exit_grace names %v, want cost_usd", grace["dimension"])
		}
		if got, _ := grace["limit"].(float64); got != 95 {
			t.Errorf("limit = %v, want 95 — the cap being overspent is the raised one", grace["limit"])
		}
	})

	t.Run("the axis the overrun was not recorded under", func(t *testing.T) {
		// One node blows tokens AND cost. checkLocked evaluates tokens
		// first and noteExceeded keeps only the first, so the pending
		// overrun reads "tokens" and the cost overrun is stored nowhere. A
		// clear that only asks "does the raise cover the RECORDED axis?"
		// therefore drops a cost overrun nobody funded — the same silent
		// over-cap completion, one variant over.
		events, err := runUncoveredOverrun(t, "run-raise-multi-axis",
			&ir.Budget{MaxTokens: 10, MaxCostUSD: 10},
			ir.BudgetOverrides{MaxTokens: 1000},
			map[string]any{"ok": true, "_tokens": 100, "_cost_usd": 100.0})
		assertStoppedOn(t, err, events, "cost_usd", 100, 10)
	})
}

// blockingRaiserExecutor blocks in the named node until its context is
// cancelled — the shape of a long agent node killed by the per-node deadline —
// and posts a raise_budget from INSIDE that node first, which is what "the
// operator raises the cap while the run is busy" actually looks like. The send
// is non-blocking so a retried node cannot wedge the executor.
type blockingRaiserExecutor struct {
	blockNode string
	ch        chan *OverrideMsg
	msg       *OverrideMsg
}

func (e *blockingRaiserExecutor) Execute(ctx context.Context, node ir.Node, _ map[string]any) (map[string]any, error) {
	if node.NodeID() == e.blockNode {
		select {
		case e.ch <- e.msg:
		default:
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return map[string]any{"ok": true}, nil
}

// TestRaiseBudget_DeadlineExpiryIsJudgedOnTheRaisedCap covers the path that
// actually kills a long node, and which the node-boundary drain never sees: the
// per-node wall-clock deadline. That expiry is classified by its own `return`
// well before execLoopAfterExec, so a raise posted during the node was still
// sitting in the channel when the verdict was taken — and the run was parked on
// a ceiling the operator had already lifted.
//
// The node's DEATH is not repairable: its deadline is frozen into the ctx when
// it starts, so no later grant moves it. The VERDICT is. With the cap raised,
// the expiry must stop being a budget stop and fall through to ordinary
// recovery dispatch, exactly as an unrelated DeadlineExceeded already does.
func TestRaiseBudget_DeadlineExpiryIsJudgedOnTheRaisedCap(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "raise_deadline_test",
		Entry: "a",
		Nodes: map[string]ir.Node{
			// "a" runs fast so a checkpoint exists before "slow" fails.
			"a":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}},
			"slow": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "slow"}},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "a", To: "slow"},
			{From: "slow", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
		// Under -race, engine startup plus the first checkpoint can exceed a
		// few hundred milliseconds before "slow" even starts. Keep the cap
		// short enough to exercise the real deadline while ensuring the
		// executor has posted its raise before that deadline is judged.
		Budget: &ir.Budget{MaxDuration: "2s"},
	}

	ch := make(chan *OverrideMsg, 2)
	msg := NewRaiseBudgetOverride(ir.BudgetOverrides{MaxDuration: "1h"}, "")
	exec := &blockingRaiserExecutor{blockNode: "slow", ch: ch, msg: msg}

	s := tmpStore(t)
	eng := New(wf, s, exec, WithOverrideChannel(ch))
	err := eng.Run(context.Background(), "run-raise-deadline", nil)

	// The node still dies — that is the frozen deadline, not something a
	// grant can undo. What must NOT survive is the budget verdict.
	if err != nil && strings.Contains(err.Error(), "budget exceeded") {
		t.Fatalf("the expiry was judged against the un-raised cap: %v", err)
	}

	events, lerr := s.LoadEvents(context.Background(), "run-raise-deadline")
	if lerr != nil {
		t.Fatalf("load events: %v", lerr)
	}
	if hasEventType(events, store.EventBudgetExceeded) {
		t.Error("a budget_exceeded event was emitted for a cap the operator had already raised")
	}

	r, lerr := s.LoadRun(context.Background(), "run-raise-deadline")
	if lerr != nil {
		t.Fatalf("load run: %v", lerr)
	}
	if r.BudgetRaises == nil || r.BudgetRaises.MaxDuration != "1h0m0s" {
		t.Fatalf("the grant did not land before the verdict: %+v", r.BudgetRaises)
	}
}

// TestRaiseBudget_AModestRaiseAlsoLiftsTheVerdict pins the half a proximity
// test got wrong. The old guard asked "is the run within 10% of its cap?", so
// the verdict only flipped once the new cap exceeded used/0.9 — measured on a
// real run (used 36001s, cap 36000s) that meant every raise below 11.112h kept
// parking it. The obvious operator gesture, "it needs a bit more, give it
// another hour", did nothing; a raise to 12h worked. Nothing in the reply, the
// event or the log said which side of that line a grant had landed on.
//
// Here the raise is 1.08x the cap — real room, comfortably inside the old
// cliff — and it must lift the verdict.
func TestRaiseBudget_AModestRaiseAlsoLiftsTheVerdict(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "raise_modest_test",
		Entry: "a",
		Nodes: map[string]ir.Node{
			"a":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}},
			"slow": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "slow"}},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "a", To: "slow"},
			{From: "slow", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
		Budget:  &ir.Budget{MaxDuration: "2s"},
	}

	// 2.16s = 1.08x. Under the old `used >= limit*0.9` test this still parked
	// the run; the operator's grant bought 160ms of real room and changed
	// nothing.
	//
	// The two numbers are COUPLED, so move them together or not at all: the
	// ratio must stay under the old cliff at used/0.9 = 1.111x, or the test
	// stops pinning the proximity guard and passes on the very code it exists
	// to refuse.
	//
	// Their 160ms difference is also this test's own wall-clock budget —
	// executor return, span.End(), the drain, and applyRaiseBudget's two store
	// writes all have to fit in it — and that budget IS exceeded in practice.
	// Measured: ~22ms worst of 37 samples under `-race` with the cores 2x
	// oversubscribed, which looked like 7x headroom; then a plain `go test
	// ./...` blew it on the first try at 443ms (used 2.443s vs the 2.16s cap).
	// Package-parallel `./...` is a heavier machine than saturated cores, and
	// no ratio inside the 1.111x cliff survives a 443ms tail — 0.08 x 2s is
	// 160ms, and buying 450ms of margin would cost a ~6s cap in a required
	// check.
	//
	// So the fixture RE-ARMS instead. The two outcomes are separable from the
	// event alone: a run whose recorded `used` reached the RAISED cap really
	// had run out of time, and a budget stop is then the correct verdict with
	// nothing to assert — retry. Below the raised cap, time was left on the
	// clock and any budget stop is the regression, whether it came from the
	// proximity guard (limit = the raised cap, used under it) or from the
	// raise never landing before the verdict (limit = the original 2s).
	const raisedCap = 2160 * time.Millisecond
	const attempts = 4

	for attempt := 1; ; attempt++ {
		runID := fmt.Sprintf("run-raise-modest-%d", attempt)
		ch := make(chan *OverrideMsg, 2)
		msg := NewRaiseBudgetOverride(ir.BudgetOverrides{MaxDuration: raisedCap.String()}, "")
		exec := &blockingRaiserExecutor{blockNode: "slow", ch: ch, msg: msg}

		s := tmpStore(t)
		eng := New(wf, s, exec, WithOverrideChannel(ch))
		err := eng.Run(context.Background(), runID, nil)

		events, lerr := s.LoadEvents(context.Background(), runID)
		if lerr != nil {
			t.Fatalf("load events: %v", lerr)
		}

		if exceeded := lastEventData(events, store.EventBudgetExceeded); exceeded != nil {
			usedNS, _ := exceeded["used"].(float64)
			used := time.Duration(usedNS)
			if used >= raisedCap {
				// Out of time under the RAISED cap: the verdict is right and
				// this attempt says nothing about how it was reached.
				if attempt == attempts {
					t.Skipf("could not set the scenario up in %d attempts: the drain kept "+
						"outlasting the %v the raise buys (last: used %v past a %v cap), so the "+
						"machine is too loaded to hold the two apart", attempts, raisedCap-2*time.Second, used, raisedCap)
				}
				continue
			}
			t.Fatalf("a raise that left %v on the clock still parked the run: used %v, "+
				"limit %v — %v", raisedCap-used, used, time.Duration(exceeded["limit"].(float64)), err)
		}
		if err != nil && strings.Contains(err.Error(), "budget exceeded") {
			t.Fatalf("a raise that bought real room still parked the run: %v", err)
		}
		return
	}
}

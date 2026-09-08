package runtime

import (
	"context"
	"errors"
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

// raiseBudgetOverrunWorkflow builds the `a -> b -> done` fixture the two
// tests below share: `b` is the node that blows the cap and posts the raise
// from inside itself, and its successor is a TERMINAL — the shape that runs
// no pre-exec check, so a pending overrun dropped at the raise is never
// re-derived anywhere and the run finishes as a success with the cap blown.
func raiseBudgetOverrunWorkflow(name string, budget *ir.Budget) *ir.Workflow {
	return &ir.Workflow{
		Name:  name,
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
		Budget:  budget,
	}
}

// runRaiseBudgetOverrun executes the shared fixture: node `b` posts `raise`
// from inside itself, then returns `spend` as its usage. Returns the stored
// run so the caller can assert the terminal status.
func runRaiseBudgetOverrun(t *testing.T, runID string, budget *ir.Budget, raise ir.BudgetOverrides, spend map[string]any) *store.Run {
	t.Helper()

	wf := raiseBudgetOverrunWorkflow(runID, budget)
	ch := make(chan *OverrideMsg, 1)
	msg := NewRaiseBudgetOverride(raise, "")

	exec := newStubExecutor()
	exec.on("a", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	})
	exec.on("b", func(_ map[string]any) (map[string]any, error) {
		ch <- msg
		out := map[string]any{"ok": true}
		for k, v := range spend {
			out[k] = v
		}
		return out, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec, WithOverrideChannel(ch))
	err := eng.Run(context.Background(), runID, nil)
	if err == nil {
		t.Fatal("run: want a budget error — the raise does not cover the overrun")
	}
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("run: err = %v, want ErrBudgetExceeded", err)
	}

	r, loadErr := s.LoadRun(context.Background(), runID)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	return r
}

// TestRaiseBudget_InsufficientRaiseStillStopsTheRun pins the first half of
// the re-derivation: the operator raises the very axis that blew, but not far
// enough. Cap 10, node spent 100, raise to 20 — still 5x over, and past the
// 10% exit grace. Dropping the pending overrun just because SOMETHING was
// raised would let the run walk into `done` and be recorded as a success with
// the cap blown, converting a hard budget failure into a silent one.
func TestRaiseBudget_InsufficientRaiseStillStopsTheRun(t *testing.T) {
	r := runRaiseBudgetOverrun(t, "run-raise-insufficient",
		&ir.Budget{MaxCostUSD: 10},
		ir.BudgetOverrides{MaxCostUSD: 20},
		map[string]any{"_cost_usd": 100.0},
	)
	if r.Status != store.RunStatusFailedResumable {
		t.Fatalf("status = %s, want failed_resumable — 100 is still past the raised cap of 20", r.Status)
	}
	if r.BudgetRaises == nil || r.BudgetRaises.MaxCostUSD != 20 {
		t.Fatalf("persisted BudgetRaises = %+v, want MaxCostUSD 20 — the grant itself still applies", r.BudgetRaises)
	}
}

// TestRaiseBudget_OtherAxisRaiseStillStopsTheRun pins the second half: the
// raise lands on an axis that is not the one that blew (tokens overran, the
// operator raised max_cost_usd). The tokens overrun is untouched by that
// grant and must still end the run.
func TestRaiseBudget_OtherAxisRaiseStillStopsTheRun(t *testing.T) {
	r := runRaiseBudgetOverrun(t, "run-raise-other-axis",
		&ir.Budget{MaxTokens: 10, MaxCostUSD: 5},
		ir.BudgetOverrides{MaxCostUSD: 50},
		map[string]any{"_tokens": 100},
	)
	if r.Status != store.RunStatusFailedResumable {
		t.Fatalf("status = %s, want failed_resumable — the tokens axis is still 10x over", r.Status)
	}
}

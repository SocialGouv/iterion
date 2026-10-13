package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// budgetPauseFixture builds a two-node chain whose nodes spend through the
// `_cost_usd` output key against a $1.0 cap with the pause policy.
func budgetPauseFixture(spendA, spendB float64) (*ir.Workflow, *stubExecutor) {
	wf := &ir.Workflow{
		Name:  "budget_pause_test",
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
		Budget:  &ir.Budget{MaxCostUSD: 1.0, OnExceeded: "pause"},
	}
	exec := newStubExecutor()
	exec.on("a", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"ok": true, "_cost_usd": spendA}, nil
	})
	exec.on("b", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"ok": true, "_cost_usd": spendB}, nil
	})
	return wf, exec
}

// The ticket's acceptance: a sequential run parks at the cap with a
// preserved checkpoint, a resume without the raise re-parks (never
// converts the park into a failure), an operator raise resumes it, and it
// finishes on the same run id. The overrun is 20% past the cap — outside
// the bounded exit grace, whose forward-only walk would otherwise deliver
// the tail — and the pause anchors on the not-yet-executed terminal node,
// so no completed node is repaid.
func TestBudgetPauseParksReParksAndResumesAfterRaise(t *testing.T) {
	wf, exec := budgetPauseFixture(0.6, 0.6)
	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-budget-pause", nil)
	if !errors.Is(err, ErrRunPausedOperator) {
		t.Fatalf("the pause policy must park, not fail: %v", err)
	}
	r, err := s.LoadRun(context.Background(), "run-budget-pause")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != store.RunStatusPausedOperator {
		t.Fatalf("status %s, want paused_operator", r.Status)
	}
	events, err := s.LoadEvents(context.Background(), "run-budget-pause")
	if err != nil {
		t.Fatal(err)
	}
	if !hasEventType(events, store.EventBudgetExceeded) {
		t.Error("expected budget_exceeded event")
	}
	var paused bool
	for _, ev := range events {
		if ev.Type == store.EventRunPaused && ev.Data["reason"] == "budget_cap_run" {
			paused = true
		}
	}
	if !paused {
		t.Error("expected run_paused with reason budget_cap_run")
	}

	// A resume without the raise re-parks: the operator's first reflex
	// must not convert the park into a failure.
	eng2 := New(wf, s, exec)
	err = eng2.Resume(context.Background(), "run-budget-pause", nil)
	if !errors.Is(err, ErrRunPausedOperator) {
		t.Fatalf("a resume without the raise must re-park, not fail: %v", err)
	}
	r, err = s.LoadRun(context.Background(), "run-budget-pause")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != store.RunStatusPausedOperator {
		t.Fatalf("status %s after the refused resume, want still paused_operator", r.Status)
	}

	// The raise is absolute: $2.0 covers the $1.2 spent plus the tail.
	if err := s.PatchRunSteering(context.Background(), "run-budget-pause", nil, &store.RunBudgetRaises{MaxCostUSD: 2.0}); err != nil {
		t.Fatalf("raise: %v", err)
	}
	eng3 := New(wf, s, exec)
	if err := eng3.Resume(context.Background(), "run-budget-pause", nil); err != nil {
		t.Fatalf("resume after the raise: %v", err)
	}
	r, err = s.LoadRun(context.Background(), "run-budget-pause")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != store.RunStatusFinished {
		t.Fatalf("status %s after the raised resume, want done", r.Status)
	}
}

// The 90% hard limit parks a policy-pause run too: it PRECEDES every cap
// crossing a node-sized step can produce, so a policy that only paused at
// 100% would almost never engage.
func TestBudgetPauseParksAtTheHardLimit(t *testing.T) {
	wf, exec := budgetPauseFixture(0.95, 0.1)
	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-budget-pause-hard", nil)
	if !errors.Is(err, ErrRunPausedOperator) {
		t.Fatalf("the pause policy must park at the 90%% block, not fail: %v", err)
	}
	r, err := s.LoadRun(context.Background(), "run-budget-pause-hard")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != store.RunStatusPausedOperator {
		t.Fatalf("status %s, want paused_operator", r.Status)
	}
}

// The default policy is unchanged: without on_exceeded the cap ends the
// run failed_resumable, checkpoint kept, resume after a raise.
func TestBudgetFailPolicyIsTheDefault(t *testing.T) {
	wf, exec := budgetPauseFixture(0.6, 0.6)
	wf.Budget.OnExceeded = ""
	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-budget-fail", nil)
	if err == nil || errors.Is(err, ErrRunPausedOperator) {
		t.Fatalf("the default policy must fail the run, got %v", err)
	}
	if !strings.Contains(err.Error(), "budget exceeded") {
		t.Errorf("expected 'budget exceeded' in error, got: %v", err)
	}
	r, err := s.LoadRun(context.Background(), "run-budget-fail")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != store.RunStatusFailedResumable {
		t.Fatalf("status %s, want failed_resumable", r.Status)
	}
}

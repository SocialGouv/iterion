package runtime

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/expr"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// cutCrossingExecutor stands in for a sandboxed claw crossing: the cut node
// runs until the engine's own context ends it, then reports what the far
// side saw — a killed process — and never the context error that cut it.
// Every other node answers at once.
type cutCrossingExecutor struct {
	cutNode  string
	attempts atomic.Int32
}

func (x *cutCrossingExecutor) Execute(ctx context.Context, node ir.Node, _ map[string]any) (map[string]any, error) {
	if node.NodeID() != x.cutNode {
		return map[string]any{"ok": true}, nil
	}
	x.attempts.Add(1)
	<-ctx.Done()
	return nil, errors.New("sandbox runner exited: signal: killed")
}

// A node the run's duration budget cuts is a budget stop, however the
// crossing words its death: the engine set the deadline and knows it fired.
// Read from the crossing's chain alone, the cut fell to recovery and the
// node was re-run — the very thing the budget stop exists to prevent.
func TestABudgetCutTheCrossingReportsInItsOwnWordsIsABudgetStop(t *testing.T) {
	x := &cutCrossingExecutor{cutNode: "slow"}
	err := New(internalTimeoutWorkflow(300*time.Millisecond), tmpStore(t), x).Run(context.Background(), "run-cut-budget", nil)
	var rt *RuntimeError
	if !errors.As(err, &rt) || rt.Code != ErrCodeBudgetExceeded {
		t.Fatalf("the run ended with %v; want a %s stop — the engine cut the node at its own deadline and knows it, whatever the crossing said", err, ErrCodeBudgetExceeded)
	}
	if n := x.attempts.Load(); n != 1 {
		t.Errorf("the cut node ran %d times; want once — a budget stop never re-runs the node it cut", n)
	}
}

// cutSiblingWorkflow: a fan_out_all router spawns two branches — one routes
// into a typed fail, the other runs a crossing that the first failure
// retires and that reports the kill in its own words.
func cutSiblingWorkflow(t *testing.T) *ir.Workflow {
	t.Helper()
	stop, err := expr.Parse("true")
	if err != nil {
		t.Fatalf("parse compute expr: %v", err)
	}
	return &ir.Workflow{
		Name:  "cut_sibling",
		Entry: "fork",
		Nodes: map[string]ir.Node{
			"fork":  &ir.RouterNode{BaseNode: ir.BaseNode{ID: "fork"}, RouterMode: ir.RouterFanOutAll},
			"guard": &ir.ComputeNode{BaseNode: ir.BaseNode{ID: "guard"}, Exprs: []*ir.ComputeExpr{{Key: "stop", AST: stop, Raw: "true"}}},
			"slow":  &ir.AgentNode{BaseNode: ir.BaseNode{ID: "slow"}},
			"collect": &ir.ComputeNode{
				BaseNode:  ir.BaseNode{ID: "collect"},
				AwaitMode: ir.AwaitWaitAll,
				Exprs:     []*ir.ComputeExpr{{Key: "stop", AST: stop, Raw: "true"}},
			},
			"refuse": &ir.FailNode{BaseNode: ir.BaseNode{ID: "refuse"}, Code: "PLAN_BUDGET_EXHAUSTED"},
			"done":   &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "fork", To: "guard"},
			{From: "fork", To: "slow"},
			{From: "guard", To: "refuse", Condition: "stop"},
			{From: "guard", To: "collect", IsElse: true},
			{From: "slow", To: "collect"},
			{From: "collect", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
		Budget:  &ir.Budget{MaxParallelBranches: 2},
	}
}

// A sibling the first failure retires is a stopped branch, however the
// crossing words its death: the branch's own context ended, and the branch
// knows it. Read from the crossing's chain alone, the stopped sibling was a
// failure of its own, and its untyped error erased the typed code the run
// should carry.
func TestAStoppedSiblingThatReportsTheCutInItsOwnWordsIsNotAFailureOfItsOwn(t *testing.T) {
	x := &cutCrossingExecutor{cutNode: "slow"}
	s := tmpStore(t)
	err := New(cutSiblingWorkflow(t), s, x).Run(context.Background(), "run-cut-sibling", nil)
	var rt *RuntimeError
	if !errors.As(err, &rt) || rt.Code != "PLAN_BUDGET_EXHAUSTED" {
		t.Fatalf("the run ended with %v; want the typed code of the branch that failed by itself — the retired sibling is not a failure of its own", err)
	}
	run, loadErr := s.LoadRun(context.Background(), "run-cut-sibling")
	if loadErr != nil {
		t.Fatalf("load run: %v", loadErr)
	}
	if run.FailureCode != "PLAN_BUDGET_EXHAUSTED" {
		t.Errorf("persisted failure_code = %q, want PLAN_BUDGET_EXHAUSTED", run.FailureCode)
	}
	if n := x.attempts.Load(); n != 1 {
		t.Errorf("the retired node ran %d times; want once", n)
	}
}

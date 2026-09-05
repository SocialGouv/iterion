package runtime

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/expr"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// branchTypedFailWorkflow: a fan_out_all router spawns ONE branch, which
// routes into a typed `resumable: true` fail node. The branch arm is a
// different code path from the trunk's failRunDeliberate.
//
// One branch on purpose: a sibling retired by the first failure contributes
// its own context.Canceled error, and commonBranchFailureCode only keeps a
// code every failed branch agrees on — so a second branch would make the
// aggregate code a race between the two goroutines.
func branchTypedFailWorkflow(t *testing.T) *ir.Workflow {
	t.Helper()
	stopExpr, err := expr.Parse("true")
	if err != nil {
		t.Fatalf("parse compute expr: %v", err)
	}
	return &ir.Workflow{
		Name:  "branch_typed_fail",
		Entry: "fork",
		Nodes: map[string]ir.Node{
			"fork": &ir.RouterNode{BaseNode: ir.BaseNode{ID: "fork"}, RouterMode: ir.RouterFanOutAll},
			"guard": &ir.ComputeNode{
				BaseNode: ir.BaseNode{ID: "guard"},
				Exprs:    []*ir.ComputeExpr{{Key: "stop", AST: stopExpr, Raw: "true"}},
			},
			"collect": &ir.ComputeNode{
				BaseNode:  ir.BaseNode{ID: "collect"},
				AwaitMode: ir.AwaitWaitAll,
				Exprs:     []*ir.ComputeExpr{{Key: "stop", AST: stopExpr, Raw: "true"}},
			},
			"refuse": &ir.FailNode{
				BaseNode:  ir.BaseNode{ID: "refuse"},
				Code:      "PLAN_BUDGET_EXHAUSTED",
				Resumable: true,
			},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "fork", To: "guard"},
			{From: "guard", To: "refuse", Condition: "stop"},
			{From: "guard", To: "collect", IsElse: true},
			{From: "collect", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
		Budget:  &ir.Budget{MaxParallelBranches: 2},
	}
}

// A typed fail reached INSIDE a fan-out branch cannot put its code on the
// run — the collector decides the branch's fate — and `resumable: true`
// cannot be honoured there either. Both are defensible, but they happened
// in SILENCE: the trunk logs a WARN when it cannot honour resumability,
// docs/dsl.md said `code:` is "stamped on the run's failure_code" with no
// caveat, and an alert sink keyed on failure_code saw nothing (Rbca4c2).
func TestTypedFailInBranchIsLoud(t *testing.T) {
	var logBuf bytes.Buffer
	s := tmpStore(t)
	eng := New(branchTypedFailWorkflow(t), s, newStubExecutor(),
		WithLogger(iterlog.New(iterlog.LevelWarn, &logBuf)))

	err := eng.Run(context.Background(), "run-branch-fail", nil)
	if err == nil {
		t.Fatal("run succeeded; a branch routes to a fail node")
	}

	// The declared code travels as a TYPED branch error, so the
	// collector's commonBranchFailureCode carries it onto the run — which
	// is what an alert sink or a merge-gate notice keyed on failure_code
	// actually reads.
	var rtErr *RuntimeError
	if !errors.As(err, &rtErr) {
		t.Fatalf("the run error is not a *RuntimeError, so it carries no code at all: %v", err)
	}
	if rtErr.Code != "PLAN_BUDGET_EXHAUSTED" {
		t.Errorf("run error code = %q, want PLAN_BUDGET_EXHAUSTED", rtErr.Code)
	}
	run, loadErr := s.LoadRun(context.Background(), "run-branch-fail")
	if loadErr != nil {
		t.Fatalf("load run: %v", loadErr)
	}
	if run.FailureCode != "PLAN_BUDGET_EXHAUSTED" {
		t.Errorf("persisted failure_code = %q, want PLAN_BUDGET_EXHAUSTED", run.FailureCode)
	}

	logged := logBuf.String()
	if !strings.Contains(logged, "refuse") {
		t.Errorf("no WARN naming the fail node; the operator gets no signal at all:\n%s", logged)
	}
	if !strings.Contains(logged, "resumable") {
		t.Errorf("the WARN does not say that `resumable: true` was not honoured:\n%s", logged)
	}
}

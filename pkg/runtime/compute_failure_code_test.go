package runtime

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/expr"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A compute node that cannot evaluate its expression is the one failure a
// resume provably cannot cure: no LLM, no shell, and inputs that come from a
// checkpoint that does not move. It used to land on the run as the generic
// EXECUTION_FAILED — the code the automatic-resume classification reads as
// "a backend fault a later attempt can outlast" — so the cloud redelivery
// re-ran it seven times in ten minutes (run 01a07804), one pod each.
//
// Asserted on the persisted RUN, not on the returned error: the redelivery
// path reads the document, and that is the copy that decided the loop.
func TestComputeExpressionFailureIsTypedExpressionFailedOnTheRun(t *testing.T) {
	bad, err := expr.Parse("sum('not an array')")
	if err != nil {
		t.Fatalf("parse compute expr: %v", err)
	}
	wf := &ir.Workflow{
		Name:  "compute_expr_failure",
		Entry: "derive",
		Nodes: map[string]ir.Node{
			"derive": &ir.ComputeNode{
				BaseNode: ir.BaseNode{ID: "derive"},
				Exprs:    []*ir.ComputeExpr{{Key: "total", AST: bad, Raw: "sum('not an array')"}},
			},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges:   []*ir.Edge{{From: "derive", To: "done"}},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}

	s := tmpStore(t)
	if err := New(wf, s, newStubExecutor()).Run(context.Background(), "run-expr", nil); err == nil {
		t.Fatal("run succeeded; the compute expression cannot evaluate")
	}
	run, err := s.LoadRun(context.Background(), "run-expr")
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if run.FailureCode != store.FailureExpressionFailed {
		t.Errorf("failure_code = %q, want %s — a generic EXECUTION_FAILED is read as retryable and re-burns a pod per attempt",
			run.FailureCode, store.FailureExpressionFailed)
	}
	if run.Status != store.RunStatusFailedResumable {
		t.Errorf("status = %q, want failed_resumable (the checkpoint stays for an operator resume after the fix)", run.Status)
	}
	if run.Error == "" {
		t.Error("run.Error is empty — the operator cannot tell which expression refused")
	}
}

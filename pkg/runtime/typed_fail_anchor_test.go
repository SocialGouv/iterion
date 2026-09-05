package runtime

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/expr"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// resumableFailWorkflow: a guard routes into a `resumable: true` fail
// node. This is the shape a phase-budget guard has.
func resumableFailWorkflow(t *testing.T, entry string) *ir.Workflow {
	t.Helper()
	stopExpr, err := expr.Parse("true")
	if err != nil {
		t.Fatalf("parse compute expr: %v", err)
	}
	return &ir.Workflow{
		Name:  "resumable_fail",
		Entry: entry,
		Nodes: map[string]ir.Node{
			"guard": &ir.ComputeNode{
				BaseNode: ir.BaseNode{ID: "guard"},
				Exprs:    []*ir.ComputeExpr{{Key: "stop", AST: stopExpr, Raw: "true"}},
			},
			"refuse": &ir.FailNode{
				BaseNode:  ir.BaseNode{ID: "refuse"},
				Code:      "PLAN_BUDGET_EXHAUSTED",
				Resumable: true,
			},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "guard", To: "refuse", Condition: "stop"},
			{From: "guard", To: "done", IsElse: true},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}
}

// The checkpoint of a resumable fail node must anchor on the GUARD, not on
// the fail node: resumeFromFailure starts execLoop at cp.NodeID, so an
// anchor on the fail node re-dispatches the fail node and reproduces the
// same outcome with nothing re-decided (R345e7d).
func TestResumableFailAnchorsOnItsGuard(t *testing.T) {
	s := tmpStore(t)
	eng := New(resumableFailWorkflow(t, "guard"), s, newStubExecutor())
	if err := eng.Run(context.Background(), "run-anchor", nil); err == nil {
		t.Fatal("run succeeded; the graph routes to a fail node")
	}

	run, err := s.LoadRun(context.Background(), "run-anchor")
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if run.Status != store.RunStatusFailedResumable {
		t.Fatalf("status = %s, want failed_resumable", run.Status)
	}
	if run.Checkpoint == nil {
		t.Fatal("no checkpoint")
	}
	if run.Checkpoint.NodeID != "guard" {
		t.Errorf("checkpoint anchored on %q, want guard", run.Checkpoint.NodeID)
	}
	if run.FailureCode != "PLAN_BUDGET_EXHAUSTED" {
		t.Errorf("failure_code = %q, want PLAN_BUDGET_EXHAUSTED", run.FailureCode)
	}
}

// When no single predecessor can be named, `resumable: true` cannot be
// honoured — a checkpoint on the fail node would only replay this refusal.
// The run ends TERMINAL and the log says why, rather than offering the
// operator a resume that silently does nothing.
func TestResumableFailWithNoGuardDegradesToTerminal(t *testing.T) {
	s := tmpStore(t)
	// The fail node IS the entry: nothing routed into it.
	eng := New(resumableFailWorkflow(t, "refuse"), s, newStubExecutor())
	if err := eng.Run(context.Background(), "run-no-anchor", nil); err == nil {
		t.Fatal("run succeeded; the entry is a fail node")
	}

	run, err := s.LoadRun(context.Background(), "run-no-anchor")
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if run.Status != store.RunStatusFailed {
		t.Errorf("status = %s, want failed — a resumable checkpoint here could only replay the same refusal", run.Status)
	}
	// The DIAGNOSIS still lands: only the resumability degrades.
	if run.FailureCode != "PLAN_BUDGET_EXHAUSTED" {
		t.Errorf("failure_code = %q, want PLAN_BUDGET_EXHAUSTED", run.FailureCode)
	}
}

// resumableFailAnchor's refusal reasons are what the operator reads in the
// log, so each one must be reached by the shape it names.
func TestResumableFailAnchorRefusals(t *testing.T) {
	eng := New(resumableFailWorkflow(t, "guard"), tmpStore(t), newStubExecutor())

	t.Run("no incoming edge recorded", func(t *testing.T) {
		rs := eng.newRunState("r", nil)
		if got, why := eng.resumableFailAnchor(rs, "refuse"); got != "" || why == "" {
			t.Errorf("anchor = %q reason = %q, want no anchor with a reason", got, why)
		}
	})

	t.Run("several branches converged", func(t *testing.T) {
		rs := eng.newRunState("r", nil)
		rs.selectedIncoming = map[string][]store.IncomingEdge{
			"refuse": {{From: "guard", To: "refuse"}, {From: "other", To: "refuse"}},
		}
		got, why := eng.resumableFailAnchor(rs, "refuse")
		if got != "" || why == "" {
			t.Errorf("anchor = %q reason = %q, want no anchor with a reason", got, why)
		}
	})

	t.Run("predecessor not in the workflow", func(t *testing.T) {
		rs := eng.newRunState("r", nil)
		rs.selectedIncoming = map[string][]store.IncomingEdge{
			"refuse": {{From: "ghost", To: "refuse"}},
		}
		if got, why := eng.resumableFailAnchor(rs, "refuse"); got != "" || why == "" {
			t.Errorf("anchor = %q reason = %q, want no anchor with a reason", got, why)
		}
	})

	t.Run("terminal predecessor", func(t *testing.T) {
		rs := eng.newRunState("r", nil)
		rs.selectedIncoming = map[string][]store.IncomingEdge{
			"refuse": {{From: "done", To: "refuse"}},
		}
		if got, why := eng.resumableFailAnchor(rs, "refuse"); got != "" || why == "" {
			t.Errorf("anchor = %q reason = %q, want no anchor with a reason", got, why)
		}
	})

	t.Run("the ordinary shape resolves", func(t *testing.T) {
		rs := eng.newRunState("r", nil)
		rs.selectedIncoming = map[string][]store.IncomingEdge{
			"refuse": {{From: "guard", To: "refuse"}},
		}
		got, why := eng.resumableFailAnchor(rs, "refuse")
		if got != "guard" || why != "" {
			t.Errorf("anchor = %q reason = %q, want guard with no reason", got, why)
		}
	})
}

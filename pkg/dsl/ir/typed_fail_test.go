package ir

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

const typedFailSource = `compute gate:
  expr:
    pct: "42"

fail plan_exhausted:
  code: PLAN_BUDGET_EXHAUSTED
  message: "planning used {{outputs.gate.pct}}% of the budget"
  resumable: true

fail not_actionable:
  code: LOT_NOT_ACTIONABLE

workflow typed_fail:
  entry: gate
  gate -> plan_exhausted when too_slow
  gate -> not_actionable else
`

// A deliberate refusal must reach the RUN as a code and a reason (#739):
// before this, every `-> fail` read "workflow reached fail node" with a
// fixed FAIL_NODE code, so the operator had to open the run's artifacts to
// learn which of a bot's several refusals had fired.
func TestCompileTypedFailNodes(t *testing.T) {
	res := compileFile(t, typedFailSource)
	for _, d := range res.Diagnostics {
		if d.Severity == SeverityError {
			t.Fatalf("unexpected compile error: %s %s", d.Code, d.Message)
		}
	}

	exhausted, ok := res.Workflow.Nodes["plan_exhausted"].(*FailNode)
	if !ok {
		t.Fatalf("plan_exhausted is %T, want *FailNode", res.Workflow.Nodes["plan_exhausted"])
	}
	if exhausted.Code != "PLAN_BUDGET_EXHAUSTED" {
		t.Errorf("code = %q, want PLAN_BUDGET_EXHAUSTED", exhausted.Code)
	}
	if !exhausted.Resumable {
		t.Error("resumable = false, want true")
	}
	if exhausted.Message == nil || len(exhausted.Message.Refs) != 1 {
		t.Fatalf("message refs = %v, want the one {{outputs.gate.pct}} ref", exhausted.Message)
	}

	// Several typed refusals coexist: one node per reason is the point.
	other, ok := res.Workflow.Nodes["not_actionable"].(*FailNode)
	if !ok {
		t.Fatalf("not_actionable is %T, want *FailNode", res.Workflow.Nodes["not_actionable"])
	}
	if other.Code != "LOT_NOT_ACTIONABLE" || other.Resumable {
		t.Errorf("not_actionable = {code:%q resumable:%v}, want {LOT_NOT_ACTIONABLE false}", other.Code, other.Resumable)
	}

	// The bare `fail` target keeps its untyped behaviour.
	bare, ok := res.Workflow.Nodes["fail"].(*FailNode)
	if !ok {
		t.Fatalf("the implicit fail node is %T, want *FailNode", res.Workflow.Nodes["fail"])
	}
	if bare.Code != "" || bare.Message != nil || bare.Resumable {
		t.Errorf("the implicit fail node was typed: %+v", bare)
	}
}

// C247 refuses a code that is not machine-readable. The value is persisted
// as the run's failure_code and read by the CLI, the studio, the gate
// notice and the alert sinks — a lowercase one would reach all of them.
func TestC247RejectsAMalformedFailCode(t *testing.T) {
	for _, bad := range []string{"plan_budget_exhausted", "Plan-Budget", "9LIVES"} {
		src := "compute gate:\n  expr:\n    pct: \"42\"\n\nfail bad_code:\n  code: \"" + bad + "\"\n\nworkflow c247:\n  entry: gate\n  gate -> bad_code\n"
		res := compileFile(t, src)
		found := false
		for _, d := range res.Diagnostics {
			if d.Code == DiagInvalidFailCode {
				found = true
				if d.Severity != SeverityError {
					t.Errorf("C247 severity = %s, want error", d.Severity)
				}
				if !strings.Contains(d.Message, "UPPER_SNAKE") {
					t.Errorf("C247 message does not say what a code must look like: %s", d.Message)
				}
			}
		}
		if !found {
			t.Errorf("code %q produced no C247: %v", bad, res.Diagnostics)
		}
		// The node still compiles — dropping it would turn a naming
		// mistake into an unreachable-target cascade.
		node, ok := res.Workflow.Nodes["bad_code"].(*FailNode)
		if !ok {
			t.Fatalf("bad_code is %T, want *FailNode", res.Workflow.Nodes["bad_code"])
		}
		if node.Code != "" {
			t.Errorf("a refused code was kept: %q", node.Code)
		}
	}
}

// A `fail <name>:` colliding with another declaration must be caught, not
// silently resolved by map order.
func TestTypedFailNameCollides(t *testing.T) {
	src := "compute gate:\n  expr:\n    pct: \"42\"\n\nfail gate:\n  code: BOOM\n\nworkflow dup:\n  entry: gate\n  gate -> done\n"
	res := compileFile(t, src)
	found := false
	for _, d := range res.Diagnostics {
		if d.Code == DiagDuplicateNodeID {
			found = true
		}
	}
	if !found {
		t.Errorf("a fail node shadowing a compute produced no duplicate-id diagnostic: %v", res.Diagnostics)
	}
}

// The reserved names stay reserved: `fail fail:` must not redefine the
// implicit terminal.
func TestTypedFailCannotUseAReservedName(t *testing.T) {
	src := "compute gate:\n  expr:\n    pct: \"42\"\n\nfail fail:\n  code: BOOM\n\nworkflow reserved:\n  entry: gate\n  gate -> fail\n"
	res := parser.Parse("test.bot", src)
	found := false
	for _, d := range res.Diagnostics {
		if d.Code == parser.DiagReservedName {
			found = true
		}
	}
	if !found {
		t.Errorf("`fail fail:` produced no reserved-name diagnostic: %v", res.Diagnostics)
	}
	if len(res.File.Fails) != 0 {
		t.Errorf("the reserved declaration was kept: %+v", res.File.Fails)
	}
	if _, isFail := ast.ReservedTargets["fail"]; !isFail {
		t.Error("`fail` is no longer a reserved target")
	}
}

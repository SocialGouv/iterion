package ir

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Compute node parsing & compilation
// ---------------------------------------------------------------------------

const computeSrc = `
schema verdict:
  approved: bool
  family: string

schema streak:
  stop: bool

agent dummy:
  model: "claude-opus-4-7"
  output: verdict

compute streak_check:
  output: streak
  expr:
    stop: "input.approved && loop.l.previous_output.approved && input.family != loop.l.previous_output.family"

workflow w:
  entry: dummy
  dummy -> streak_check as l(3) with {
    approved: "{{outputs.dummy.approved}}",
    family: "{{outputs.dummy.family}}"
  }
  streak_check -> done when stop
  streak_check -> dummy as l(3)
`

func TestCompute_Compiles(t *testing.T) {
	wf := mustCompile(t, computeSrc)

	cn, ok := wf.Nodes["streak_check"].(*ComputeNode)
	if !ok {
		t.Fatalf("expected streak_check to be *ComputeNode, got %T", wf.Nodes["streak_check"])
	}
	if cn.OutputSchema != "streak" {
		t.Errorf("OutputSchema = %q, want %q", cn.OutputSchema, "streak")
	}
	if len(cn.Exprs) != 1 {
		t.Fatalf("expected 1 expr, got %d", len(cn.Exprs))
	}
	if cn.Exprs[0].Key != "stop" {
		t.Errorf("expr key = %q, want %q", cn.Exprs[0].Key, "stop")
	}
	if cn.Exprs[0].AST == nil {
		t.Error("expected non-nil parsed AST")
	}
	// Verify the AST references span all expected namespaces.
	refs := cn.Exprs[0].AST.Refs()
	seen := make(map[string]bool)
	for _, r := range refs {
		seen[r.Namespace] = true
	}
	for _, ns := range []string{"input", "loop"} {
		if !seen[ns] {
			t.Errorf("expected expression to reference namespace %q (Refs: %v)", ns, refs)
		}
	}
}

// ---------------------------------------------------------------------------
// `when "<expression>"` parsing & compilation
// ---------------------------------------------------------------------------

const whenExprSrc = `
schema v:
  approved: bool

agent reviewer:
  model: "claude-opus-4-7"
  output: v

workflow w:
  entry: reviewer
  reviewer -> done when "approved && loop.l.previous_output.approved" as l(3)
  reviewer -> reviewer as l(3)
`

func TestWhenExpression_Compiles(t *testing.T) {
	wf := mustCompile(t, whenExprSrc)
	var found bool
	for _, e := range wf.Edges {
		if e.From == "reviewer" && e.To == "done" {
			if e.Expression == nil {
				t.Errorf("expected parsed expression on edge reviewer->done, got nil")
			}
			if !strings.Contains(e.ExpressionSrc, "approved && loop.l.previous_output.approved") {
				t.Errorf("ExpressionSrc = %q, expected to contain the source", e.ExpressionSrc)
			}
			found = true
		}
	}
	if !found {
		t.Error("did not find reviewer->done edge")
	}
}

// ---------------------------------------------------------------------------
// Bad expression diagnostics
// ---------------------------------------------------------------------------

const badExprSrc = `
schema v:
  approved: bool

agent reviewer:
  model: "claude-opus-4-7"
  output: v

workflow w:
  entry: reviewer
  reviewer -> done when "approved && &&"
`

func TestWhenExpression_BadProducesDiag(t *testing.T) {
	r := compileFile(t, badExprSrc)
	hasC032 := false
	for _, d := range r.Diagnostics {
		if d.Code == DiagBadExpr {
			hasC032 = true
		}
	}
	if !hasC032 {
		t.Errorf("expected DiagBadExpr (C032), got diagnostics: %v", r.Diagnostics)
	}
}

// ---------------------------------------------------------------------------
// Compute with no expressions diagnostics
// ---------------------------------------------------------------------------

const computeNoExprSrc = `
schema s:
  stop: bool

compute c:
  output: s

workflow w:
  entry: c
  c -> done
`

func TestCompute_NoExprProducesDiag(t *testing.T) {
	r := compileFile(t, computeNoExprSrc)
	hasC031 := false
	for _, d := range r.Diagnostics {
		if d.Code == DiagComputeNoExpr {
			hasC031 = true
		}
	}
	if !hasC031 {
		t.Errorf("expected DiagComputeNoExpr (C031), got diagnostics: %v", r.Diagnostics)
	}
}

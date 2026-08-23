package ir

import (
	"os"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Helper: expectDiag asserts at least one diagnostic with the given code.
// ---------------------------------------------------------------------------

func expectDiag(t *testing.T, r *CompileResult, code DiagCode) {
	t.Helper()
	for _, d := range r.Diagnostics {
		if d.Code == code {
			return
		}
	}
	t.Errorf("expected diagnostic %s, got: %v", code, r.Diagnostics)
}

func expectNoDiag(t *testing.T, r *CompileResult, code DiagCode) {
	t.Helper()
	for _, d := range r.Diagnostics {
		if d.Code == code {
			t.Errorf("unexpected diagnostic %s: %s", code, d.Message)
		}
	}
}

// ---------------------------------------------------------------------------
// C009 — session: inherit/fork at convergence point
// ---------------------------------------------------------------------------

func TestValidateInheritAtConvergence_Rejected(t *testing.T) {
	src := `
schema s:
  ok: bool

prompt sys:
  System.

prompt usr:
  User.

agent a1:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

agent a2:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

router r1:
  mode: fan_out_all

agent after_convergence:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr
  session: inherit
  await: wait_all

workflow test:
  entry: r1
  r1 -> a1
  r1 -> a2
  a1 -> after_convergence with { result_a: "{{outputs.a1}}" }
  a2 -> after_convergence with { result_b: "{{outputs.a2}}" }
  after_convergence -> done
`
	r := compileFile(t, src)
	expectDiag(t, r, DiagSessionAfterConvergence)
}

func TestValidateForkAtConvergence_Rejected(t *testing.T) {
	src := `
schema s:
  ok: bool

prompt sys:
  System.

prompt usr:
  User.

agent a1:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

agent a2:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

router r1:
  mode: fan_out_all

agent after_convergence:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr
  session: fork
  await: wait_all

workflow test:
  entry: r1
  r1 -> a1
  r1 -> a2
  a1 -> after_convergence with { result_a: "{{outputs.a1}}" }
  a2 -> after_convergence with { result_b: "{{outputs.a2}}" }
  after_convergence -> done
`
	r := compileFile(t, src)
	expectDiag(t, r, DiagSessionAfterConvergence)
}

func TestValidatePersistAtConvergence_Allowed(t *testing.T) {
	src := `
schema s:
  ok: bool

prompt sys:
  System.

prompt usr:
  User.

agent a1:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

agent a2:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

router r1:
  mode: fan_out_all

agent after_convergence:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr
  session: persist
  await: wait_all

workflow test:
  entry: r1
  r1 -> a1
  r1 -> a2
  a1 -> after_convergence with { result_a: "{{outputs.a1}}" }
  a2 -> after_convergence with { result_b: "{{outputs.a2}}" }
  after_convergence -> done
`
	r := compileFile(t, src)
	expectNoDiag(t, r, DiagSessionAfterConvergence)
	expectNoDiag(t, r, DiagPersistInFanOut)
}

func TestValidatePersistInFanOutWithLoop_Rejected(t *testing.T) {
	src := `
schema s:
  ok: bool

prompt sys:
  System.

prompt usr:
  User.

agent a:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr
  session: persist

agent b:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

router r1:
  mode: fan_out_all

agent join:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr
  await: wait_all

workflow test:
  entry: r1
  r1 -> a
  r1 -> b
  a -> b as retry(3)
  a -> join
  b -> join
  join -> done
`
	r := compileFile(t, src)
	expectDiag(t, r, DiagPersistInFanOut)
}

func TestValidatePersistAfterImplicitJoin_Allowed(t *testing.T) {
	src := `
schema s:
  ok: bool

prompt sys:
  System.

prompt usr:
  User.

agent a1:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

agent a2:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

router r1:
  mode: fan_out_all

agent merge:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

agent writer:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr
  session: persist

workflow test:
  entry: r1
  r1 -> a1
  r1 -> a2
  a1 -> merge
  a2 -> merge
  merge -> writer
  writer -> done
`
	r := compileFile(t, src)
	expectNoDiag(t, r, DiagPersistInFanOut)
}

func TestValidatePersistInFanOutBody_Rejected(t *testing.T) {
	src := `
schema s:
  ok: bool

prompt sys:
  System.

prompt usr:
  User.

agent a1:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr
  session: persist

agent a2:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

router r1:
  mode: fan_out_all

agent join:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr
  await: wait_all

workflow test:
  entry: r1
  r1 -> a1
  r1 -> a2
  a1 -> join
  a2 -> join
  join -> done
`
	r := compileFile(t, src)
	expectDiag(t, r, DiagPersistInFanOut)
}

func TestValidatePersistOnLoopHeadInFanOut_Rejected(t *testing.T) {
	// Sharing execBranchBodyNodes used to inherit an elected-join carve-out:
	// a -> a as retry elects a as the join, so persist on a compiled clean
	// even though a is a fan-out target. C243 must still fire.
	src := `
schema s:
  ok: bool

prompt sys:
  System.

prompt usr:
  User.

agent a:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr
  session: persist

agent b:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

router r1:
  mode: fan_out_all

agent join:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr
  await: wait_all

workflow test:
  entry: r1
  r1 -> a
  r1 -> b
  a -> a as retry(3) when ok
  a -> join else
  b -> join
  join -> done
`
	r := compileFile(t, src)
	expectDiag(t, r, DiagPersistInFanOut)
}

func TestValidatePersistInLLMMultiBody_Rejected(t *testing.T) {
	src := `
schema s:
  ok: bool

prompt sys:
  System.

prompt usr:
  User.

agent a1:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr
  session: persist

agent a2:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

router r1:
  mode: llm
  model: "test-model"
  multi: true

agent join:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr
  await: wait_all

workflow test:
  entry: r1
  r1 -> a1
  r1 -> a2
  a1 -> join
  a2 -> join
  join -> done
`
	r := compileFile(t, src)
	expectDiag(t, r, DiagPersistInFanOut)
}

func TestValidatePersistOnLLMSingle_Allowed(t *testing.T) {
	src := `
schema s:
  ok: bool

prompt sys:
  System.

prompt usr:
  User.

agent a1:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr
  session: persist

agent a2:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

router r1:
  mode: llm
  model: "test-model"

workflow test:
  entry: r1
  r1 -> a1
  r1 -> a2
  a1 -> done
  a2 -> done
`
	r := compileFile(t, src)
	expectNoDiag(t, r, DiagPersistInFanOut)
}

func TestValidateFreshAtConvergence_Allowed(t *testing.T) {
	src := `
schema s:
  ok: bool

prompt sys:
  System.

prompt usr:
  User.

agent a1:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

agent a2:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

router r1:
  mode: fan_out_all

agent after_convergence:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr
  session: fresh
  await: wait_all

workflow test:
  entry: r1
  r1 -> a1
  r1 -> a2
  a1 -> after_convergence with { result_a: "{{outputs.a1}}" }
  a2 -> after_convergence with { result_b: "{{outputs.a2}}" }
  after_convergence -> done
`
	r := compileFile(t, src)
	expectNoDiag(t, r, DiagSessionAfterConvergence)
}

// ---------------------------------------------------------------------------
// C010 — multiple unconditional edges
// ---------------------------------------------------------------------------

func TestValidateMultipleDefaultEdges_Rejected(t *testing.T) {
	src := `
schema s:
  ok: bool

prompt sys:
  System.

prompt usr:
  User.

agent a:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

agent b:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

workflow test:
  entry: a
  a -> b
  a -> done
`
	r := compileFile(t, src)
	expectDiag(t, r, DiagMultipleDefaultEdges)
}

func TestValidateMultipleDefaultEdges_RouterFanOutAllowed(t *testing.T) {
	src := `
schema s:
  ok: bool

prompt sys:
  System.

prompt usr:
  User.

agent a1:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

agent a2:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

router r1:
  mode: fan_out_all

workflow test:
  entry: r1
  r1 -> a1
  r1 -> a2
  a1 -> done
  a2 -> done
`
	r := compileFile(t, src)
	expectNoDiag(t, r, DiagMultipleDefaultEdges)
}

func TestValidateMultipleDefaultEdges_RouterRoundRobinAllowed(t *testing.T) {
	src := `
schema s:
  ok: bool

prompt sys:
  System.

prompt usr:
  User.

agent a1:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

agent a2:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

router r1:
  mode: round_robin

workflow test:
  entry: r1
  r1 -> a1
  r1 -> a2
  a1 -> done
  a2 -> done
`
	r := compileFile(t, src)
	expectNoDiag(t, r, DiagMultipleDefaultEdges)
}

func TestValidateRoundRobinTooFewEdges(t *testing.T) {
	src := `
schema s:
  ok: bool

prompt sys:
  System.

prompt usr:
  User.

agent a1:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

router r1:
  mode: round_robin

workflow test:
  entry: r1
  r1 -> a1
  a1 -> done
`
	r := compileFile(t, src)
	expectDiag(t, r, DiagRoundRobinTooFewEdges)
}

// ---------------------------------------------------------------------------
// C011 — ambiguous conditions
// ---------------------------------------------------------------------------

func TestValidateAmbiguousCondition_Rejected(t *testing.T) {
	src := `
schema s:
  approved: bool

prompt sys:
  System.

prompt usr:
  User.

judge check:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

agent fix_a:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

agent fix_b:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

workflow test:
  entry: check
  check -> fix_a when approved
  check -> fix_b when approved
`
	r := compileFile(t, src)
	expectDiag(t, r, DiagAmbiguousCondition)
}

func TestValidateConditions_NoAmbiguity(t *testing.T) {
	src := `
schema s:
  approved: bool

prompt sys:
  System.

prompt usr:
  User.

judge check:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

agent fix:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

workflow test:
  entry: check
  check -> done when approved
  check -> fix when not approved
  fix -> check
`
	r := compileFile(t, src)
	expectNoDiag(t, r, DiagAmbiguousCondition)
}

// ---------------------------------------------------------------------------
// C012 — missing fallback
// ---------------------------------------------------------------------------

func TestValidateMissingFallback_Rejected(t *testing.T) {
	src := `
schema s:
  approved: bool

prompt sys:
  System.

prompt usr:
  User.

judge check:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

workflow test:
  entry: check
  check -> done when approved
`
	r := compileFile(t, src)
	expectDiag(t, r, DiagMissingFallback)
}

func TestValidateMissingFallback_WithDefault(t *testing.T) {
	src := `
schema s:
  approved: bool

prompt sys:
  System.

prompt usr:
  User.

judge check:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

agent fix:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

workflow test:
  entry: check
  check -> done when approved
  check -> fix
  fix -> check
`
	r := compileFile(t, src)
	expectNoDiag(t, r, DiagMissingFallback)
}

func TestValidateMissingFallback_ExhaustiveBoolAllowed(t *testing.T) {
	src := `
schema s:
  approved: bool

prompt sys:
  System.

prompt usr:
  User.

judge check:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

agent fix:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

workflow test:
  entry: check
  check -> done when approved
  check -> fix when not approved
  fix -> check
`
	r := compileFile(t, src)
	expectNoDiag(t, r, DiagMissingFallback)
}

// ---------------------------------------------------------------------------
// C013 — condition field not boolean
// ---------------------------------------------------------------------------

func TestValidateConditionNotBool_Rejected(t *testing.T) {
	src := `
schema s:
  reason: string

prompt sys:
  System.

prompt usr:
  User.

judge check:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

workflow test:
  entry: check
  check -> done when reason
  check -> fail
`
	r := compileFile(t, src)
	expectDiag(t, r, DiagConditionNotBool)
}

// ---------------------------------------------------------------------------
// C014 — condition field not found in schema
// ---------------------------------------------------------------------------

func TestValidateConditionFieldNotFound_Rejected(t *testing.T) {
	src := `
schema s:
  ok: bool

prompt sys:
  System.

prompt usr:
  User.

judge check:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

workflow test:
  entry: check
  check -> done when nonexistent_field
  check -> fail
`
	r := compileFile(t, src)
	expectDiag(t, r, DiagConditionFieldNotFound)
}

func TestValidateConditionField_Valid(t *testing.T) {
	src := `
schema s:
  approved: bool

prompt sys:
  System.

prompt usr:
  User.

judge check:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

agent fix:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

workflow test:
  entry: check
  check -> done when approved
  check -> fix when not approved
  fix -> check
`
	r := compileFile(t, src)
	expectNoDiag(t, r, DiagConditionFieldNotFound)
	expectNoDiag(t, r, DiagConditionNotBool)
}

// ---------------------------------------------------------------------------
// C016 — unreachable nodes
// ---------------------------------------------------------------------------

func TestValidateUnreachableNode_Rejected(t *testing.T) {
	src := `
schema s:
  ok: bool

prompt sys:
  System.

prompt usr:
  User.

agent a:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

agent orphan:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

workflow test:
  entry: a
  a -> done
  orphan -> done
`
	r := compileFile(t, src)
	expectDiag(t, r, DiagUnreachableNode)
}

func TestValidateReachable_AllNodesConnected(t *testing.T) {
	src := `
schema s:
  ok: bool

prompt sys:
  System.

prompt usr:
  User.

agent a:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

agent b:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

workflow test:
  entry: a
  a -> b
  b -> done
`
	r := compileFile(t, src)
	expectNoDiag(t, r, DiagUnreachableNode)
}

// ---------------------------------------------------------------------------
// C017 — outputs.<node>.history but node not in a loop
// ---------------------------------------------------------------------------

func TestValidateHistoryRefNotInLoop_Rejected(t *testing.T) {
	src := `
schema s:
  ok: bool

prompt sys:
  Check {{outputs.a.history}}.

prompt usr:
  User.

agent a:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

workflow test:
  entry: a
  a -> done
`
	r := compileFile(t, src)
	expectDiag(t, r, DiagHistoryRefNotInLoop)
}

func TestValidateHistoryRef_InLoopAllowed(t *testing.T) {
	src := `
schema s:
  approved: bool

prompt sys:
  Check {{outputs.check.history}}.

prompt usr:
  User.

judge check:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

agent fix:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

workflow test:
  entry: check
  check -> done when approved
  check -> fix when not approved as refine(5)
  fix -> check
`
	r := compileFile(t, src)
	expectNoDiag(t, r, DiagHistoryRefNotInLoop)
}

// ---------------------------------------------------------------------------
// Combined: reference fixture must still compile without validation errors
// ---------------------------------------------------------------------------

func TestValidateReferenceFixturesClean(t *testing.T) {
	// Re-run fixture compilation and ensure no new validation errors.
	// This uses the same fixtures as TestCompileReferenceFixture.
	fixtures := []string{
		"pr_refine_single_model.bot",
		"pr_refine_dual_model_parallel.bot",
		"pr_refine_dual_model_parallel_compliance.bot",
		"recipe_benchmark.bot",
		"ci_fix_until_green.bot",
	}

	newCodes := []DiagCode{
		DiagSessionAfterConvergence,
		DiagMultipleDefaultEdges,
		DiagAmbiguousCondition,
		DiagMissingFallback,
		DiagConditionNotBool,
		DiagConditionFieldNotFound,
		DiagUnreachableNode,
		DiagHistoryRefNotInLoop,
	}

	for _, fixture := range fixtures {
		t.Run(fixture, func(t *testing.T) {
			path := "../testdata/" + fixture
			src := readFixture(t, path)
			if src == "" {
				t.Skip("fixture not found")
			}
			r := compileFile(t, src)
			for _, code := range newCodes {
				expectNoDiag(t, r, code)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// C021 — llm router with fewer than 2 outgoing edges
// ---------------------------------------------------------------------------

func TestValidateLLMRouterTooFewEdges(t *testing.T) {
	src := `
prompt sys:
  System.

prompt usr:
  User.

schema s:
  ok: bool

agent a1:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

router r1:
  mode: llm
  model: "test-model"

workflow test:
  entry: r1
  r1 -> a1
  a1 -> done
`
	r := compileFile(t, src)
	expectDiag(t, r, DiagLLMRouterTooFewEdges)
}

// ---------------------------------------------------------------------------
// C022 — llm router edge has when condition
// ---------------------------------------------------------------------------

func TestValidateLLMRouterConditionEdge(t *testing.T) {
	src := `
prompt sys:
  System.

prompt usr:
  User.

schema s:
  ok: bool

agent a1:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

agent a2:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

router r1:
  mode: llm
  model: "test-model"

workflow test:
  entry: r1
  r1 -> a1 when ok
  r1 -> a2
  a1 -> done
  a2 -> done
`
	r := compileFile(t, src)
	expectDiag(t, r, DiagLLMRouterConditionEdge)
}

// TestValidateLLMRouterExpressionConditionEdge guards that the
// DiagLLMRouterConditionEdge check fires for the EXPRESSION form of a
// condition (`when "expr"`, which lands in e.Expression) and not only the
// simple boolean-field form (e.Condition). The validator must use
// IsConditional() so an LLM router can't smuggle a `when "..."` past it.
func TestValidateLLMRouterExpressionConditionEdge(t *testing.T) {
	src := `
prompt sys:
  System.

prompt usr:
  User.

prompt route_sys:
  Route wisely.

schema s:
  ok: bool

agent a1:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

agent a2:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

router r1:
  mode: llm
  model: "test-model"
  system: route_sys

workflow test:
  entry: r1
  r1 -> a1 when "ok"
  r1 -> a2
  a1 -> done
  a2 -> done
`
	r := compileFile(t, src)
	expectDiag(t, r, DiagLLMRouterConditionEdge)
}

// ---------------------------------------------------------------------------
// LLM router — valid configuration (no diagnostics)
// ---------------------------------------------------------------------------

func TestValidateLLMRouterValid(t *testing.T) {
	src := `
prompt sys:
  System.

prompt usr:
  User.

prompt route_sys:
  Route wisely.

schema s:
  ok: bool

agent a1:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

agent a2:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

router r1:
  mode: llm
  model: "test-model"
  system: route_sys

workflow test:
  entry: r1
  r1 -> a1
  r1 -> a2
  a1 -> done
  a2 -> done
`
	r := compileFile(t, src)
	expectNoDiag(t, r, DiagLLMRouterTooFewEdges)
	expectNoDiag(t, r, DiagLLMRouterConditionEdge)

	// Verify the compiled node has the right fields.
	if r.Workflow == nil {
		t.Fatal("expected non-nil workflow")
	}
	n := r.Workflow.Nodes["r1"]
	if n == nil {
		t.Fatal("expected node r1")
	}
	rn := n.(*RouterNode)
	if rn.RouterMode != RouterLLM {
		t.Errorf("expected RouterLLM, got %v", rn.RouterMode)
	}
	if rn.Model != "test-model" {
		t.Errorf("expected model test-model, got %s", rn.Model)
	}
	if rn.SystemPrompt != "route_sys" {
		t.Errorf("expected system prompt route_sys, got %s", rn.SystemPrompt)
	}
}

// ---------------------------------------------------------------------------
// LLM router — property order independence (model before mode)
// ---------------------------------------------------------------------------

func TestValidateLLMRouterPropertyOrderIndependence(t *testing.T) {
	// model: appears before mode: — must still compile correctly as an LLM router.
	src := `
prompt sys:
  System.

prompt usr:
  User.

schema s:
  ok: bool

agent a1:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

agent a2:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

router r1:
  model: "test-model"
  mode: llm
  system: sys

workflow test:
  entry: r1
  r1 -> a1
  r1 -> a2
  a1 -> done
  a2 -> done
`
	r := compileFile(t, src)
	expectNoDiag(t, r, DiagLLMRouterTooFewEdges)
	expectNoDiag(t, r, DiagRouterLLMOnlyProperty)

	if r.Workflow == nil {
		t.Fatal("expected non-nil workflow")
	}
	n := r.Workflow.Nodes["r1"]
	if n == nil {
		t.Fatal("expected node r1")
	}
	rn := n.(*RouterNode)
	if rn.RouterMode != RouterLLM {
		t.Errorf("expected RouterLLM, got %v", rn.RouterMode)
	}
	if rn.Model != "test-model" {
		t.Errorf("expected model test-model, got %s", rn.Model)
	}
}

// ---------------------------------------------------------------------------
// C023 — LLM-only property on non-llm router
// ---------------------------------------------------------------------------

func TestValidateRouterLLMOnlyProperty(t *testing.T) {
	src := `
prompt sys:
  System.

prompt usr:
  User.

schema s:
  ok: bool

agent a1:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

agent a2:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

router r1:
  mode: fan_out_all
  model: "some-model"

workflow test:
  entry: r1
  r1 -> a1
  r1 -> a2
  a1 -> done
  a2 -> done
`
	r := compileFile(t, src)
	expectDiag(t, r, DiagRouterLLMOnlyProperty)
}

// ---------------------------------------------------------------------------
// C024 — invalid reasoning_effort value
// ---------------------------------------------------------------------------

func TestValidateReasoningEffort_Invalid(t *testing.T) {
	src := `
schema s:
  ok: bool

prompt sys:
  System.

prompt usr:
  User.

agent a1:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

workflow test:
  entry: a1
  a1 -> done
`
	r := compileFile(t, src)
	// Inject an invalid reasoning effort after compilation to test the IR validator.
	r.Workflow.Nodes["a1"].(*AgentNode).ReasoningEffort = "ultra"
	// Re-run validation.
	c := &compiler{}
	c.validateReasoningEffort(r.Workflow)
	if !hasDiag(c.diags, DiagInvalidReasoningEffort) {
		t.Error("expected diagnostic C024 for invalid reasoning_effort")
	}
}

func TestValidateReasoningEffort_Valid(t *testing.T) {
	src := `
schema s:
  ok: bool

prompt sys:
  System.

prompt usr:
  User.

agent a1:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr
  reasoning_effort: high

workflow test:
  entry: a1
  a1 -> done
`
	r := compileFile(t, src)
	expectNoDiag(t, r, DiagInvalidReasoningEffort)
}

// ---------------------------------------------------------------------------
// C122 — invalid per-node timeout
// ---------------------------------------------------------------------------

func TestValidateNodeTimeout_Invalid(t *testing.T) {
	src := `
prompt sys:
  System.

prompt usr:
  User.

agent a1:
  model: "m"
  system: sys
  user: usr
  timeout: "banana"

workflow test:
  entry: a1
  a1 -> done
`
	r := compileFile(t, src)
	expectDiag(t, r, DiagInvalidNodeTimeout)
}

func TestValidateNodeTimeout_Valid(t *testing.T) {
	src := `
prompt sys:
  System.

prompt usr:
  User.

agent a1:
  model: "m"
  system: sys
  user: usr
  timeout: "20m"

workflow test:
  entry: a1
  a1 -> done
`
	r := compileFile(t, src)
	expectNoDiag(t, r, DiagInvalidNodeTimeout)
}

// An unset bare env ref expands to "" and is deferred to the runtime
// resolver rather than flagged at compile time.
func TestValidateNodeTimeout_EnvSubstDeferred(t *testing.T) {
	src := `
prompt sys:
  System.

prompt usr:
  User.

agent a1:
  model: "m"
  system: sys
  user: usr
  timeout: "${NODE_TIMEOUT}"

workflow test:
  entry: a1
  a1 -> done
`
	r := compileFile(t, src)
	expectNoDiag(t, r, DiagInvalidNodeTimeout)
}

func TestResolveEffortLiteral(t *testing.T) {
	tests := []struct {
		name     string
		literal  string
		envKey   string
		envValue string
		want     string
	}{
		{name: "enum literal passes through", literal: "max", want: "max"},
		{name: "empty stays empty", literal: "", want: ""},
		{name: "env-subst with valid default",
			literal:  "${ITERION_TEST_EFFORT:-max}",
			envKey:   "ITERION_TEST_EFFORT",
			envValue: "",
			want:     "max"},
		{name: "env wins over default",
			literal:  "${ITERION_TEST_EFFORT:-max}",
			envKey:   "ITERION_TEST_EFFORT",
			envValue: "low",
			want:     "low"},
		{name: "invalid expanded value clamps to empty",
			literal:  "${ITERION_TEST_EFFORT:-ultra}",
			envKey:   "ITERION_TEST_EFFORT",
			envValue: "",
			want:     ""},
		{name: "bare env var unset returns empty",
			literal:  "${ITERION_TEST_EFFORT}",
			envKey:   "ITERION_TEST_EFFORT",
			envValue: "",
			want:     ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.envKey != "" {
				t.Setenv(tt.envKey, tt.envValue)
			}
			if got := ResolveEffortLiteral(tt.literal); got != tt.want {
				t.Errorf("ResolveEffortLiteral(%q) = %q, want %q", tt.literal, got, tt.want)
			}
		})
	}
}

func TestLooksLikeModelSpec(t *testing.T) {
	yes := []string{
		"gpt-5.6-sol",
		"gpt-5.6-terra",
		"gpt-5.6-luna",
		"openai-codex/gpt-5.6-sol",
		"anthropic/claude-opus-5",
		"claude-opus-5",
		"openai/gpt-5.5",
		"xai/grok-4",
		"opus",
		"sonnet",
		"o1",
		"o3-mini",
		"a:b",
		"bedrock/us.amazon.nova-pro-v1:0",
		"anthropic.claude-sonnet-4-20250514-v1:0",
		"ollama/llama3:8b",
	}
	for _, s := range yes {
		if !LooksLikeModelSpec(s) {
			t.Errorf("LooksLikeModelSpec(%q) = false, want true", s)
		}
	}
	no := []string{
		"",
		"victor",
		"root",
		"/home/victor",
		"../secret",
		"$HOME",
		"${VAR}",
		":leading",
		"hello world",
		strings.Repeat("a-", 70),
	}
	for _, s := range no {
		if LooksLikeModelSpec(s) {
			t.Errorf("LooksLikeModelSpec(%q) = true, want false", s)
		}
	}
}

func TestResolveModelLiteral(t *testing.T) {
	const antKey = "sk-ant-api03-abcdefghijklmnopqrstuvwxyz012345"
	const ghpKey = "ghp_abcdefghijklmnopqrstuvwxyz0123456789"
	tests := []struct {
		name     string
		literal  string
		envKey   string
		envValue string
		want     string
		// shapeOK asserts LooksLikeModelSpec(envValue) so the name
		// gate (not the shape check) is what refused the expansion.
		shapeOK bool
	}{
		{name: "plain spec passes through", literal: "openai-codex/gpt-5.6-sol", want: "openai-codex/gpt-5.6-sol"},
		{name: "empty stays empty", literal: "", want: ""},
		{name: "env-subst with valid default",
			literal:  "${ITERION_TEST_MODEL:-openai-codex/gpt-5.6-sol}",
			envKey:   "ITERION_TEST_MODEL",
			envValue: "",
			want:     "openai-codex/gpt-5.6-sol"},
		{name: "env wins over default when the var contains MODEL",
			literal:  "${ITERION_TEST_MODEL:-openai-codex/gpt-5.6-sol}",
			envKey:   "ITERION_TEST_MODEL",
			envValue: "openai-codex/gpt-5.6-terra",
			want:     "openai-codex/gpt-5.6-terra"},
		{name: "non-model expansion clamps to empty",
			literal:  "${ITERION_TEST_MODEL:-/not/a/model}",
			envKey:   "ITERION_TEST_MODEL",
			envValue: "",
			want:     ""},
		{name: "username-shaped expansion is not a model",
			literal:  "${ITERION_TEST_MODEL}",
			envKey:   "ITERION_TEST_MODEL",
			envValue: "victor",
			want:     ""},
		{name: "bare env var unset returns empty",
			literal:  "${ITERION_TEST_MODEL}",
			envKey:   "ITERION_TEST_MODEL",
			envValue: "",
			want:     ""},
		{name: "ANTHROPIC_API_KEY is refused even when the value looks like a model",
			literal:  "${ANTHROPIC_API_KEY}",
			envKey:   "ANTHROPIC_API_KEY",
			envValue: antKey,
			want:     "",
			shapeOK:  true},
		{name: "GITHUB_TOKEN is refused even when the value looks like a model",
			literal:  "${GITHUB_TOKEN}",
			envKey:   "GITHUB_TOKEN",
			envValue: ghpKey,
			want:     "",
			shapeOK:  true},
		{name: "hyphenated secret value is refused",
			literal:  "${GITHUB_TOKEN}",
			envKey:   "GITHUB_TOKEN",
			envValue: "xoxb-1234-abcd-efghijkl",
			want:     "",
			shapeOK:  true},
		{name: "name gate refuses a model-shaped value from a non-MODEL var",
			literal:  "${SOME_OTHER_VAR}",
			envKey:   "SOME_OTHER_VAR",
			envValue: "gpt-5.6-sol",
			want:     "",
			shapeOK:  true},
		{name: "LITELLM_MODEL_API_KEY is a credential even though it contains MODEL",
			literal:  "${LITELLM_MODEL_API_KEY}",
			envKey:   "LITELLM_MODEL_API_KEY",
			envValue: "gpt-5.6-sol",
			want:     "",
			shapeOK:  true},
		{name: "OPENROUTER_MODEL_KEY is a credential even though it contains MODEL",
			literal:  "${OPENROUTER_MODEL_KEY}",
			envKey:   "OPENROUTER_MODEL_KEY",
			envValue: "gpt-5.6-sol",
			want:     "",
			shapeOK:  true},
		{name: "ITERION_VIBE_MODEL_CLAUDE still expands",
			literal:  "${ITERION_VIBE_MODEL_CLAUDE:-openai-codex/gpt-5.6-sol}",
			envKey:   "ITERION_VIBE_MODEL_CLAUDE",
			envValue: "anthropic/claude-opus-5",
			want:     "anthropic/claude-opus-5"},
		{name: "colonated default still expands",
			literal:  "${ITERION_TEST_MODEL:-ollama/llama3:8b}",
			envKey:   "ITERION_TEST_MODEL",
			envValue: "",
			want:     "ollama/llama3:8b"},
		{name: "nested ${${X_MODEL}} is refused",
			literal:  "${${X_MODEL}}",
			envKey:   "X_MODEL",
			envValue: "ANTHROPIC_API_KEY",
			want:     ""},
		{name: "nested ${$X_MODEL} is refused",
			literal:  "${$X_MODEL}",
			envKey:   "X_MODEL",
			envValue: "ANTHROPIC_API_KEY",
			want:     ""},
		{name: "nested ${A:-${B_MODEL:-c}} is refused (more than one $)",
			literal: "${A:-${B_MODEL:-openai-codex/gpt-5.6-sol}}",
			want:    ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.envKey != "" {
				t.Setenv(tt.envKey, tt.envValue)
			}
			if tt.shapeOK && !LooksLikeModelSpec(tt.envValue) {
				t.Fatalf("precondition: LooksLikeModelSpec(%q) should be true so the name gate is the thing under test", tt.envValue)
			}
			if got := ResolveModelLiteral(tt.literal); got != tt.want {
				t.Errorf("ResolveModelLiteral(%q) = %q, want %q", tt.literal, got, tt.want)
			}
		})
	}
}

func TestModelEnvNameOK(t *testing.T) {
	yes := []string{
		"CODEX_MODEL",
		"ANTHROPIC_MODEL",
		"ITERION_VIBE_MODEL_CLAUDE",
		"ITERION_TEST_MODEL",
	}
	for _, name := range yes {
		if !modelEnvNameOK(name) {
			t.Errorf("modelEnvNameOK(%q) = false, want true", name)
		}
	}
	no := []string{
		"ANTHROPIC_API_KEY",
		"GITHUB_TOKEN",
		"LITELLM_MODEL_API_KEY",
		"OPENROUTER_MODEL_KEY",
		"MODEL_REGISTRY_TOKEN",
		"SOME_OTHER_VAR",
	}
	for _, name := range no {
		if modelEnvNameOK(name) {
			t.Errorf("modelEnvNameOK(%q) = true, want false", name)
		}
	}
}

func TestModelEnvRefNames(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"${ANTHROPIC_API_KEY}", []string{"ANTHROPIC_API_KEY"}},
		{"${ITERION_TEST_MODEL:-openai-codex/gpt-5.6-sol}", []string{"ITERION_TEST_MODEL"}},
		{"$CODEX_MODEL", []string{"CODEX_MODEL"}},
		{"${A:-${B_MODEL:-c}}", []string{"A", "B_MODEL"}},
		{"plain spec", nil},
	}
	for _, tt := range tests {
		got := modelEnvRefNames(tt.in)
		if len(got) != len(tt.want) {
			t.Errorf("modelEnvRefNames(%q) = %v, want %v", tt.in, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("modelEnvRefNames(%q) = %v, want %v", tt.in, got, tt.want)
				break
			}
		}
	}
}

// Env-substituted reasoning_effort cannot be evaluated at compile time:
// the C024 check must defer to runtime instead of erroring on a literal
// it doesn't recognise.
func TestValidateReasoningEffort_EnvSubstDeferred(t *testing.T) {
	src := `
schema s:
  ok: bool

prompt sys:
  System.

prompt usr:
  User.

agent a1:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr
  reasoning_effort: "${VIBE_EFFORT:-max}"

workflow test:
  entry: a1
  a1 -> done
`
	r := compileFile(t, src)
	expectNoDiag(t, r, DiagInvalidReasoningEffort)
}

func readFixture(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}

// ---------------------------------------------------------------------------
// C029 — outputs ref to non-existent node
// ---------------------------------------------------------------------------

func TestValidateRefUnknownNode(t *testing.T) {
	src := `
schema s:
  ok: bool

prompt sys:
  Use {{outputs.ghost.ok}} here.

prompt usr:
  User.

agent a:
  model: "m"
  output: s
  system: sys
  user: usr

workflow test:
  entry: a
  a -> done
`
	r := compileFile(t, src)
	expectDiag(t, r, DiagUnknownRefNode)
}

func TestValidateRefKnownNode_OK(t *testing.T) {
	src := `
schema s:
  ok: bool

prompt sys:
  System.

prompt usr:
  Use {{outputs.a.ok}} here.

agent a:
  model: "m"
  output: s
  system: sys
  user: usr

agent b:
  model: "m"
  output: s
  system: sys
  user: usr

workflow test:
  entry: a
  a -> b
  b -> done
`
	r := compileFile(t, src)
	expectNoDiag(t, r, DiagUnknownRefNode)
}

// ---------------------------------------------------------------------------
// C031 — field not in output schema
// ---------------------------------------------------------------------------

func TestValidateRefFieldNotInSchema(t *testing.T) {
	src := `
schema s:
  ok: bool

prompt sys:
  System.

prompt usr:
  Use {{outputs.a.missing_field}} here.

agent a:
  model: "m"
  output: s
  system: sys
  user: usr

agent b:
  model: "m"
  output: s
  system: sys
  user: usr

workflow test:
  entry: a
  a -> b
  b -> done
`
	r := compileFile(t, src)
	expectDiag(t, r, DiagRefFieldNotInSchema)
}

func TestValidateRefFieldInSchema_OK(t *testing.T) {
	src := `
schema s:
  ok: bool

prompt sys:
  System.

prompt usr:
  Use {{outputs.a.ok}} here.

agent a:
  model: "m"
  output: s
  system: sys
  user: usr

agent b:
  model: "m"
  output: s
  system: sys
  user: usr

workflow test:
  entry: a
  a -> b
  b -> done
`
	r := compileFile(t, src)
	expectNoDiag(t, r, DiagRefFieldNotInSchema)
}

// ---------------------------------------------------------------------------
// C032 — field access on node without output schema (warning)
// ---------------------------------------------------------------------------

func TestValidateRefNodeNoSchema_Warn(t *testing.T) {
	src := `
schema s:
  ok: bool

prompt sys:
  System.

prompt usr:
  Use {{outputs.a.some_field}} here.

agent a:
  model: "m"
  system: sys
  user: usr

agent b:
  model: "m"
  output: s
  system: sys
  user: usr

workflow test:
  entry: a
  a -> b
  b -> done
`
	r := compileFile(t, src)
	expectDiag(t, r, DiagRefNodeNoSchema)
}

func TestValidateRefNodeNoSchema_WholeOutput_NoWarn(t *testing.T) {
	src := `
schema s:
  ok: bool

prompt sys:
  System.

prompt usr:
  Use {{outputs.a}} here.

agent a:
  model: "m"
  system: sys
  user: usr

agent b:
  model: "m"
  output: s
  system: sys
  user: usr

workflow test:
  entry: a
  a -> b
  b -> done
`
	r := compileFile(t, src)
	expectNoDiag(t, r, DiagRefNodeNoSchema)
}

// ---------------------------------------------------------------------------
// C033 — undeclared variable
// ---------------------------------------------------------------------------

func TestValidateRefUndeclaredVar(t *testing.T) {
	src := `
schema s:
  ok: bool

prompt sys:
  Use {{vars.unknown}} here.

prompt usr:
  User.

agent a:
  model: "m"
  output: s
  system: sys
  user: usr

workflow test:
  entry: a
  a -> done
`
	r := compileFile(t, src)
	expectDiag(t, r, DiagUndeclaredVar)
}

func TestValidateRefDeclaredVar_OK(t *testing.T) {
	src := `
vars:
  my_var: string

schema s:
  ok: bool

prompt sys:
  Use {{vars.my_var}} here.

prompt usr:
  User.

agent a:
  model: "m"
  output: s
  system: sys
  user: usr

workflow test:
  entry: a
  a -> done
`
	r := compileFile(t, src)
	expectNoDiag(t, r, DiagUndeclaredVar)
}

// ---------------------------------------------------------------------------
// C035 — unknown artifact
// ---------------------------------------------------------------------------

func TestValidateRefUnknownArtifact(t *testing.T) {
	src := `
schema s:
  ok: bool

prompt sys:
  Use {{artifacts.missing}} here.

prompt usr:
  User.

agent a:
  model: "m"
  output: s
  system: sys
  user: usr

workflow test:
  entry: a
  a -> done
`
	r := compileFile(t, src)
	expectDiag(t, r, DiagUnknownArtifact)
}

func TestValidateRefPublishedArtifact_OK(t *testing.T) {
	src := `
schema s:
  ok: bool

prompt sys:
  System.

prompt usr:
  Use {{artifacts.result}} here.

agent a:
  model: "m"
  output: s
  publish: result
  system: sys
  user: usr

agent b:
  model: "m"
  output: s
  system: sys
  user: usr

workflow test:
  entry: a
  a -> b
  b -> done
`
	r := compileFile(t, src)
	expectNoDiag(t, r, DiagUnknownArtifact)
}

// ---------------------------------------------------------------------------
// C034 — input ref field not in the validated namespace
// ---------------------------------------------------------------------------

func TestValidateRefInputFieldNotInSchema(t *testing.T) {
	src := `
schema s:
  ok: bool

prompt sys:
  Use {{input.missing_field}} here.

prompt usr:
  User.

agent a:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

workflow test:
  entry: a
  a -> done
`
	r := compileFile(t, src)
	expectDiag(t, r, DiagInputFieldNotInSchema)
}

func TestValidateRefInputFieldInSchema_OK(t *testing.T) {
	src := `
schema s:
  ok: bool

prompt sys:
  Use {{input.ok}} here.

prompt usr:
  User.

agent a:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

workflow test:
  entry: a
  a -> done
`
	r := compileFile(t, src)
	expectNoDiag(t, r, DiagInputFieldNotInSchema)
}

func TestValidateRefInputFieldNoInputSchema_Skip(t *testing.T) {
	src := `
schema s:
  ok: bool

prompt sys:
  Use {{input.anything}} here.

prompt usr:
  User.

agent a:
  model: "m"
  output: s
  system: sys
  user: usr

workflow test:
  entry: a
  a -> done
`
	r := compileFile(t, src)
	expectNoDiag(t, r, DiagInputFieldNotInSchema)
}

// C034 on an edge with-mapping validates {{input.x}} against the SOURCE
// node's OUTPUT schema — the payload available when the edge fires — not
// the source input schema and not run-level inputs / vars.

const edgeInputRefSrc = `
schema src_in:
  only_in: string

schema src_out:
  produced: string

schema dst_in:
  from_out: string
  from_in: string
  from_run: string

prompt sys:
  System.

prompt usr:
  User.

agent src:
  model: "m"
  input: src_in
  output: src_out
  system: sys
  user: usr

agent dst:
  model: "m"
  input: dst_in
  output: src_out
  system: sys
  user: usr

vars:
  reviewer: string

workflow test:
  entry: src
  src -> dst with {
    from_out: "{{input.produced}}",
    from_in: "{{input.only_in}}",
    from_run: "{{input.reviewer}}"
  }
  dst -> done
`

func TestValidateRefInputOnEdge_SourceOutputOK(t *testing.T) {
	r := compileFile(t, edgeInputRefSrc)
	for _, d := range r.Diagnostics {
		if d.Code != DiagInputFieldNotInSchema {
			continue
		}
		if strings.Contains(d.Message, `field "produced"`) {
			t.Errorf("C034 on source-output field produced: %s", d.Message)
		}
	}
}

func TestValidateRefInputOnEdge_SourceInputOnly_C034(t *testing.T) {
	r := compileFile(t, edgeInputRefSrc)
	var msg string
	for _, d := range r.Diagnostics {
		if d.Code == DiagInputFieldNotInSchema && strings.Contains(d.Message, `field "only_in"`) {
			msg = d.Message
			break
		}
	}
	if msg == "" {
		t.Fatalf("expected C034 for {{input.only_in}} on the edge, got: %v", r.Diagnostics)
	}
	for _, want := range []string{
		"output schema",
		"source node",
		"edge with-mappings",
		"input schema, not its output",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("C034 message missing %q: %s", want, msg)
		}
	}
}

func TestValidateRefInputOnEdge_RunInputOnly_C034VarsHint(t *testing.T) {
	r := compileFile(t, edgeInputRefSrc)
	var msg string
	for _, d := range r.Diagnostics {
		if d.Code == DiagInputFieldNotInSchema && strings.Contains(d.Message, `field "reviewer"`) {
			msg = d.Message
			break
		}
	}
	if msg == "" {
		t.Fatalf("expected C034 for {{input.reviewer}} on the edge, got: %v", r.Diagnostics)
	}
	for _, want := range []string{
		"output schema",
		"source node",
		"edge with-mappings",
		"{{vars.reviewer}}",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("C034 message missing %q: %s", want, msg)
		}
	}
}

func TestValidateRefInputOnEdge_RouterPassThrough_NoSchemaSkip(t *testing.T) {
	src := `
schema payload:
  data: string

prompt sys:
  System.

prompt usr:
  User.

router distribute:
  mode: fan_out_all

agent analyzer:
  model: "m"
  input: payload
  output: payload
  system: sys
  user: usr
  readonly: true

workflow test:
  entry: distribute
  distribute -> analyzer with { data: "{{input.data}}" }
  analyzer -> done
`
	r := compileFile(t, src)
	expectNoDiag(t, r, DiagInputFieldNotInSchema)
	expectNoDiag(t, r, DiagRefNodeNoSchema)
}

func TestValidateRefInputOnEdge_MidGraphRouter_NoIncomingWith_C032(t *testing.T) {
	src := `
schema payload:
  topic: string

prompt sys:
  System.

prompt usr:
  User.

agent seed:
  model: "m"
  output: payload
  system: sys
  user: usr

router pick:
  mode: condition

agent show:
  model: "m"
  input: payload
  output: payload
  system: sys
  user: usr

vars:
  topic: string

workflow test:
  entry: seed
  seed -> pick
  pick -> show with { topic: "{{input.topic}}" }
  show -> done
`
	r := compileFile(t, src)
	expectNoDiag(t, r, DiagInputFieldNotInSchema)
	expectDiag(t, r, DiagRefNodeNoSchema)
	var msg string
	for _, d := range r.Diagnostics {
		if d.Code == DiagRefNodeNoSchema && strings.Contains(d.Message, `field "topic"`) {
			msg = d.Message
			break
		}
	}
	if msg == "" {
		t.Fatalf("expected C032 for mid-graph router {{input.topic}}, got: %v", r.Diagnostics)
	}
	for _, want := range []string{
		"does not pass it through",
		"incoming with-mapping",
		"not run inputs",
		"{{vars.topic}}",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("C032 message missing %q: %s", want, msg)
		}
	}
}

func TestValidateRefInputOnEdge_MidGraphRouter_IncomingWith_OK(t *testing.T) {
	src := `
schema payload:
  topic: string

prompt sys:
  System.

prompt usr:
  User.

agent seed:
  model: "m"
  output: payload
  system: sys
  user: usr

router pick:
  mode: condition

agent show:
  model: "m"
  input: payload
  output: payload
  system: sys
  user: usr

workflow test:
  entry: seed
  seed -> pick with { topic: "{{outputs.seed.topic}}" }
  pick -> show with { topic: "{{input.topic}}" }
  show -> done
`
	r := compileFile(t, src)
	expectNoDiag(t, r, DiagInputFieldNotInSchema)
	expectNoDiag(t, r, DiagRefNodeNoSchema)
}

func TestValidateRefInputOnEdge_SchemalessSource_C032Warn(t *testing.T) {
	src := `
schema dst_in:
  reviewer: string

prompt sys:
  System.

prompt usr:
  User.

agent seed:
  model: "m"
  system: sys
  user: usr

agent dst:
  model: "m"
  input: dst_in
  output: dst_in
  system: sys
  user: usr

vars:
  reviewer: string

workflow test:
  entry: seed
  seed -> dst with { reviewer: "{{input.reviewer}}" }
  dst -> done
`
	r := compileFile(t, src)
	expectNoDiag(t, r, DiagInputFieldNotInSchema)
	expectDiag(t, r, DiagRefNodeNoSchema)
	var msg string
	for _, d := range r.Diagnostics {
		if d.Code == DiagRefNodeNoSchema && strings.Contains(d.Message, `field "reviewer"`) {
			msg = d.Message
			break
		}
	}
	if msg == "" {
		t.Fatalf("expected C032 for {{input.reviewer}} on a schemaless source, got: %v", r.Diagnostics)
	}
	for _, want := range []string{
		"no output schema",
		"not run inputs",
		"{{vars.reviewer}}",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("C032 message missing %q: %s", want, msg)
		}
	}
}

// ---------------------------------------------------------------------------
// C036 — node not reachable before consumer
// ---------------------------------------------------------------------------

func TestValidateRefNodeNotReachable(t *testing.T) {
	src := `
schema s:
  ok: bool

prompt sys:
  Use {{outputs.b.ok}} here.

prompt usr:
  User.

agent a:
  model: "m"
  output: s
  system: sys
  user: usr

agent b:
  model: "m"
  output: s
  system: sys
  user: usr

workflow test:
  entry: a
  a -> done
  b -> done
`
	r := compileFile(t, src)
	expectDiag(t, r, DiagRefNodeNotReachable)
}

func TestValidateRefReachableViaLoop(t *testing.T) {
	src := `
schema s:
  approved: bool

prompt sys:
  System.

prompt usr:
  User.

agent writer:
  model: "m"
  output: s
  system: sys
  user: usr

judge reviewer:
  model: "m"
  output: s
  system: sys
  user: usr

workflow test:
  entry: writer
  writer -> reviewer
  reviewer -> done when approved
  reviewer -> writer when not approved as revision_loop(5) with {
    feedback: "{{outputs.reviewer}}"
  }
`
	r := compileFile(t, src)
	expectNoDiag(t, r, DiagRefNodeNotReachable)
}

func TestValidateRefInEdgeWithMapping_UnknownNode(t *testing.T) {
	src := `
schema s:
  ok: bool

prompt sys:
  System.

prompt usr:
  User.

agent a:
  model: "m"
  output: s
  system: sys
  user: usr

agent b:
  model: "m"
  output: s
  system: sys
  user: usr

workflow test:
  entry: a
  a -> b with {
    data: "{{outputs.ghost}}"
  }
  b -> done
`
	r := compileFile(t, src)
	expectDiag(t, r, DiagUnknownRefNode)
}

func TestValidateRefInToolCommand_UnknownNode(t *testing.T) {
	src := `
schema s:
  ok: bool

prompt sys:
  System.

prompt usr:
  User.

agent a:
  model: "m"
  output: s
  system: sys
  user: usr

tool t1:
  command: "echo {{outputs.phantom}}"

workflow test:
  entry: a
  a -> t1
  t1 -> done
`
	r := compileFile(t, src)
	expectDiag(t, r, DiagUnknownRefNode)
}

// ---------------------------------------------------------------------------
// Underscore-prefixed fields (runtime-injected) — should be skipped
// ---------------------------------------------------------------------------

func TestValidateRefUnderscoreField_Skipped(t *testing.T) {
	src := `
schema s:
  ok: bool

prompt sys:
  System.

prompt usr:
  User.

agent a:
  model: "m"
  output: s
  system: sys
  user: usr

agent b:
  model: "m"
  output: s
  system: sys
  user: usr

workflow test:
  entry: a
  a -> b with {
    sid: "{{outputs.a._session_id}}"
  }
  b -> done
`
	r := compileFile(t, src)
	expectNoDiag(t, r, DiagRefFieldNotInSchema)
	expectNoDiag(t, r, DiagRefNodeNoSchema)
}

// ---------------------------------------------------------------------------
// C060 — Playwright MCP requires a browser-capable sandbox image
// ---------------------------------------------------------------------------

func TestValidatePlaywrightMCP_SandboxWithoutBrowserImage(t *testing.T) {
	// A workflow that uses a sandbox image NOT marked as browser-capable
	// must trigger C060 when an MCP server declares the Playwright
	// package.
	src := `
schema s:
  ok: bool

prompt sys:
  System.

prompt usr:
  User.

mcp_server pw:
  command: "npx"
  args: ["-y", "@playwright/mcp@latest"]

agent a:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

workflow test:
  entry: a
  sandbox:
    image: "ghcr.io/socialgouv/iterion-sandbox-slim:1.0.0"
  a -> done
`
	r := compileFile(t, src)
	expectDiag(t, r, DiagPlaywrightNeedsBrowserImage)
}

func TestValidatePlaywrightMCP_BrowserImageSatisfiesRequirement(t *testing.T) {
	// Same setup but the sandbox image name signals browser-capable —
	// no diagnostic should fire.
	src := `
schema s:
  ok: bool

prompt sys:
  System.

prompt usr:
  User.

mcp_server pw:
  command: "npx"
  args: ["-y", "@playwright/mcp@latest"]

agent a:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

workflow test:
  entry: a
  sandbox:
    image: "ghcr.io/socialgouv/iterion-sandbox-browser:1.0.0"
  a -> done
`
	r := compileFile(t, src)
	expectNoDiag(t, r, DiagPlaywrightNeedsBrowserImage)
}

func TestValidatePlaywrightMCP_NoSandboxSkipsCheck(t *testing.T) {
	// Host-mode workflow: operator is responsible for chromium install.
	// C060 must NOT fire.
	src := `
schema s:
  ok: bool

prompt sys:
  System.

prompt usr:
  User.

mcp_server pw:
  command: "npx"
  args: ["-y", "@playwright/mcp@latest"]

agent a:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

workflow test:
  entry: a
  a -> done
`
	r := compileFile(t, src)
	expectNoDiag(t, r, DiagPlaywrightNeedsBrowserImage)
}

func TestValidatePlaywrightMCP_NonPlaywrightCommandNotFlagged(t *testing.T) {
	// A non-Playwright MCP server with a sandbox must not be flagged
	// (the matcher is intentionally narrow).
	src := `
schema s:
  ok: bool

prompt sys:
  System.

prompt usr:
  User.

mcp_server other:
  command: "npx"
  args: ["-y", "@modelcontextprotocol/server-filesystem", "/tmp"]

agent a:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr

workflow test:
  entry: a
  sandbox:
    image: "ghcr.io/socialgouv/iterion-sandbox-slim:1.0.0"
  a -> done
`
	r := compileFile(t, src)
	expectNoDiag(t, r, DiagPlaywrightNeedsBrowserImage)
}

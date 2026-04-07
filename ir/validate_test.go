package ir

import (
	"os"
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
		"pr_refine_single_model.iter",
		"pr_refine_dual_model_parallel.iter",
		"pr_refine_dual_model_parallel_compliance.iter",
		"recipe_benchmark.iter",
		"ci_fix_until_green.iter",
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
			path := "../examples/" + fixture
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
	found := false
	for _, d := range c.diags {
		if d.Code == DiagInvalidReasoningEffort {
			found = true
		}
	}
	if !found {
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

func readFixture(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}

// ---------------------------------------------------------------------------
// C030 — outputs ref to non-existent node
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
// C034 — input ref field not in input schema
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

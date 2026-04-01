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
// C009 — session: inherit after join
// ---------------------------------------------------------------------------

func TestValidateInheritAfterJoin_Rejected(t *testing.T) {
	src := `
schema s:
  ok: bool

schema join_out:
  merged: json

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

join j1:
  strategy: wait_all
  require: [a1, a2]
  output: join_out

agent after_join:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr
  session: inherit

workflow test:
  entry: r1
  r1 -> a1
  r1 -> a2
  a1 -> j1
  a2 -> j1
  j1 -> after_join
  after_join -> done
`
	r := compileFile(t, src)
	expectDiag(t, r, DiagInheritAfterJoin)
}

func TestValidateInheritAfterJoin_FreshAllowed(t *testing.T) {
	src := `
schema s:
  ok: bool

schema join_out:
  merged: json

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

join j1:
  strategy: wait_all
  require: [a1, a2]
  output: join_out

agent after_join:
  model: "m"
  input: s
  output: s
  system: sys
  user: usr
  session: fresh

workflow test:
  entry: r1
  r1 -> a1
  r1 -> a2
  a1 -> j1
  a2 -> j1
  j1 -> after_join
  after_join -> done
`
	r := compileFile(t, src)
	expectNoDiag(t, r, DiagInheritAfterJoin)
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
// C015 — join require references unknown node
// ---------------------------------------------------------------------------

func TestValidateJoinRequireUnknown_Rejected(t *testing.T) {
	src := `
schema s:
  ok: bool

schema join_out:
  merged: json

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

join j1:
  strategy: wait_all
  require: [a1, nonexistent_node]
  output: join_out

workflow test:
  entry: a1
  a1 -> j1
  j1 -> done
`
	r := compileFile(t, src)
	expectDiag(t, r, DiagJoinRequireUnknown)
}

func TestValidateJoinRequire_Valid(t *testing.T) {
	src := `
schema s:
  ok: bool

schema join_out:
  merged: json

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

join j1:
  strategy: wait_all
  require: [a1, a2]
  output: join_out

workflow test:
  entry: r1
  r1 -> a1
  r1 -> a2
  a1 -> j1
  a2 -> j1
  j1 -> done
`
	r := compileFile(t, src)
	expectNoDiag(t, r, DiagJoinRequireUnknown)
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
		DiagInheritAfterJoin,
		DiagMultipleDefaultEdges,
		DiagAmbiguousCondition,
		DiagMissingFallback,
		DiagConditionNotBool,
		DiagConditionFieldNotFound,
		DiagJoinRequireUnknown,
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

func readFixture(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}

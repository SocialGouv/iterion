package ir

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)

// TestDiagnosticCodes_Unique guards against future regressions of
// the duplicate-code class of bug (several codes historically described two
// unrelated things — downstream consumers routing diagnostics by
// code mixed the categories). Asserted via a manual list because
// DiagCode is a string type with no registry to iterate.
func TestDiagnosticCodes_Unique(t *testing.T) {
	all := map[string]DiagCode{
		"DiagUnknownNode":               DiagUnknownNode,
		"DiagUnknownSchema":             DiagUnknownSchema,
		"DiagUnknownPrompt":             DiagUnknownPrompt,
		"DiagBadTemplateRef":            DiagBadTemplateRef,
		"DiagDuplicateLoop":             DiagDuplicateLoop,
		"DiagNoWorkflow":                DiagNoWorkflow,
		"DiagMultipleWorkflow":          DiagMultipleWorkflow,
		"DiagMissingEntry":              DiagMissingEntry,
		"DiagMissingModelOrBackend":     DiagMissingModelOrBackend,
		"DiagDuplicateMCPServer":        DiagDuplicateMCPServer,
		"DiagInvalidMCPServer":          DiagInvalidMCPServer,
		"DiagComputeNoExpr":             DiagComputeNoExpr,
		"DiagBadExpr":                   DiagBadExpr,
		"DiagDuplicateNodeID":           DiagDuplicateNodeID,
		"DiagReservedNodeName":          DiagReservedNodeName,
		"DiagInvalidSandboxMode":        DiagInvalidSandboxMode,
		"DiagSandboxAutoNoConfig":       DiagSandboxAutoNoConfig,
		"DiagBudgetCostInvalid":         DiagBudgetCostInvalid,
		"DiagSessionAfterConvergence":   DiagSessionAfterConvergence,
		"DiagMultipleDefaultEdges":      DiagMultipleDefaultEdges,
		"DiagAmbiguousCondition":        DiagAmbiguousCondition,
		"DiagMissingFallback":           DiagMissingFallback,
		"DiagConditionNotBool":          DiagConditionNotBool,
		"DiagConditionFieldNotFound":    DiagConditionFieldNotFound,
		"DiagUnreachableNode":           DiagUnreachableNode,
		"DiagHistoryRefNotInLoop":       DiagHistoryRefNotInLoop,
		"DiagUndeclaredCycle":           DiagUndeclaredCycle,
		"DiagRoundRobinTooFewEdges":     DiagRoundRobinTooFewEdges,
		"DiagLLMRouterTooFewEdges":      DiagLLMRouterTooFewEdges,
		"DiagLLMRouterConditionEdge":    DiagLLMRouterConditionEdge,
		"DiagRouterLLMOnlyProperty":     DiagRouterLLMOnlyProperty,
		"DiagFanOutEachMissingOver":     DiagFanOutEachMissingOver,
		"DiagFanOutEachOnlyProperty":    DiagFanOutEachOnlyProperty,
		"DiagFanOutEachEdges":           DiagFanOutEachEdges,
		"DiagInvalidReasoningEffort":    DiagInvalidReasoningEffort,
		"DiagInvalidLoopIterations":     DiagInvalidLoopIterations,
		"DiagDuplicateWithKey":          DiagDuplicateWithKey,
		"DiagUnknownRefNode":            DiagUnknownRefNode,
		"DiagRefFieldNotInSchema":       DiagRefFieldNotInSchema,
		"DiagRefNodeNoSchema":           DiagRefNodeNoSchema,
		"DiagUndeclaredVar":             DiagUndeclaredVar,
		"DiagInputFieldNotInSchema":     DiagInputFieldNotInSchema,
		"DiagUnknownArtifact":           DiagUnknownArtifact,
		"DiagRefNodeNotReachable":       DiagRefNodeNotReachable,
		"DiagNodeMaxTokensVsBudget":     DiagNodeMaxTokensVsBudget,
		"DiagUnsupportedMCPAuth":        DiagUnsupportedMCPAuth,
		"DiagInvalidCompaction":         DiagInvalidCompaction,
		"DiagDuplicateAttachment":       DiagDuplicateAttachment,
		"DiagAttachmentVarConflict":     DiagAttachmentVarConflict,
		"DiagInvalidAttachmentMIME":     DiagInvalidAttachmentMIME,
		"DiagUnknownAttachment":         DiagUnknownAttachment,
		"DiagAttachmentSubfieldUnknown": DiagAttachmentSubfieldUnknown,
		"DiagUnknownCapability":         DiagUnknownCapability,
		"DiagMalformedCapability":       DiagMalformedCapability,
		"DiagBoardCapInSandbox":         DiagBoardCapInSandbox,
		"DiagUnknownProvider":           DiagUnknownProvider,
		"DiagProviderChainIgnored":      DiagProviderChainIgnored,
	}
	seen := make(map[DiagCode]string, len(all))
	for name, code := range all {
		if string(code) == "" {
			t.Errorf("%s has empty code", name)
			continue
		}
		if other, dup := seen[code]; dup {
			t.Errorf("code %s used by both %s and %s", code, other, name)
		}
		seen[code] = name
	}
}

// TestUndeclaredCycle_DetectedOutsideEntryComponent guards the fix
// that extended validateUndeclaredCycles to sweep every reachable
// node, not just the component traceable from w.Entry. Cycles inside
// a fan-out branch that don't trace back to entry are now caught.
func TestUndeclaredCycle_DetectedOutsideEntryComponent(t *testing.T) {
	src := `
schema empty:
  ok: bool

prompt sys:
  body
  hello

agent entry_node:
  model: "gpt-4"
  system: sys
  output: empty

agent loop_a:
  model: "gpt-4"
  system: sys
  output: empty

agent loop_b:
  model: "gpt-4"
  system: sys
  output: empty

router fanout:
  mode: fan_out_all

workflow w:
  entry: entry_node

  entry_node -> fanout
  fanout -> done
  fanout -> loop_a
  loop_a -> loop_b
  loop_b -> loop_a
`
	r := compileFile(t, src)
	expectDiag(t, r, DiagUndeclaredCycle)
}

// TestBoardCapInSandbox_Warning guards the previously-dead C082
// diagnostic now firing when board.* capabilities are granted on
// a sandboxed workflow.
func TestBoardCapInSandbox_Warning(t *testing.T) {
	src := `
schema empty:
  ok: bool

prompt sys:
  body
  hello

agent writer:
  model: "gpt-4"
  system: sys
  output: empty
  capabilities: [board.create]

workflow w:
  sandbox: auto
  entry: writer
  writer -> done
`
	r := compileFile(t, src)
	expectDiag(t, r, DiagBoardCapInSandbox)
}

// TestBoardCapWithoutSandbox_NoWarning is the inverse — no sandbox
// means the stdio path works fine and no warning is expected.
func TestBoardCapWithoutSandbox_NoWarning(t *testing.T) {
	src := `
schema empty:
  ok: bool

prompt sys:
  body
  hello

agent writer:
  model: "gpt-4"
  system: sys
  output: empty
  capabilities: [board.read]

workflow w:
  entry: writer
  writer -> done
`
	r := compileFile(t, src)
	expectNoDiag(t, r, DiagBoardCapInSandbox)
}

// TestBudget_NegativeCostInvalid guards the new C046 diagnostic for
// negative / NaN / Inf max_cost_usd. The DSL parser doesn't accept a
// bare negative literal in budget blocks, but a recipe or programmatic
// AST construction can plant one — so we exercise the compiler's
// budget path directly.
func TestBudget_NegativeCostInvalid(t *testing.T) {
	c := &compiler{}
	_ = c.compileBudget(&ast.BudgetBlock{MaxCostUSD: -5.0})
	if !hasDiag(c.diags, DiagBudgetCostInvalid) {
		t.Errorf("expected DiagBudgetCostInvalid, got diagnostics: %v", c.diags)
	}
}

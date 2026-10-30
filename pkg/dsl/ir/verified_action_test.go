package ir

import "testing"

// Wrap a tool node in a minimal valid workflow so Compile + validate run.
func vaWorkflow(toolBody string) string {
	return `tool act:
` + toolBody + `

workflow w:
  entry: act
  act -> done
`
}

// A well-formed Verified Action node compiles clean and carries the quad.
func TestVerifiedAction_CompilesAndDefaults(t *testing.T) {
	src := vaWorkflow(`  command: "git commit -am wip"
  goal: "advance HEAD"
  postcondition: "git rev-parse HEAD"
  policy: recover`)
	wf := mustCompile(t, src)
	tn, ok := wf.Nodes["act"].(*ToolNode)
	if !ok {
		t.Fatal("act is not a ToolNode")
	}
	if tn.Postcondition == "" || tn.Goal == "" {
		t.Fatalf("quad not compiled: %+v", tn)
	}
	if tn.Policy != PolicyRecover {
		t.Fatalf("Policy = %q, want recover", tn.Policy)
	}
	// recover with no recovery block → default one self-repair attempt.
	if tn.Recovery == nil || tn.Recovery.MaxRepairAttempts != 1 {
		t.Fatalf("Recovery default = %+v, want MaxRepairAttempts=1", tn.Recovery)
	}
}

// A postcondition without an explicit policy defaults to required.
func TestVerifiedAction_DefaultPolicyRequired(t *testing.T) {
	src := vaWorkflow(`  command: "echo hi"
  postcondition: "true"`)
	wf := mustCompile(t, src)
	tn := wf.Nodes["act"].(*ToolNode)
	if tn.Policy != PolicyRequired {
		t.Fatalf("Policy = %q, want required (default)", tn.Policy)
	}
	if tn.Recovery != nil {
		t.Fatalf("required policy should have no recovery, got %+v", tn.Recovery)
	}
}

// C103 — invalid policy value.
func TestVerifiedAction_C103InvalidPolicy(t *testing.T) {
	src := vaWorkflow(`  command: "echo hi"
  postcondition: "true"
  policy: yolo`)
	r := compileFile(t, src)
	expectDiag(t, r, DiagInvalidPolicy)
}

// C104 — recovery configured without a postcondition.
func TestVerifiedAction_C104RecoveryNoPostcondition(t *testing.T) {
	src := vaWorkflow(`  command: "echo hi"
  policy: recover`)
	r := compileFile(t, src)
	expectDiag(t, r, DiagRecoveryNoPostcond)
}

// C105 — recovery attached to a gate (recipe == postcondition).
func TestVerifiedAction_C105RecoveryOnGate(t *testing.T) {
	src := vaWorkflow(`  command: "check_invariant"
  postcondition: "check_invariant"
  policy: recover`)
	r := compileFile(t, src)
	expectDiag(t, r, DiagRecoveryOnGate)
}

// C106 — recovery bounds present but policy is not recover (dead config).
func TestVerifiedAction_C106RecoveryWithoutRecover(t *testing.T) {
	src := vaWorkflow(`  command: "echo hi"
  postcondition: "true"
  policy: required
  recovery:
    max_repair_attempts: 2`)
	r := compileFile(t, src)
	expectDiag(t, r, DiagRecoveryWithoutRecov)
}

// C179 — a registry recipe (a bare tool name) under policy: recover whose
// only budgeted rung is max_repair_attempts reaches NO recovery rung: since
// #2783 dispatch skips rung 3 on a recipe with no command to correct, and
// rung 4 is off without max_agent_attempts. Exactly one warning, naming the
// node.
//
// Mutation seen red: treat a registry recipe as self-repairable in
// ir.ToolRecipeSelfRepairable (pkg/dsl/ir/recipe_kind.go) and this witness
// loses its C179.
func TestVerifiedAction_C179RegistryRecipeRepairOnly(t *testing.T) {
	src := vaWorkflow(`  command: "noop_tool"
  goal: "touch nothing"
  postcondition: "true"
  policy: recover
  recovery:
    max_repair_attempts: 1`)
	r := compileFile(t, src)
	var found []Diagnostic
	for _, d := range r.Diagnostics {
		if d.Code == DiagRepairRungUnreachable {
			found = append(found, d)
		}
	}
	if len(found) != 1 {
		t.Fatalf("expected exactly one %s, got diagnostics %v", DiagRepairRungUnreachable, r.Diagnostics)
	}
	if found[0].NodeID != "act" {
		t.Errorf("C179 names node %q, want act", found[0].NodeID)
	}
	if found[0].Severity != SeverityWarning {
		t.Errorf("C179 severity = %s, want warning (philosophy #1: warn, never reject)", found[0].Severity)
	}
}

// A recipe-LESS node (no command/script/action) is already refused by C004:
// C179 must not stack on the error with an empty tool name.
func TestVerifiedAction_C179SilentWhenNoRecipeAtAll(t *testing.T) {
	src := vaWorkflow(`  goal: "touch nothing"
  postcondition: "true"
  policy: recover
  recovery:
    max_repair_attempts: 1`)
	r := compileFile(t, src)
	for _, d := range r.Diagnostics {
		if d.Code == DiagRepairRungUnreachable {
			t.Fatalf("C179 fired on a recipe-less node already refused by C004: %v", d)
		}
	}
}

// The DEFAULTED recovery is the same shape: policy: recover with no
// recovery: block budgets one self-repair attempt (compile.go), which a
// registry recipe cannot spend.
func TestVerifiedAction_C179DefaultedRecoveryOnRegistryRecipe(t *testing.T) {
	src := vaWorkflow(`  command: "noop_tool"
  postcondition: "true"
  policy: recover`)
	expectDiag(t, compileFile(t, src), DiagRepairRungUnreachable)
}

// A recipe with a command to correct — a shell command, or a script — gives
// rung 3 its surface: no C179.
func TestVerifiedAction_C179ShellAndScriptRecipesSilent(t *testing.T) {
	for name, recipe := range map[string]string{
		"shell command": `  command: "echo hi"`,
		"script": `  script: |
    echo hi
  language: sh`,
	} {
		t.Run(name, func(t *testing.T) {
			src := vaWorkflow(recipe + `
  postcondition: "true"
  policy: recover
  recovery:
    max_repair_attempts: 1`)
			expectNoDiag(t, compileFile(t, src), DiagRepairRungUnreachable)
		})
	}
}

// max_agent_attempts opens rung 4, so the ladder is reachable even on a
// registry recipe: no C179.
func TestVerifiedAction_C179AgentRungBudgeted(t *testing.T) {
	src := vaWorkflow(`  command: "noop_tool"
  postcondition: "true"
  policy: recover
  recovery:
    max_repair_attempts: 1
    max_agent_attempts: 1`)
	expectNoDiag(t, compileFile(t, src), DiagRepairRungUnreachable)
}

// No recovery at all (no policy: recover, no recovery: block): the policy
// defaults to required and nothing is budgeted — no C179.
func TestVerifiedAction_C179NoRecoverySilent(t *testing.T) {
	src := vaWorkflow(`  command: "noop_tool"
  postcondition: "true"`)
	expectNoDiag(t, compileFile(t, src), DiagRepairRungUnreachable)
}

// A plain tool node (no quad) emits none of the new diagnostics.
func TestVerifiedAction_NoQuadNoDiagnostics(t *testing.T) {
	src := vaWorkflow(`  command: "echo hi"`)
	r := compileFile(t, src)
	expectNoDiag(t, r, DiagInvalidPolicy)
	expectNoDiag(t, r, DiagRecoveryNoPostcond)
	expectNoDiag(t, r, DiagRecoveryOnGate)
	expectNoDiag(t, r, DiagRecoveryWithoutRecov)
	expectNoDiag(t, r, DiagRepairRungUnreachable)
}

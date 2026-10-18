package ir

import (
	"fmt"
	"strings"
	"testing"
)

// The C167 cells: the enumeration's discovery half. Each witness compiles a
// real workflow and reads the diagnostics the compiler emits.

// baseWorkflow is the shared scaffold: one agent node on claw, the gate on.
// The %s slots take the node's tools LINE (including its indent and newline,
// empty for an undeclared surface), the node's rules lines and the
// workflow's rules lines.
const baseWorkflow = `
schema empty:
  ok: bool

agent start:
  model: "test-model"
  output: empty
  backend: claw
%s
workflow w:
  entry: start
  permission: ask
%s
`

func compileCoverage(t *testing.T, nodeRules, nodeTools, workflowRules string) *CompileResult {
	t.Helper()
	toolsLine := ""
	if nodeTools != "NONE" && nodeTools != "" {
		toolsLine = "  tools: [" + nodeTools + "]\n"
	}
	rulesLine := ""
	if nodeRules != "" {
		rulesLine = "  " + nodeRules + "\n"
	}
	wfLine := ""
	if workflowRules != "" {
		wfLine = "  " + workflowRules + "\n"
	}
	src := fmt.Sprintf(baseWorkflow, toolsLine+rulesLine, wfLine)
	return compileFile(t, src)
}

// TestDenyBashBesideReplWarnsOnce is the ticket's exact cell: a claw node
// granting bash and repl under deny: ["Bash"]. The warning fires once and
// names the uncovered tool from the registry.
func TestDenyBashBesideReplWarnsOnce(t *testing.T) {
	r := compileCoverage(t, `deny: ["Bash"]`, `"bash", "repl"`, "")
	d := 0
	for _, diag := range r.Diagnostics {
		if diag.Code == DiagPermissionClassRisk {
			d++
		}
	}
	if d != 1 {
		t.Fatalf("the Bash+repl cell must fire once, got %d: %v", d, r.Diagnostics)
	}
	expectDiagMessage(t, r, DiagPermissionClassRisk, `"repl"`)
}

// TestDenyClassShellCoversEverythingStaysSilent: the remedy clears the
// warning — a class row covers every member, so nothing is uncovered.
func TestDenyClassShellCoversEverythingStaysSilent(t *testing.T) {
	r := compileCoverage(t, `deny: ["class:shell"]`, `"bash", "repl", "diagnostic_shell"`, "")
	expectNoDiag(t, r, DiagPermissionClassRisk)
}

// TestAmbientClawNodeWithPartialDenyWarns: no tools: declaration on claw is
// the registry's full built-in set; a partial deny leaves repl uncovered.
func TestAmbientClawNodeWithPartialDenyWarns(t *testing.T) {
	r := compileCoverage(t, `deny: ["Bash"]`, "NONE", "")
	expectDiag(t, r, DiagPermissionClassRisk)
	expectDiagMessage(t, r, DiagPermissionClassRisk, `"repl"`)
}

// TestGateOffStaysSilent: an inert list is C111's verdict; the coverage
// warning does not double-fire on it.
func TestGateOffStaysSilent(t *testing.T) {
	src := `
schema empty:
  ok: bool

agent start:
  model: "test-model"
  output: empty
  backend: claw
  tools: ["bash", "repl"]
  deny: ["Bash"]

workflow w:
  entry: start
  permission: off
  start -> done
`
	r := compileFile(t, src)
	expectNoDiag(t, r, DiagPermissionClassRisk)
}

// TestAllowClassRowWarnsTheWidening: allow widens, so an allow row naming a
// class grants present AND future members — warned, not refused.
func TestAllowClassRowWarnsTheWidening(t *testing.T) {
	src := `
schema empty:
  ok: bool

agent start:
  model: "test-model"
  output: empty
  backend: claw
  tools: ["bash", "repl"]
  allow: ["class:shell"]
  deny: ["class:shell"]

workflow w:
  entry: start
  permission: ask
  start -> done
`
	r := compileFile(t, src)
	expectDiag(t, r, DiagPermissionClassRisk)
}

// TestAskClassRowStaysSilent: ask widens protectively — a future member is
// asked, the gate holds — so no warning.
func TestAskClassRowStaysSilent(t *testing.T) {
	src := `
schema empty:
  ok: bool

agent start:
  model: "test-model"
  output: empty
  backend: claw
  tools: ["bash", "repl"]
  ask: ["class:shell"]

workflow w:
  entry: start
  permission: deny
  start -> done
`
	r := compileFile(t, src)
	expectNoDiag(t, r, DiagPermissionClassRisk)
}

// TestUnknownClassIsRefusedAtAuthoring: through C154, the parser's error
// naming the known classes.
func TestUnknownClassIsRefusedAtAuthoring(t *testing.T) {
	src := `
schema empty:
  ok: bool

agent start:
  model: "test-model"
  output: empty
  backend: claw
  tools: ["bash"]
  deny: ["class:shel"]

workflow w:
  entry: start
  permission: ask
  start -> done
`
	r := compileFile(t, src)
	expectDiag(t, r, DiagInvalidPermissionRule)
	expectDiagMessage(t, r, DiagInvalidPermissionRule, `class:shel`)
	found := false
	for _, d := range r.Diagnostics {
		if d.Code == DiagInvalidPermissionRule && strings.Contains(d.Message, "known classes") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the refusal must name the known classes; got %v", r.Diagnostics)
	}
}

// TestNodeDenyReplacesWorkflowForCoverage: the coverage question reads the
// list the node RUNS under — its own deny when declared, the workflow's
// otherwise.
func TestNodeDenyReplacesWorkflowForCoverage(t *testing.T) {
	// Node declares its own deny, covering nothing shell-shaped: the
	// workflow's class row cannot rescue it — replacement, not union.
	r := compileCoverage(t, `deny: ["Read"]`, `"bash", "repl"`, `deny: ["class:shell"]`)
	expectDiag(t, r, DiagPermissionClassRisk)

	// Node declares nothing: the workflow's class row covers it — silent.
	r = compileCoverage(t, "", `"bash", "repl"`, `deny: ["class:shell"]`)
	expectNoDiag(t, r, DiagPermissionClassRisk)
}

// TestNonClawNodeStaysSilent: the class cell is claw's — the names resolve
// only on the backend whose registry holds them.
func TestNonClawNodeStaysSilent(t *testing.T) {
	src := `
schema empty:
  ok: bool

agent start:
  model: "test-model"
  output: empty
  backend: claude_code
  deny: ["Bash"]

workflow w:
  entry: start
  permission: ask
  start -> done
`
	r := compileFile(t, src)
	expectNoDiag(t, r, DiagPermissionClassRisk)
}

// TestAnAskRowBoundsTheTool: an ask row stops an unattended execution in
// every mode, so an asked tool counts as covered — the shipped copilot bot
// asks diagnostic_shell and must not fire. A SPELLING ask bounds its tool
// only: ask Bash beside repl still fires, naming repl.
func TestAnAskRowBoundsTheTool(t *testing.T) {
	// The copilot shape: diagnostic_shell granted and asked — silent.
	r := compileCoverage(t, `ask: ["diagnostic_shell"]`, `"read_file", "diagnostic_shell"`, `deny: ["Grep", "Write", "Edit"]`)
	expectNoDiag(t, r, DiagPermissionClassRisk)

	// A spelling ask bounds bash only; repl stays uncovered.
	r = compileCoverage(t, `ask: ["Bash"]`, `"bash", "repl"`, "")
	expectDiag(t, r, DiagPermissionClassRisk)
	expectDiagMessage(t, r, DiagPermissionClassRisk, `"repl"`)
}

// TestFallbackClawRouteCarriesTheCell: the node does not run on claw, but a
// fallback route does — the same list rides the route at the moment the run
// is already failing over.
func TestFallbackClawRouteCarriesTheCell(t *testing.T) {
	src := `
schema empty:
  ok: bool

agent start:
  model: "test-model"
  output: empty
  backend: claude_code
  tools: ["bash", "repl"]
  deny: ["Bash"]
  fallbacks:
    cl:
      backend: "claw"
      model: "test-model"
      on: [unavailable]

workflow w:
  entry: start
  permission: ask
  start -> done
`
	r := compileFile(t, src)
	expectDiag(t, r, DiagPermissionClassRisk)
	expectDiagMessage(t, r, DiagPermissionClassRisk, `"repl"`)
}

// TestWorkflowAllowClassRowWarns: the same widening cell at the workflow
// level.
func TestWorkflowAllowClassRowWarns(t *testing.T) {
	src := `
schema empty:
  ok: bool

agent start:
  model: "test-model"
  output: empty
  backend: claw
  tools: ["read_file"]

workflow w:
  entry: start
  permission: ask
  allow: ["class:shell"]
  start -> done
`
	r := compileFile(t, src)
	expectDiag(t, r, DiagPermissionClassRisk)
}

// TestAllowClassShadowedByAskStaysSilent: an ask row of the same class
// decides before allow in Evaluate, so every member call pauses and the
// allow row grants nothing — "each auto-approved" would be false. The
// reviewer's cell: both rows compile clean, and the warning misdescribed
// the pair.
func TestAllowClassShadowedByAskStaysSilent(t *testing.T) {
	src := `
schema empty:
  ok: bool

agent start:
  model: "test-model"
  output: empty
  backend: claw
  tools: ["bash", "repl"]
  allow: ["class:shell"]
  ask: ["class:shell"]

workflow w:
  entry: start
  permission: ask
  start -> done
`
	r := compileFile(t, src)
	expectNoDiag(t, r, DiagPermissionClassRisk)
}

// TestWorkflowAllowClassShadowedByWorkflowAskStaysSilent: the same shadow at
// the workflow level, where the two lists are declared side by side.
func TestWorkflowAllowClassShadowedByWorkflowAskStaysSilent(t *testing.T) {
	src := `
schema empty:
  ok: bool

agent start:
  model: "test-model"
  output: empty
  backend: claw
  tools: ["bash", "repl"]

workflow w:
  entry: start
  permission: ask
  allow: ["class:shell"]
  ask: ["class:shell"]
  start -> done
`
	r := compileFile(t, src)
	expectNoDiag(t, r, DiagPermissionClassRisk)
}

// TestNoBoundsClawNodeStaysSilent: SOME bound must be declared for the
// uncovered-shell cell to fire — a gate-on claw node with no deny and no ask
// rows has no incomplete bound to name (the mode default already bounds
// every unmatched tool, and C111 owns the "no lists at all" verdict). The
// shape the docs recommend; the mutant that drops this arm warns on every
// node of every such workflow.
func TestNoBoundsClawNodeStaysSilent(t *testing.T) {
	r := compileCoverage(t, "", "NONE", "")
	expectNoDiag(t, r, DiagPermissionClassRisk)
}

// recoveryWorkflow scaffolds one Verified Action tool node whose rung-4
// recovery agent carries the given agent_tools, under a workflow-level deny
// row on claw. The recovery agent is gated by the workflow's list, and its
// tool set is the list the author wrote for it.
func recoveryWorkflow(t *testing.T, agentTools string) *CompileResult {
	t.Helper()
	src := `
schema empty:
  ok: bool

tool deploy:
  command: "./deploy.sh"
  output: empty
  goal: "the service is deployed"
  postcondition: "./check.sh"
  policy: recover
  recovery:
    max_agent_attempts: 1
    agent_tools: [` + agentTools + `]

workflow w:
  entry: deploy
  permission: ask
  deny: ["Bash"]
  default_backend: "claw"
  deploy -> done
`
	return compileFile(t, src)
}

// TestRecoveryAgentReadsItsDeclaredTools: the rung-4 agent's surface is the
// `agent_tools:` list the runtime hands it — a declared list without a shell
// member contributes nothing to the class cell, and a declared shell member
// the deny row does not bound still fires.
func TestRecoveryAgentReadsItsDeclaredTools(t *testing.T) {
	// The false positive the ambient read produced: the agent holds
	// read_file, so it cannot call diagnostic_shell or repl — silent.
	r := recoveryWorkflow(t, "read_file")
	expectNoDiag(t, r, DiagPermissionClassRisk)

	// A declared shell tool outside the deny row is the real cell: the
	// recovery agent can call repl, nothing bounds it.
	r = recoveryWorkflow(t, "repl")
	expectDiag(t, r, DiagPermissionClassRisk)
	expectDiagMessage(t, r, DiagPermissionClassRisk, `"repl"`)

	// No agent_tools key at all is the ambient surface — every member.
	r = recoveryWorkflowNoTools(t)
	expectDiag(t, r, DiagPermissionClassRisk)
	expectDiagMessage(t, r, DiagPermissionClassRisk, `"repl"`)
}

// recoveryWorkflowNoTools is recoveryWorkflow without the agent_tools key:
// the rung-4 agent runs the registry's ambient built-in set.
func recoveryWorkflowNoTools(t *testing.T) *CompileResult {
	t.Helper()
	src := `
schema empty:
  ok: bool

tool deploy:
  command: "./deploy.sh"
  output: empty
  goal: "the service is deployed"
  postcondition: "./check.sh"
  policy: recover
  recovery:
    max_agent_attempts: 1

workflow w:
  entry: deploy
  permission: ask
  deny: ["Bash"]
  default_backend: "claw"
  deploy -> done
`
	return compileFile(t, src)
}

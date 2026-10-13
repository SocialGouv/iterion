package ir

import (
	"strings"
	"testing"
)

// TestCompilePermission exercises the permission gate field end-to-end:
// parser → AST → IR on a workflow that sets the scalar mode + allow/ask/deny
// rule lists at workflow level and a per-node permission mode override on an
// agent, a judge, and a tool node. Each value uses one of the accepted
// barewords (off / ask / deny) so the C110 validator stays silent, and the
// workflow mode is "ask" so the rule lists do not trigger C111.
func TestCompilePermission(t *testing.T) {
	src := `
schema empty:
  ok: bool

prompt sys:
  hi

prompt usr:
  hi

agent start:
  model: "test-model"
  input: empty
  output: empty
  system: sys
  user: usr
  permission: deny

judge gate:
  model: "test-model"
  input: empty
  output: empty
  system: sys
  user: usr
  permission: ask

tool ship:
  command: "true"
  output: empty
  permission: off

workflow minimal:
  entry: start
  permission: ask
  allow: ["Read(**)"]
  ask: ["Bash(go build:*)"]
  deny: ["Bash(rm:*)"]
  start -> gate
  gate -> ship
  ship -> done
`
	w := mustCompile(t, src)

	if w.Permission != "ask" {
		t.Errorf("workflow.Permission = %q, want ask", w.Permission)
	}
	if got := w.PermissionAllow; len(got) != 1 || got[0] != "Read(**)" {
		t.Errorf("workflow.PermissionAllow = %v, want [Read(**)]", got)
	}
	if got := w.PermissionAsk; len(got) != 1 || got[0] != "Bash(go build:*)" {
		t.Errorf("workflow.PermissionAsk = %v, want [Bash(go build:*)]", got)
	}
	if got := w.PermissionDeny; len(got) != 1 || got[0] != "Bash(rm:*)" {
		t.Errorf("workflow.PermissionDeny = %v, want [Bash(rm:*)]", got)
	}

	a, ok := w.Nodes["start"].(*AgentNode)
	if !ok {
		t.Fatalf("start node = %T, want *AgentNode", w.Nodes["start"])
	}
	if a.Permission != "deny" {
		t.Errorf("agent.Permission = %q, want deny", a.Permission)
	}
	j, ok := w.Nodes["gate"].(*JudgeNode)
	if !ok {
		t.Fatalf("gate node = %T, want *JudgeNode", w.Nodes["gate"])
	}
	if j.Permission != "ask" {
		t.Errorf("judge.Permission = %q, want ask", j.Permission)
	}
	tn, ok := w.Nodes["ship"].(*ToolNode)
	if !ok {
		t.Fatalf("ship node = %T, want *ToolNode", w.Nodes["ship"])
	}
	if tn.Permission != "off" {
		t.Errorf("tool.Permission = %q, want off", tn.Permission)
	}
}

// TestValidatePermissionInvalid asserts that a typo like `permission: bogus`
// raises the C110 diagnostic on every site (workflow + agent + judge + tool).
func TestValidatePermissionInvalid(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{
			name: "workflow",
			src: `
schema empty:
  ok: bool

agent start:
  model: "test-model"
  output: empty

workflow w:
  entry: start
  permission: bogus
  start -> done
`,
		},
		{
			name: "agent",
			src: `
schema empty:
  ok: bool

agent start:
  model: "test-model"
  output: empty
  permission: bogus

workflow w:
  entry: start
  start -> done
`,
		},
		{
			name: "judge",
			src: `
schema empty:
  ok: bool

judge gate:
  model: "test-model"
  output: empty
  permission: bogus

workflow w:
  entry: gate
  gate -> done
`,
		},
		{
			name: "tool",
			src: `
schema empty:
  ok: bool

tool ship:
  command: "true"
  output: empty
  permission: bogus

workflow w:
  entry: ship
  ship -> done
`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := compileFile(t, tc.src)
			expectDiag(t, r, DiagInvalidPermission)
		})
	}
}

// TestValidatePermissionValidNoDiag confirms that the three accepted barewords
// ("off", "ask", "deny") never trigger C110.
func TestValidatePermissionValidNoDiag(t *testing.T) {
	for _, v := range []string{"off", "ask", "deny"} {
		t.Run(v, func(t *testing.T) {
			src := `
schema empty:
  ok: bool

agent start:
  model: "test-model"
  output: empty
  permission: ` + v + `

workflow w:
  entry: start
  permission: ` + v + `
  start -> done
`
			r := compileFile(t, src)
			expectNoDiag(t, r, DiagInvalidPermission)
		})
	}
}

// TestPermissionRulesWithoutGate verifies C111 fires when allow/ask/deny rules
// are declared but the resolved workflow permission mode is unset or off, and
// stays silent when the gate is enabled (ask/deny).
func TestPermissionRulesWithoutGate(t *testing.T) {
	t.Run("unset_mode_warns", func(t *testing.T) {
		src := `
schema empty:
  ok: bool

agent start:
  model: "test-model"
  output: empty

workflow w:
  entry: start
  deny: ["Bash(rm:*)"]
  start -> done
`
		r := compileFile(t, src)
		expectDiag(t, r, DiagPermissionRulesNoGate)
	})

	t.Run("off_mode_warns", func(t *testing.T) {
		src := `
schema empty:
  ok: bool

agent start:
  model: "test-model"
  output: empty

workflow w:
  entry: start
  permission: off
  allow: ["Read(**)"]
  start -> done
`
		r := compileFile(t, src)
		expectDiag(t, r, DiagPermissionRulesNoGate)
	})

	t.Run("ask_mode_silent", func(t *testing.T) {
		src := `
schema empty:
  ok: bool

agent start:
  model: "test-model"
  output: empty

workflow w:
  entry: start
  permission: ask
  deny: ["Bash(rm:*)"]
  start -> done
`
		r := compileFile(t, src)
		expectNoDiag(t, r, DiagPermissionRulesNoGate)
	})
}

// TestToolNodePermissionInert verifies C112 warns when a tool node sets a
// non-off permission mode (parsed but not enforced — the gate only governs
// agent/judge LLM tool calls), and stays silent for agent/judge nodes.
func TestToolNodePermissionInert(t *testing.T) {
	t.Run("tool_node_deny_warns", func(t *testing.T) {
		src := `
schema empty:
  ok: bool

agent start:
  model: "test-model"
  output: empty

tool ship:
  command: "true"
  output: empty
  permission: deny

workflow w:
  entry: start
  start -> ship
  ship -> done
`
		r := compileFile(t, src)
		expectDiag(t, r, DiagToolNodePermissionInert)
	})

	t.Run("agent_node_ask_silent", func(t *testing.T) {
		src := `
schema empty:
  ok: bool

agent start:
  model: "test-model"
  output: empty
  permission: ask

workflow w:
  entry: start
  start -> done
`
		r := compileFile(t, src)
		expectNoDiag(t, r, DiagToolNodePermissionInert)
	})

	t.Run("the_remedy_names_the_workflow", func(t *testing.T) {
		// This graph has NO agent node, so a remedy that says "gate the
		// agent nodes instead" has no addressee — while the gate the node's
		// Verified-Action recovery agent does obey is the workflow's
		// (pkg/backend/model,
		// TestWorkflowGateGovernsAToolNodesRecoveryAgent). A remedy must
		// point at a gate that exists in the graph in front of the author.
		src := `
schema empty:
  ok: bool

tool ship:
  command: "rm -rf ./dist"
  output: empty
  permission: deny
  goal: "the artefacts are rebuilt"
  postcondition: "./check.sh"
  policy: recover
  recovery:
    max_agent_attempts: 1

workflow w:
  entry: ship
  ship -> done
`
		r := compileFile(t, src)
		var inert *Diagnostic
		for i := range r.Diagnostics {
			if r.Diagnostics[i].Code == DiagToolNodePermissionInert {
				inert = &r.Diagnostics[i]
			}
		}
		if inert == nil {
			t.Fatalf("no C112 on a tool node's permission: deny: %+v", r.Diagnostics)
		}
		if !strings.Contains(inert.Hint, "workflow") {
			t.Errorf("C112 hint = %q; want the workflow named — it is the gate this node's recovery agent obeys, and the only one this graph has", inert.Hint)
		}
		// The remedy's condition is conditional on purpose (C112 also fires
		// on a tool node with no recovery at all), so it must name the WHOLE
		// condition: the bound alone runs no rung, the ladder reaches rungs
		// 3-4 only under policy: recover (pkg/backend/model,
		// TestOnlyPolicyRecoverHandsTheGoalToTheRungAgent). Half the
		// condition sends the author to gate a rung that never runs.
		for _, want := range []string{"policy: recover", "max_agent_attempts > 0"} {
			if !strings.Contains(inert.Hint, want) {
				t.Errorf("C112 hint = %q; want it to name %q — reaching the agent rung needs BOTH halves", inert.Hint, want)
			}
		}
	})
}

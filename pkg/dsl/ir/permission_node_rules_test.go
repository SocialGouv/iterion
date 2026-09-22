package ir

import (
	"slices"
	"strings"
	"testing"
)

// TestEffectivePermissionRulesReplaces pins the one rule the whole feature
// rests on: a non-empty node list REPLACES the workflow list of the same
// kind. The mutation this reddens is the tempting one — concatenating the
// two — which would make a narrowing impossible to express.
func TestEffectivePermissionRulesReplaces(t *testing.T) {
	wf := []string{"Read(**)", "Bash(git diff:*)"}
	node := []string{"Bash"}

	if got := EffectivePermissionRules(node, wf); !slices.Equal(got, node) {
		t.Errorf("declared node list must REPLACE the workflow's: got %v, want %v", got, node)
	}
	if got := EffectivePermissionRules(nil, wf); !slices.Equal(got, wf) {
		t.Errorf("absent node list must inherit the workflow's: got %v, want %v", got, wf)
	}
	// An empty-but-non-nil list is "not declared": the AST's JSON seam
	// carries these lists with omitempty, so nil and [] arrive identical
	// and a rule built on the difference would break through the studio.
	if got := EffectivePermissionRules([]string{}, wf); !slices.Equal(got, wf) {
		t.Errorf("an empty node list is not a declaration: got %v, want %v", got, wf)
	}
}

// TestCompileNodePermissionRules verifies the three lists reach the IR on
// both node kinds that accept them, as the NODE's own fields.
func TestCompileNodePermissionRules(t *testing.T) {
	src := `
schema empty:
  ok: bool

agent start:
  model: "test-model"
  output: empty
  allow: ["Read(pkg/**)"]
  deny: ["Bash"]

judge gate:
  model: "test-model"
  output: empty
  ask: ["WebFetch"]

workflow w:
  entry: start
  permission: deny
  allow: ["Read(**)"]
  start -> gate
  gate -> done
`
	r := compileFile(t, src)
	a, ok := r.Workflow.Nodes["start"].(*AgentNode)
	if !ok {
		t.Fatalf("start is not an agent node: %T", r.Workflow.Nodes["start"])
	}
	if got := a.PermissionAllow; !slices.Equal(got, []string{"Read(pkg/**)"}) {
		t.Errorf("agent PermissionAllow = %v", got)
	}
	if got := a.PermissionDeny; !slices.Equal(got, []string{"Bash"}) {
		t.Errorf("agent PermissionDeny = %v", got)
	}
	if got := a.PermissionAsk; len(got) != 0 {
		t.Errorf("agent PermissionAsk = %v, want empty", got)
	}
	j, ok := r.Workflow.Nodes["gate"].(*JudgeNode)
	if !ok {
		t.Fatalf("gate is not a judge node: %T", r.Workflow.Nodes["gate"])
	}
	if got := j.PermissionAsk; !slices.Equal(got, []string{"WebFetch"}) {
		t.Errorf("judge PermissionAsk = %v", got)
	}
	// The accessors are the enumeration lever: a future site that asks a
	// node for its rules goes through the interface, not the struct.
	var nn LLMNode = a
	if got := nn.GetPermissionDeny(); !slices.Equal(got, []string{"Bash"}) {
		t.Errorf("LLMNode.GetPermissionDeny() = %v", got)
	}
}

// TestNodeAskRulesReachTheRouteScreens is the class test for the trap this
// change had to avoid: C136 and C176 read the ask rules to decide whether a
// route can PAUSE, and both read them per node. Wiring the node lists only
// into the runtime would leave five screens reading w.PermissionAsk — a
// node-declared ask: would then become a gate that silently does not hold.
//
// Mutating EffectiveAskRules back to w.PermissionAsk reddens every subtest.
func TestNodeAskRulesReachTheRouteScreens(t *testing.T) {
	t.Run("c176_primary_route_grok", func(t *testing.T) {
		// grok enforces deny but cannot pause: a node-declared ask: rule
		// must refuse the route exactly as a workflow-declared one does.
		src := `
schema empty:
  ok: bool

agent start:
  model: "test-model"
  backend: grok
  output: empty
  permission: deny
  ask: ["Bash(git push:*)"]

workflow w:
  entry: start
  sandbox: none
  start -> done
`
		r := compileFile(t, src)
		expectDiag(t, r, DiagFallbackUnsafeCross)
	})

	t.Run("c136_sandboxed_claw", func(t *testing.T) {
		// A sandboxed claw route cannot pause for an Ask either.
		src := `
schema empty:
  ok: bool

agent start:
  model: "test-model"
  backend: claw
  output: empty
  permission: deny
  ask: ["Bash(git push:*)"]

workflow w:
  entry: start
  start -> done
`
		r := compileFile(t, src)
		expectDiag(t, r, DiagGatedCLIBackendSandbox)
	})

	t.Run("node_ask_replaces_workflow_ask", func(t *testing.T) {
		// The workflow declares an ask: rule that no backend can honour on
		// grok; the node REPLACES it with a claw-only alias, which neither
		// CLI can invoke, so the route is reachable again. A screen that
		// unioned the two lists would keep refusing.
		src := `
schema empty:
  ok: bool

agent start:
  model: "test-model"
  backend: grok
  output: empty
  permission: deny
  ask: ["diagnostic_shell"]

workflow w:
  entry: start
  sandbox: none
  ask: ["Bash(git push:*)"]
  start -> done
`
		r := compileFile(t, src)
		expectNoDiag(t, r, DiagFallbackUnsafeCross)
	})
}

// TestPermissionRulesReachNoGatedNode covers C111's three shapes and the
// false positive the per-node form removes.
func TestPermissionRulesReachNoGatedNode(t *testing.T) {
	t.Run("node_list_under_an_off_gate_warns", func(t *testing.T) {
		src := `
schema empty:
  ok: bool

agent start:
  model: "test-model"
  output: empty
  deny: ["Bash"]

workflow w:
  entry: start
  start -> done
`
		r := compileFile(t, src)
		expectDiagMessage(t, r, DiagPermissionRulesNoGate, "its effective permission gate is")
	})

	t.Run("workflow_list_shadowed_at_every_gated_node_warns", func(t *testing.T) {
		// The gate is ON and the node is gated — the old predicate ("is the
		// workflow mode off") says nothing here. The workflow's deny: is
		// nonetheless inert: the only gated node replaces it.
		src := `
schema empty:
  ok: bool

agent start:
  model: "test-model"
  output: empty
  deny: ["Bash"]

workflow w:
  entry: start
  permission: deny
  deny: ["Bash(rm:*)"]
  start -> done
`
		r := compileFile(t, src)
		expectDiagMessage(t, r, DiagPermissionRulesNoGate, "every reader declares its own")
	})

	t.Run("a_node_that_turns_the_gate_on_makes_workflow_rules_live", func(t *testing.T) {
		// The workflow mode is unset, so the pre-change predicate warned
		// "rules are inert" — a false positive: the node turns the gate on
		// and inherits the workflow's allow: verbatim.
		src := `
schema empty:
  ok: bool

agent start:
  model: "test-model"
  output: empty
  permission: deny

workflow w:
  entry: start
  allow: ["Read(**)"]
  start -> done
`
		r := compileFile(t, src)
		expectNoDiag(t, r, DiagPermissionRulesNoGate)
	})

	t.Run("a_second_gated_node_without_its_own_list_keeps_it_live", func(t *testing.T) {
		src := `
schema empty:
  ok: bool

agent start:
  model: "test-model"
  output: empty
  deny: ["Bash"]

judge gate:
  model: "test-model"
  output: empty

workflow w:
  entry: start
  permission: deny
  deny: ["Bash(rm:*)"]
  start -> gate
  gate -> done
`
		r := compileFile(t, src)
		expectNoDiag(t, r, DiagPermissionRulesNoGate)
	})
}

// TestPermissionRulesReachNoGatedNodeMoreShapes covers the shapes round 1
// proved the first predicate got wrong. Each is its own cause with its own
// fix, so each gets its own case.
func TestPermissionRulesReachNoGatedNodeMoreShapes(t *testing.T) {
	t.Run("no_llm_node_at_all_warns", func(t *testing.T) {
		// The one shape no run-time override can falsify: with nothing that
		// issues LLM tool calls, the rules are unreadable, full stop. An
		// early return here was a silent regression on the pre-change code.
		src := `
schema empty:
  ok: bool

compute c:
  output: empty
  expr:
    ok: "true"

workflow w:
  entry: c
  permission: deny
  deny: ["Bash(rm:*)"]
  c -> done
`
		r := compileFile(t, src)
		expectDiagMessage(t, r, DiagPermissionRulesNoGate, "no node issues LLM tool calls")
	})

	t.Run("a_tool_nodes_recovery_agent_rung_reads_the_workflow_list", func(t *testing.T) {
		// A Verified Action's rung-4 agent is a synthetic node that declares
		// no lists and is therefore gated by the WORKFLOW's — and it holds
		// bash. Calling that list "inert" invites the author to delete the
		// only bound on the most dangerous agent in the graph.
		src := `
schema empty:
  ok: bool

agent worker:
  model: "test-model"
  output: empty
  deny: ["Write"]

tool deploy:
  command: "./deploy.sh"
  output: empty
  goal: "the service is deployed"
  postcondition: "./check.sh"
  policy: recover
  recovery:
    max_agent_attempts: 1
    agent_tools: [read_file, bash]

workflow w:
  entry: worker
  permission: deny
  deny: ["Bash(rm -rf:*)"]
  worker -> deploy
  deploy -> done
`
		r := compileFile(t, src)
		expectNoDiag(t, r, DiagPermissionRulesNoGate)
	})

	t.Run("a_recovery_agent_rung_is_a_reader_even_under_an_off_gate", func(t *testing.T) {
		// With no agent/judge node and the gate off, the absolute arm
		// ("nothing can ever read them") would fire — and be false: an
		// operator's --permission arms the gate and the recovery agent, which
		// holds bash, is then bounded by exactly this list.
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
    agent_tools: [read_file, bash]

workflow w:
  entry: deploy
  deny: ["Bash(rm -rf:*)"]
  deploy -> done
`
		r := compileFile(t, src)
		for _, d := range r.Diagnostics {
			if d.Code == DiagPermissionRulesNoGate && strings.Contains(d.Message, "nothing can ever read them") {
				t.Fatalf("the recovery agent rung READS this list; got the absolute verdict: %s", d.Message)
			}
		}
	})

	t.Run("an_ungated_node_without_its_own_list_is_not_a_shadow", func(t *testing.T) {
		// Node b is off under the DSL, so it is not a GATED reader — but
		// --permission arms it and it then inherits the workflow's deny:.
		// Calling that list shadowed would invite deleting b's only bound.
		src := `
schema empty:
  ok: bool

agent a:
  model: "test-model"
  output: empty
  permission: deny
  deny: ["Write"]

agent b:
  model: "test-model"
  output: empty
  permission: off

workflow w:
  entry: a
  deny: ["Bash(rm -rf:*)"]
  a -> b
  b -> done
`
		r := compileFile(t, src)
		for _, d := range r.Diagnostics {
			if d.Code == DiagPermissionRulesNoGate && strings.Contains(d.Message, "every reader declares its own") {
				t.Fatalf("node b reads this list once the gate is armed; got: %s", d.Message)
			}
		}
	})

	t.Run("a_recovery_agent_rung_reads_all_three_workflow_lists", func(t *testing.T) {
		// The synthetic recovery agent declares NO list, so it inherits all
		// three. Marking only one kind as read leaves the suite green and
		// produces a false shadow verdict on the other two — advice to delete
		// the only bound on the graph's most dangerous agent.
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
    agent_tools: [read_file, bash]

workflow w:
  entry: deploy
  permission: deny
  allow: ["Read(**)"]
  ask: ["WebFetch"]
  deny: ["Bash(rm -rf:*)"]
  deploy -> done
`
		r := compileFile(t, src)
		expectNoDiag(t, r, DiagPermissionRulesNoGate)
	})

	t.Run("a_gated_workflow_whose_every_reader_is_off_names_the_override", func(t *testing.T) {
		// The branch that exists because the parenthetical used to print the
		// workflow's own mode next to "no reader runs with the gate on" — a
		// sentence contradicting itself. Bound to the message, since the
		// sibling arm emits the same code.
		src := `
schema empty:
  ok: bool

agent a:
  model: "test-model"
  output: empty
  permission: off

judge g:
  model: "test-model"
  output: empty
  permission: off

workflow w:
  entry: a
  permission: deny
  deny: ["Bash(rm -rf:*)"]
  a -> g
  g -> done
`
		r := compileFile(t, src)
		expectDiagMessage(t, r, DiagPermissionRulesNoGate, "every reader overrides it to off")
	})

	t.Run("an_invalid_workflow_mode_suppresses_the_workflow_verdict", func(t *testing.T) {
		// Every node overrides the workflow with a valid mode, so the node
		// loop never observes the workflow's own bad word. Without reading it
		// up front, C111 quotes "effective permission is askk" — a mode C110
		// just refused, on a file that does not compile.
		src := `
schema empty:
  ok: bool

agent a:
  model: "test-model"
  output: empty
  permission: off

workflow w:
  entry: a
  permission: askk
  allow: ["Read(**)"]
  a -> done
`
		r := compileFile(t, src)
		expectDiag(t, r, DiagInvalidPermission)
		expectNoDiag(t, r, DiagPermissionRulesNoGate)
	})

	t.Run("an_invalid_node_mode_does_not_count_as_a_shadow", func(t *testing.T) {
		// C110 refuses the mode; a node that cannot start must not be read
		// as the reader that shadows the workflow's list.
		src := `
schema empty:
  ok: bool

agent start:
  model: "test-model"
  output: empty
  permission: nope
  deny: ["Bash"]

workflow w:
  entry: start
  deny: ["Write(**)"]
  start -> done
`
		r := compileFile(t, src)
		expectDiag(t, r, DiagInvalidPermission)
		expectNoDiag(t, r, DiagPermissionRulesNoGate)
	})
}

// TestInvalidPermissionRuleIsRefused covers C154: an entry the gate's own
// parser cannot read fails the node at dispatch today, silently green at
// compile time. The check runs permission.ValidateRule — the production
// parser — so it has no spelling of its own to drift.
func TestInvalidPermissionRuleIsRefused(t *testing.T) {
	const tmplNode = `
schema empty:
  ok: bool

agent start:
  model: "test-model"
  output: empty
  permission: deny
  deny: [%s]

workflow w:
  entry: start
  start -> done
`
	for _, tc := range []struct {
		name, rule string
		wantDiag   bool
	}{
		{"empty_rule", `"", "Bash"`, true},
		{"unclosed_paren", `"Bash(go test:*"`, true},
		{"missing_tool_name", `"(go test:*)"`, true},
		{"well_formed_bare", `"Bash"`, false},
		{"well_formed_scoped", `"Bash(go test:*)"`, false},
		{"well_formed_glob", `"mcp__github__get_*"`, false},
		{"well_formed_path", `"Read(.env*)"`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := compileFile(t, sprintfBot(tmplNode, tc.rule))
			if tc.wantDiag {
				expectDiag(t, r, DiagInvalidPermissionRule)
			} else {
				expectNoDiag(t, r, DiagInvalidPermissionRule)
			}
		})
	}

	t.Run("every_accessor_arm_is_read", func(t *testing.T) {
		// nodePermissionRules is the ONLY reader of the six
		// LLMNode.GetPermission{Allow,Ask,Deny} accessors, and three of them
		// could be blinded to nil with the whole suite green. One fixture
		// carrying an unreadable rule in each surviving arm reddens all three.
		src := `
schema empty:
  ok: bool

agent a:
  model: "test-model"
  output: empty
  permission: deny
  allow: ["Bash(go build:*"]

judge g:
  model: "test-model"
  output: empty
  permission: deny
  allow: ["(nope)"]
  deny: ["Write(**"]

workflow w:
  entry: a
  a -> g
  g -> done
`
		r := compileFile(t, src)
		for _, want := range []string{
			`agent "a" declares an unreadable allow permission rule`,
			`judge "g" declares an unreadable allow permission rule`,
			`judge "g" declares an unreadable deny permission rule`,
		} {
			expectDiagMessage(t, r, DiagInvalidPermissionRule, want)
		}
	})

	t.Run("a_judges_list_is_checked_too", func(t *testing.T) {
		src := `
schema empty:
  ok: bool

judge gate:
  model: "test-model"
  output: empty
  permission: deny
  ask: ["Bash(go test:*"]

workflow w:
  entry: gate
  gate -> done
`
		r := compileFile(t, src)
		expectDiag(t, r, DiagInvalidPermissionRule)
	})

	t.Run("a_workflow_list_is_checked_too", func(t *testing.T) {
		src := `
schema empty:
  ok: bool

agent start:
  model: "test-model"
  output: empty

workflow w:
  entry: start
  permission: deny
  allow: ["Bash(go test:*"]
  start -> done
`
		r := compileFile(t, src)
		expectDiag(t, r, DiagInvalidPermissionRule)
	})
}

// sprintfBot keeps each table row to the one literal that differs.
func sprintfBot(tmpl, arg string) string { return strings.Replace(tmpl, "%s", arg, 1) }

// TestNodeAskRulesReachEveryRouteScreen gives the two remaining in-package
// EffectiveAskRules call sites a witness of their own. Round 1 proved by
// per-site mutation that four of the six could be reverted to
// w.PermissionAsk with the whole suite green — a chokepoint is proven on its
// complete enumeration, not on the two sites that happened to have a test.
func TestNodeAskRulesReachEveryRouteScreen(t *testing.T) {
	t.Run("authored_fallback_route", func(t *testing.T) {
		// checkFallbackCrossing: the node's own ask: must refuse a grok
		// FALLBACK exactly as it refuses a grok primary.
		//
		// The assertion is on the MESSAGE, not the code: a fallback crossing
		// emits C176 for several unrelated reasons (tool-surface inversion,
		// session continuity), so a code-only assertion stays green when the
		// ask screen is blinded — a witness of nothing.
		src := `
schema empty:
  ok: bool

agent start:
  model: "anthropic/claude-opus-5"
  backend: claude_code
  output: empty
  permission: deny
  ask: ["Bash(git push:*)"]
  fallbacks:
    relief:
      backend: grok
      model: "grok-4"
      on: [unavailable]

workflow w:
  entry: start
  sandbox: none
  start -> done
`
		r := compileFile(t, src)
		expectDiagMessage(t, r, DiagFallbackUnsafeCross, "cannot pause for the explicit ask: rules")
	})

	t.Run("judge_node_primary_route", func(t *testing.T) {
		// The IR, the JSON seam and extractBackendFields each carry two
		// duplicated arms, and every compiler screen reaches a node's lists
		// through the LLMNode ACCESSORS. Blinding only the judge's accessors
		// left the whole suite green before this case.
		src := `
schema empty:
  ok: bool

judge gate:
  model: "test-model"
  backend: grok
  output: empty
  permission: deny
  ask: ["Bash(git push:*)"]

workflow w:
  entry: gate
  sandbox: none
  gate -> done
`
		r := compileFile(t, src)
		expectDiagMessage(t, r, DiagFallbackUnsafeCross, "cannot pause for the explicit ask: rules")
	})

	t.Run("run_level_fallback_route", func(t *testing.T) {
		// ApplyRunFallback is the launch-time applier for --fallback. It is
		// not reached by `iterion validate`, so only a direct call witnesses
		// it — and a node-declared ask: silently ceasing to refuse here is a
		// live ungating.
		agent := &AgentNode{BaseNode: BaseNode{ID: "work"}}
		agent.Backend = "claw"
		agent.PermissionAsk = []string{"Bash(git push:*)"}
		agent.Permission = "deny"
		w := &Workflow{Name: "w", Nodes: map[string]Node{"work": agent}}

		refusals := ApplyRunFallback(w, []Fallback{{Backend: "grok"}}, false)
		if len(refusals) == 0 {
			t.Fatal("a node-declared ask: rule must refuse a run-level grok fallback")
		}
		if !strings.Contains(refusals[0], "grok") || !strings.Contains(refusals[0], "cannot pause") {
			t.Errorf("refusal = %q, want grok + cannot-pause", refusals[0])
		}

		// And the workflow's own list still refuses when the node declares none.
		agent.PermissionAsk = nil
		w.PermissionAsk = []string{"Bash(git push:*)"}
		if got := ApplyRunFallback(w, []Fallback{{Backend: "grok"}}, false); len(got) == 0 {
			t.Error("a workflow ask: rule must still refuse a run-level grok fallback")
		}
	})
}

// expectDiagMessage requires a diagnostic of the given code WHOSE MESSAGE
// carries want. A code-only assertion is worthless where several unrelated
// checks share one code: the mutation that blinds the check under test
// leaves a sibling's diagnostic behind, and the test stays green.
func expectDiagMessage(t *testing.T, r *CompileResult, code DiagCode, want string) {
	t.Helper()
	for _, d := range r.Diagnostics {
		if d.Code == code && strings.Contains(d.Message, want) {
			return
		}
	}
	t.Fatalf("no %s diagnostic containing %q; got %v", code, want, r.Diagnostics)
}

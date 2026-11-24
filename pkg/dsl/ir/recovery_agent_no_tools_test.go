package ir

import (
	"strings"
	"testing"
)

// C352: a tool node whose Verified Action reaches rung 4 on claw with an
// agent that holds no tool. Each witness compiles `.bot` TEXT.

// recoveryBot scaffolds one Verified Action tool node and its workflow. tool
// is the node's body after its postcondition (the policy and the recovery
// block), workflow the workflow's body before its edge.
func recoveryBot(tool, workflow string) string {
	return "tool deploy:\n  command: \"./deploy.sh\"\n  goal: \"the service is deployed\"\n  postcondition: \"./check.sh\"\n" + tool +
		"workflow w:\n  entry: deploy\n" + workflow + "  deploy -> done\n"
}

// recoveryAgentNoTools returns the C352 findings of a compile.
func recoveryAgentNoTools(r *CompileResult) []Diagnostic {
	var out []Diagnostic
	for _, d := range r.Diagnostics {
		if d.Code == DiagRecoveryAgentNoTools {
			out = append(out, d)
		}
	}
	return out
}

// TestARecoveryAgentWithNoToolOnClawWarns is the fire set: rung 4 is
// reachable, the source sends its agent to claw, and nothing gives that
// agent a tool.
func TestARecoveryAgentWithNoToolOnClawWarns(t *testing.T) {
	const claw = "  default_backend: \"claw\"\n"
	const rung = "  policy: recover\n  recovery:\n    max_agent_attempts: 2\n"
	for _, tc := range []struct {
		name, src string
	}{
		{"no agent_tools", recoveryBot(rung, claw)},
		{"agent_tools: [] declared", recoveryBot(rung+"    agent_tools: []\n", claw)},
		// The defect is the surface, not a rule: asked whatever the gate
		// says, armed or not, with or without lists.
		{"gate on, with rules", recoveryBot(rung, claw+"  permission: deny\n  deny: [\"Bash\"]\n")},
		{"gate explicitly off", recoveryBot(rung, claw+"  permission: off\n")},
		{"both rungs budgeted", recoveryBot(rung+"    max_repair_attempts: 1\n", claw)},
		{"a dial defaulting to claw", recoveryBot(rung, "  default_backend: \"${C352_UNSET:-claw}\"\n")},
		{"codemode off at the workflow", recoveryBot(rung, claw+"  codemode: off\n")},
		// The agent inherits the workflow's grant, and a grant rides a list
		// that already holds a tool.
		{"workflow capabilities", recoveryBot(rung, claw+"  capabilities: [board.read]\n")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := recoveryAgentNoTools(compileText(t, tc.src))
			if len(got) != 1 {
				t.Fatalf("C352 fired %d time(s), want once: %+v", len(got), got)
			}
			for _, want := range []string{`tool "deploy"`, "max_agent_attempts: 2", "runs on claw", "`recovery.agent_tools:`", "is served none"} {
				if !strings.Contains(got[0].Message, want) {
					t.Errorf("message does not carry %q: %s", want, got[0].Message)
				}
			}
		})
	}
}

// TestARecoveryAgentStaysSilentWhereItHoldsATool is the other half: every
// arm that takes one of the three conditions away.
func TestARecoveryAgentStaysSilentWhereItHoldsATool(t *testing.T) {
	const claw = "  default_backend: \"claw\"\n"
	const rung = "  policy: recover\n  recovery:\n    max_agent_attempts: 2\n"
	for _, tc := range []struct{ name, src string }{
		// A surface on the claw route.
		{"agent_tools declared", recoveryBot(rung+"    agent_tools: [bash, read_file]\n", claw)},
		{"codemode on at the workflow", recoveryBot(rung, claw+"  codemode: on\n")},

		// No route the source sends to claw: the synthetic agent names no
		// backend of its own, so the workflow default is the only field that
		// can.
		{"default_backend: claude_code", recoveryBot(rung, "  default_backend: \"claude_code\"\n")},
		{"no default_backend", recoveryBot(rung, "")},
		{"default_backend: auto", recoveryBot(rung, "  default_backend: \"auto\"\n")},
		{"a dial nothing answers", recoveryBot(rung, "  default_backend: \"${C352_UNSET}\"\n")},
		{"a vars-routed default_backend", "vars:\n  b: string = \"claw\"\n" + recoveryBot(rung, "  default_backend: \"{{vars.b}}\"\n")},

		// Rung 4 is not reachable: nothing dispatches the agent.
		{"only the self-repair rung", recoveryBot("  policy: recover\n  recovery:\n    max_repair_attempts: 2\n", claw)},
		{"policy: recover with no recovery block", recoveryBot("  policy: recover\n", claw)},
		{"bounds under another policy", recoveryBot("  policy: best_effort\n  recovery:\n    max_agent_attempts: 2\n", claw)},
		{"no Verified Action at all", "tool deploy:\n  command: \"./deploy.sh\"\nworkflow w:\n  entry: deploy\n" + claw + "  deploy -> done\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := recoveryAgentNoTools(compileText(t, tc.src)); len(got) != 0 {
				t.Fatalf("C352 on a recovery agent that holds a tool, or is never dispatched on claw: %s", got[0].Message)
			}
		})
	}
}

// TestRecoveryAgentNoToolsDiagnosticShape holds what a validate loop acts
// on: the code, the severity, the `recovery:` line it points at, the
// property it names, the node it is attributed to and the catalogue's fix.
func TestRecoveryAgentNoToolsDiagnosticShape(t *testing.T) {
	src := recoveryBot("  policy: recover\n  recovery:\n    max_agent_attempts: 1\n", "  default_backend: \"claw\"\n")
	got := recoveryAgentNoTools(compileText(t, src))
	if len(got) != 1 {
		t.Fatalf("C352 fired %d time(s), want once: %+v", len(got), got)
	}
	d := got[0]
	if d.Code != "C352" || d.Severity != SeverityWarning {
		t.Errorf("code %s severity %v, want a C352 warning: the node still runs, and the postcondition judges it", d.Code, d.Severity)
	}
	if d.NodeID != "deploy" {
		t.Errorf("attributed to %q, want the tool node", d.NodeID)
	}
	// `recovery:` is the sixth line of the fixture.
	if d.File != "x.bot" || d.Line != 6 || d.Column != 3 || d.Property != "recovery" {
		t.Errorf("at %s:%d:%d property %q, want x.bot:6:3 on `recovery`", d.File, d.Line, d.Column, d.Property)
	}
	if d.Hint == "" || d.Hint != HintFor(DiagRecoveryAgentNoTools) {
		t.Errorf("hint %q is not the catalogue's %q", d.Hint, HintFor(DiagRecoveryAgentNoTools))
	}
	for _, want := range []string{"`agent_tools: [", "max_agent_attempts"} {
		if !strings.Contains(d.Hint, want) {
			t.Errorf("the fix line does not carry %q: %s", want, d.Hint)
		}
	}
	if info := Catalog[DiagRecoveryAgentNoTools]; info.Title != "Recovery agent that holds no tool" {
		t.Errorf("catalogue title %q", info.Title)
	}
}

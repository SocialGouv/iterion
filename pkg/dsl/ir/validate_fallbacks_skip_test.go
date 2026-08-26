package ir

import (
	"strings"
	"testing"
)

// Tests for the `action: skip` terminal route and the `when:` route gate.
// A skip route is the "continue and ignore" half of a peer-node
// unavailability policy; the compile checks below are what keep it from
// silently hiding routes (skip-not-last), doing two contradictory things
// (skip + backend), or gating on state that does not exist at dispatch
// (`when:` reading outputs.*).

// skipWorkflow prepends the vars block the when: gates reference — an
// undeclared var in when: is a C173 error since the gate would silently
// read false at run time.
func skipWorkflow(nodeProps, fallbacks string) string {
	return "vars:\n  policy: string = \"wait\"\n\n" + fallbackWorkflow(nodeProps, fallbacks)
}

func TestFallbackSkipRouteCompiles(t *testing.T) {
	src := skipWorkflow("", `    give_up:
      action: skip
      when: "vars.policy == 'skip'"
      on: [usage_window, unavailable, auth]
`)
	cr := compileFallbackSrc(t, src)
	if cr.HasErrors() {
		t.Fatalf("expected a clean compile, got %+v", cr.Diagnostics)
	}
	agent := cr.Workflow.Nodes["x"].(*AgentNode)
	if len(agent.Fallbacks) != 1 {
		t.Fatalf("expected 1 route, got %d", len(agent.Fallbacks))
	}
	fb := agent.Fallbacks[0]
	if fb.Action != FallbackActionSkip {
		t.Fatalf("Action = %q, want %q", fb.Action, FallbackActionSkip)
	}
	if fb.When != "vars.policy == 'skip'" {
		t.Fatalf("When = %q", fb.When)
	}
}

func TestFallbackUnknownActionIsAnError(t *testing.T) {
	src := fallbackWorkflow("", "    weird:\n      action: retry\n")
	cr := compileFallbackSrc(t, src)
	assertHasDiag(t, cr, DiagFallbackMalformed, "unknown action")
}

func TestFallbackSkipWithBackendIsAnError(t *testing.T) {
	src := fallbackWorkflow("", `    give_up:
      action: skip
      backend: "claw"
      model: "anthropic/claude-opus-5"
`)
	cr := compileFallbackSrc(t, src)
	assertHasDiag(t, cr, DiagFallbackMalformed, "action: skip together with a backend")
}

func TestFallbackSkipNotLastIsAnError(t *testing.T) {
	src := fallbackWorkflow("  tools: [read_file, bash]\n", `    give_up:
      action: skip
    api:
      backend: "claw"
      model: "anthropic/claude-opus-5"
`)
	cr := compileFallbackSrc(t, src)
	assertHasDiag(t, cr, DiagFallbackMalformed, "not the LAST route")
}

func TestFallbackWhenUnparseableIsAnError(t *testing.T) {
	src := skipWorkflow("", "    give_up:\n      action: skip\n      when: \"vars.policy ==\"\n")
	cr := compileFallbackSrc(t, src)
	assertHasDiag(t, cr, DiagFallbackMalformed, "unparseable when:")
}

func TestFallbackWhenNonVarsRefIsAnError(t *testing.T) {
	src := skipWorkflow("", "    give_up:\n      action: skip\n      when: \"outputs.gate.converged\"\n")
	cr := compileFallbackSrc(t, src)
	assertHasDiag(t, cr, DiagFallbackMalformed, "only vars.* resolves")
}

// A typo'd var in when: must be a compile error: at run time an absent
// var reads as nil → false and silently disarms the route's policy.
func TestFallbackWhenUndeclaredVarIsAnError(t *testing.T) {
	src := skipWorkflow("", "    give_up:\n      action: skip\n      when: \"vars.polcy == 'skip'\"\n")
	cr := compileFallbackSrc(t, src)
	assertHasDiag(t, cr, DiagFallbackMalformed, "undeclared variable")
}

// metered on a skip route is dead config: the route spends nothing.
func TestFallbackSkipWithMeteredIsAnError(t *testing.T) {
	src := skipWorkflow("", "    give_up:\n      action: skip\n      metered: true\n")
	cr := compileFallbackSrc(t, src)
	assertHasDiag(t, cr, DiagFallbackMalformed, "metered")
}

// assertHasDiag fails unless the compile produced the given code with a
// message containing want.
func assertHasDiag(t *testing.T, cr *CompileResult, code DiagCode, want string) {
	t.Helper()
	for _, d := range cr.Diagnostics {
		if d.Code == code && strings.Contains(d.Message, want) {
			return
		}
	}
	t.Fatalf("expected %s diagnostic containing %q, got %+v", code, want, cr.Diagnostics)
}

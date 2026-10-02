package ir

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// C271 — a synchronous interaction: is inert on the CLI-agent backends
// (#1644)
// ---------------------------------------------------------------------------

// TestSyncInteractionInertOnCLIBackends: on codex, kimi, grok and opencode
// no ask_user tool reaches the agent — the runtime grants it to claw
// outright or to a node whose tools: list constrains the route, and that
// list never reaches these CLIs' argv — so a sync interaction: there is
// prompt text, never a pause. claude_code, claw and pi serve it (native
// MCP server, in-process registry, embedded RPC extension) and stay
// silent. A WARNING, not C267's error: the run completes; the defect is
// a question never asked.
//
// Mutation that reddens it: drop the DiagSyncInteractionInert emission
// from validateSyncInteractionBackends.
func TestSyncInteractionInertOnCLIBackends(t *testing.T) {
	src := func(backend, extra string) string {
		return "agent a:\n  model: \"m\"\n" + backend + "  interaction: human\n" + extra +
			"workflow w:\n  entry: a\n  a -> done\n"
	}
	for name, tc := range map[string]struct {
		src  string
		want bool // true = C271 fires
	}{
		"codex, no tools":    {src("  backend: \"codex\"\n", ""), true},
		"kimi, no tools":     {src("  backend: \"kimi\"\n", ""), true},
		"grok, no tools":     {src("  backend: \"grok\"\n", ""), true},
		"opencode, no tools": {src("  backend: \"opencode\"\n", ""), true},
		// A tools: list changes nothing on these backends — it never
		// reaches the argv — so ask_user is appended to a list nobody
		// reads: still inert, still warned.
		"kimi, with a tools list": {src("  backend: \"kimi\"\n", "  tools: [bash]\n"), true},
		// llm / llm_or_human arm the same ask_user surface.
		"codex, llm mode": {
			"agent a:\n  model: \"m\"\n  backend: \"codex\"\n  interaction: llm\n  interaction_model: \"m\"\nworkflow w:\n  entry: a\n  a -> done\n", true,
		},
		// The backends that CAN serve a sync question stay silent.
		"claude_code": {src("  backend: \"claude_code\"\n", ""), false},
		"claw":        {src("  backend: \"claw\"\n", ""), false},
		"pi":          {src("  backend: \"pi\"\n", ""), false},
		// No interaction declared — nothing to serve.
		"kimi, no interaction": {
			"agent a:\n  model: \"m\"\n  backend: \"kimi\"\nworkflow w:\n  entry: a\n  a -> done\n", false,
		},
		// interaction: async is C267's refusal, not this warning.
		"kimi, async": {
			"agent a:\n  model: \"m\"\n  backend: \"kimi\"\n  interaction: async\nworkflow w:\n  entry: a\n  a -> done\n", false,
		},
		// A route the source does not decide is checked at dispatch, not
		// here — same rule as C267's.
		"a dial without a default": {
			"agent a:\n  model: \"m\"\n  backend: \"${C1644_UNSET}\"\n  interaction: human\nworkflow w:\n  entry: a\n  a -> done\n", false,
		},
		// …but a dial's DEFAULT decides, as on every backend screen (#1389).
		"a dial defaulting to grok": {
			"agent a:\n  model: \"m\"\n  backend: \"${C1644_UNSET:-grok}\"\n  interaction: human\nworkflow w:\n  entry: a\n  a -> done\n", true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := compileText(t, tc.src)
			var got *Diagnostic
			for i := range r.Diagnostics {
				if r.Diagnostics[i].Code == DiagSyncInteractionInert {
					got = &r.Diagnostics[i]
				}
			}
			if !tc.want {
				if got != nil {
					t.Fatalf("C271 on a served or undecided route: %s", got.Message)
				}
				return
			}
			if got == nil || got.Severity != SeverityWarning {
				t.Fatalf("no C271 warning: %+v\n%v", got, r.Diagnostics)
			}
			if got.NodeID != "a" {
				t.Errorf("C271 attributed to %q, want the node", got.NodeID)
			}
			if !strings.Contains(got.Message, "ask_user") {
				t.Errorf("message %q does not name the missing tool", got.Message)
			}
		})
	}
}

// TestSyncInteractionInertOnAFallbackRoute: the node's primary may serve
// the pause while a fallback route cannot — the warning names the route,
// so the author sees which arm degrades.
func TestSyncInteractionInertOnAFallbackRoute(t *testing.T) {
	src := "agent a:\n  model: \"m\"\n  backend: \"claude_code\"\n  interaction: human\n" +
		"  fallbacks:\n    backup:\n      backend: \"kimi\"\n      model: \"k\"\n" +
		"workflow w:\n  entry: a\n  a -> done\n"
	r := compileText(t, src)
	var got *Diagnostic
	for i := range r.Diagnostics {
		if r.Diagnostics[i].Code == DiagSyncInteractionInert {
			got = &r.Diagnostics[i]
		}
	}
	if got == nil {
		t.Fatalf("no C271 on the kimi fallback\ndiagnostics: %v", r.Diagnostics)
	}
	if !strings.Contains(got.Message, `fallback "backup"`) {
		t.Errorf("message %q does not name the fallback route", got.Message)
	}
	// The workflow-level default drives nodes that pin no backend: same
	// screen, one level up.
	r = compileText(t, "agent a:\n  model: \"m\"\n  interaction: llm_or_human\n  interaction_model: \"m\"\n"+
		"workflow w:\n  default_backend: \"opencode\"\n  entry: a\n  a -> done\n")
	expectDiag(t, r, DiagSyncInteractionInert)
}

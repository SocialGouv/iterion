package ir

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// fallbackWorkflow renders a one-agent workflow with the given node
// properties and `fallbacks:` body, so each test states only what it is
// about.
func fallbackWorkflow(nodeProps, fallbacks string) string {
	var b strings.Builder
	b.WriteString("agent x:\n")
	b.WriteString("  backend: \"claude_code\"\n")
	b.WriteString("  model: \"claude-opus-5\"\n")
	b.WriteString("  system: p\n")
	b.WriteString(nodeProps)
	b.WriteString("  fallbacks:\n")
	b.WriteString(fallbacks)
	b.WriteString("\nprompt p:\n  hi\n\nworkflow w:\n  entry: x\n  x -> done\n")
	return b.String()
}

func compileFallbackSrc(t *testing.T, src string) *CompileResult {
	t.Helper()
	pr := parser.Parse("t.bot", src)
	for _, d := range pr.Diagnostics {
		if d.Severity == parser.SeverityError {
			t.Fatalf("parse error: %s\n--- source ---\n%s", d.Error(), src)
		}
	}
	return Compile(pr.File)
}

// TestFallbacksCompileToOrderedRoutes is the happy path: the block
// parses, compiles, and preserves declaration order — which IS the try
// order.
//
// The node declares its `tools:` explicitly, and that is not incidental:
// crossing the claw⇄CLI boundary with an empty list is refused (C176)
// because the list inverts meaning there. On claude_code the list is
// inert (bypassPermissions keeps the full native toolset); it exists so
// the claw route is not a tool-less agent.
func TestFallbacksCompileToOrderedRoutes(t *testing.T) {
	src := fallbackWorkflow("  tools: [read_file, bash]\n", `    api:
      backend: "claw"
      model: "anthropic/claude-opus-5"
      on: [usage_window]
    gpt:
      backend: "claw"
      model: "openai/gpt-5.5"
      metered: true
`)
	cr := compileFallbackSrc(t, src)
	if cr.HasErrors() {
		t.Fatalf("expected a clean compile, got %+v", cr.Diagnostics)
	}
	agent, ok := cr.Workflow.Nodes["x"].(*AgentNode)
	if !ok {
		t.Fatal("node x is not an agent")
	}
	if len(agent.Fallbacks) != 2 {
		t.Fatalf("got %d routes, want 2", len(agent.Fallbacks))
	}
	if agent.Fallbacks[0].Name != "api" || agent.Fallbacks[1].Name != "gpt" {
		t.Errorf("declaration order not preserved: %+v", agent.Fallbacks)
	}
	if agent.Fallbacks[0].Model != "anthropic/claude-opus-5" {
		t.Errorf("route model = %q", agent.Fallbacks[0].Model)
	}
	if len(agent.Fallbacks[0].On) != 1 || agent.Fallbacks[0].On[0] != "usage_window" {
		t.Errorf("route triggers = %v", agent.Fallbacks[0].On)
	}
	if !agent.Fallbacks[1].Metered {
		t.Error("metered: true was dropped — the author's spend acknowledgement must survive compilation")
	}
}

// TestFallbackEmptyRouteIsAnError: a route with no target would re-issue
// the identical call that just failed.
func TestFallbackEmptyRouteIsAnError(t *testing.T) {
	src := fallbackWorkflow("", "    nowhere:\n      on: [usage_window]\n")
	cr := compileFallbackSrc(t, src)
	if !hasDiag(cr.Diagnostics, DiagFallbackMalformed) {
		t.Fatalf("expected C173, got %+v", cr.Diagnostics)
	}
	if !cr.HasErrors() {
		t.Error("C173 must be an error: a route to nowhere silently doubles the retry budget")
	}
}

// TestFallbackBackendWithoutModelIsAnError: model specs are not portable
// across backends, so the inherited model cannot be valid on the new one.
func TestFallbackBackendWithoutModelIsAnError(t *testing.T) {
	src := fallbackWorkflow("", "    api:\n      backend: \"claw\"\n")
	cr := compileFallbackSrc(t, src)
	if !hasDiag(cr.Diagnostics, DiagFallbackMalformed) {
		t.Fatalf("expected C173 for a backend switch with no model, got %+v", cr.Diagnostics)
	}
}

// TestFallbackDuplicateNameIsAnError: the name is the stable id a
// fall-through is reported by.
func TestFallbackDuplicateNameIsAnError(t *testing.T) {
	src := fallbackWorkflow("", `    api:
      model: "openai/gpt-5.5"
    api:
      model: "openai/gpt-5.4-mini"
`)
	cr := compileFallbackSrc(t, src)
	if !hasDiag(cr.Diagnostics, DiagFallbackMalformed) {
		t.Fatalf("expected C173 for a duplicate route name, got %+v", cr.Diagnostics)
	}
}

// TestFallbackUnknownTriggerWarns: the vocabulary is a soft set, so an
// out-of-tree runtime can extend it — but a typo must be visible.
func TestFallbackUnknownTriggerWarns(t *testing.T) {
	src := fallbackWorkflow("", "    api:\n      model: \"openai/gpt-5.5\"\n      on: [usage_windwo]\n")
	cr := compileFallbackSrc(t, src)
	if !hasDiag(cr.Diagnostics, DiagFallbackUnknownOn) {
		t.Fatalf("expected C175, got %+v", cr.Diagnostics)
	}
	if cr.HasErrors() {
		t.Error("C175 must be a warning: the trigger set is a soft registry")
	}
}

// TestFallbackUngatedRouteIsRefused: the permission gate is the
// anti-prompt-injection boundary. A route that cannot enforce it must
// not exist — pi already fails rather than degrades in this exact
// situation, and C176 adopts that precedent at compile time.
func TestFallbackUngatedRouteIsRefused(t *testing.T) {
	src := fallbackWorkflow("  permission: ask\n  tools: [read_file]\n",
		"    cheap:\n      backend: \"kimi\"\n      model: \"kimi-code/kimi-for-coding\"\n")
	cr := compileFallbackSrc(t, src)
	if !hasDiag(cr.Diagnostics, DiagFallbackUnsafeCross) {
		t.Fatalf("expected C176 for a gated node routing to a backend that cannot enforce the gate, got %+v", cr.Diagnostics)
	}
	if !cr.HasErrors() {
		t.Error("C176 must be an error: falling back UNGATED is the failure this check exists for")
	}
}

// externalHookBackends are the CLI backends whose PreToolUse hook is an
// EXTERNAL process. Both earned their entry with a live denial (a real model,
// a real tool call, a filesystem sentinel), and both are deny-only for the
// same structural reason — see gateEnforcingModes.
var externalHookBackends = []struct{ backend, model string }{
	{"kimi", "kimi-code/kimi-for-coding"},
	{"grok", "grok-4.6"},
}

func TestFallbackExternalHookDenyRouteIsAllowed(t *testing.T) {
	for _, tc := range externalHookBackends {
		t.Run(tc.backend, func(t *testing.T) {
			src := fallbackWorkflow("  permission: deny\n  tools: [read_file]\n",
				"    cheap:\n      backend: \""+tc.backend+"\"\n      model: \""+tc.model+"\"\n")
			cr := compileFallbackSrc(t, src)
			if hasDiag(cr.Diagnostics, DiagFallbackUnsafeCross) {
				t.Fatalf("%s's proven PreToolUse deny hook must admit permission: deny routes: %+v", tc.backend, cr.Diagnostics)
			}
		})
	}
}

func TestGatedPrimaryBackendIsScreened(t *testing.T) {
	src := "agent x:\n  backend: \"codex\"\n  model: \"gpt-5.4\"\n  system: p\n  permission: deny\n" +
		"\nprompt p:\n  hi\n\nworkflow w:\n  entry: x\n  x -> done\n"
	cr := compileFallbackSrc(t, src)
	if !hasDiag(cr.Diagnostics, DiagFallbackUnsafeCross) {
		t.Fatalf("a gated primary backend without live enforcement proof must C176: %+v", cr.Diagnostics)
	}
}

func TestExternalHookDenyPrimaryBackendIsAllowed(t *testing.T) {
	for _, tc := range externalHookBackends {
		t.Run(tc.backend, func(t *testing.T) {
			src := "agent x:\n  backend: \"" + tc.backend + "\"\n  model: \"" + tc.model + "\"\n  system: p\n  permission: deny\n" +
				"\nprompt p:\n  hi\n\nworkflow w:\n  entry: x\n  x -> done\n"
			cr := compileFallbackSrc(t, src)
			if hasDiag(cr.Diagnostics, DiagFallbackUnsafeCross) {
				t.Fatalf("%s permission: deny primary should compile: %+v", tc.backend, cr.Diagnostics)
			}
		})
	}
}

// TestExternalHookDenyWithAskRulesIsRefused: an explicit ask rule outranks the
// mode default, and an external hook process cannot pause the parent iterion
// run. Admitting it would silently downgrade the operator's `ask` to `deny`.
func TestExternalHookDenyWithAskRulesIsRefused(t *testing.T) {
	for _, tc := range externalHookBackends {
		t.Run(tc.backend, func(t *testing.T) {
			src := "agent x:\n  backend: \"" + tc.backend + "\"\n  model: \"" + tc.model + "\"\n  system: p\n  permission: deny\n" +
				"\nprompt p:\n  hi\n\nworkflow w:\n  entry: x\n  ask: [\"Bash(git push:*)\"]\n  x -> done\n"
			cr := compileFallbackSrc(t, src)
			if !hasDiag(cr.Diagnostics, DiagFallbackUnsafeCross) {
				t.Fatalf("%s cannot preserve an explicit ask rule from an external hook process: %+v", tc.backend, cr.Diagnostics)
			}
		})
	}
}

// TestFallbackToolsInversionIsRefused: an empty `tools:` list means ZERO
// tools on claw and the FULL native toolset on a CLI backend, so a
// crossing route silently changes what the node can DO — a read-only
// reviewer becomes an agent able to edit the code it is judging.
func TestFallbackToolsInversionIsRefused(t *testing.T) {
	src := "agent x:\n  backend: \"claw\"\n  model: \"anthropic/claude-opus-5\"\n  system: p\n" +
		"  fallbacks:\n    cli:\n      backend: \"claude_code\"\n      model: \"claude-opus-5\"\n" +
		"\nprompt p:\n  hi\n\nworkflow w:\n  entry: x\n  x -> done\n"
	cr := compileFallbackSrc(t, src)
	if !hasDiag(cr.Diagnostics, DiagFallbackUnsafeCross) {
		t.Fatalf("expected C176 for a claw⇄CLI crossing with no tools list, got %+v", cr.Diagnostics)
	}
}

// TestFallbackClawToCLIRefusedEvenWithToolsList: the two directions are
// not symmetric. Declaring `tools:` does NOT make a claw→CLI route safe
// — under the always-on bypassPermissions a CLI agent ignores the
// lowercase list entirely and carries the full native toolset, so a
// reviewer restricted to read_file would gain Edit/Write the moment the
// chain falls through, on a node the engine already admitted as a
// read-only parallel branch.
func TestFallbackClawToCLIRefusedEvenWithToolsList(t *testing.T) {
	src := "agent x:\n  backend: \"claw\"\n  model: \"anthropic/claude-opus-5\"\n  system: p\n" +
		"  tools: [read_file]\n" +
		"  fallbacks:\n    cli:\n      backend: \"claude_code\"\n      model: \"claude-opus-5\"\n" +
		"\nprompt p:\n  hi\n\nworkflow w:\n  entry: x\n  x -> done\n"
	cr := compileFallbackSrc(t, src)
	if !hasDiag(cr.Diagnostics, DiagFallbackUnsafeCross) {
		t.Fatalf("a claw→CLI route un-restricts the node whatever its tools list; expected C176, got %+v", cr.Diagnostics)
	}
}

// TestFallbackCLIToClawAllowedWithToolsList: the other direction
// RESTRICTS, which is the documented pattern — a CLI node declares the
// tools its claw route will actually get. The list is inert on the CLI
// primary and load-bearing on the route, so this must compile clean.
func TestFallbackCLIToClawAllowedWithToolsList(t *testing.T) {
	src := fallbackWorkflow("  tools: [read_file, bash]\n",
		"    api:\n      backend: \"claw\"\n      model: \"anthropic/claude-opus-5\"\n")
	cr := compileFallbackSrc(t, src)
	if hasDiag(cr.Diagnostics, DiagFallbackUnsafeCross) {
		t.Fatalf("CLI→claw with an explicit tools list is the documented pattern: %+v", cr.Diagnostics)
	}
}

func TestFallbackPersistCrossBackendRefused(t *testing.T) {
	src := fallbackWorkflow("  session: persist\n  tools: [read_file, bash]\n",
		"    api:\n      backend: \"claw\"\n      model: \"anthropic/claude-opus-5\"\n")
	cr := compileFallbackSrc(t, src)
	if !hasDiag(cr.Diagnostics, DiagFallbackUnsafeCross) {
		t.Fatalf("session: persist + backend-changing fallback must C176, got %+v", cr.Diagnostics)
	}
}

func TestFallbackInheritIfAvailableCrossBackendRefused(t *testing.T) {
	src := fallbackWorkflow("  session: inherit_if_available\n  tools: [read_file, bash]\n",
		"    api:\n      backend: \"claw\"\n      model: \"anthropic/claude-opus-5\"\n")
	cr := compileFallbackSrc(t, src)
	if !hasDiag(cr.Diagnostics, DiagFallbackUnsafeCross) {
		t.Fatalf("session: inherit_if_available + backend-changing fallback must C176, got %+v", cr.Diagnostics)
	}
}

// TestFallbackEnvRefBackendDefersToRuntime: the literal text is not the
// resolved backend, so a check keyed on it would misfire.
func TestFallbackEnvRefBackendDefersToRuntime(t *testing.T) {
	src := fallbackWorkflow("  permission: deny\n  tools: [read_file]\n",
		"    dyn:\n      backend: \"${FALLBACK_BACKEND:-claw}\"\n      model: \"anthropic/claude-opus-5\"\n")
	cr := compileFallbackSrc(t, src)
	if hasDiag(cr.Diagnostics, DiagFallbackUnsafeCross) {
		t.Errorf("an env-ref backend must defer to the runtime, not be judged on its literal: %+v", cr.Diagnostics)
	}
	if cr.HasErrors() {
		t.Errorf("unexpected errors: %+v", cr.Diagnostics)
	}
}

// TestNodeWithoutFallbacksIsUntouched guards the blast radius: every bot
// that predates the block must compile exactly as before.
func TestNodeWithoutFallbacksIsUntouched(t *testing.T) {
	src := "agent x:\n  backend: \"claw\"\n  system: p\n\nprompt p:\n  hi\n\nworkflow w:\n  entry: x\n  x -> done\n"
	cr := compileFallbackSrc(t, src)
	for _, code := range []DiagCode{DiagFallbackMalformed, DiagFallbackUnknownOn, DiagFallbackUnsafeCross, DiagFallbackDrift} {
		if hasDiag(cr.Diagnostics, code) {
			t.Errorf("%s fired on a node with no fallbacks: block", code)
		}
	}
}

// TestFallbackUngatedRouteRefusedFromWorkflowGate: `permission:` on the
// workflow block is the DOCUMENTED place to declare the mode, and a
// node's own field means "inherit". A check reading only the node field
// let the common shape compile clean.
func TestFallbackUngatedRouteRefusedFromWorkflowGate(t *testing.T) {
	src := "agent x:\n  backend: \"claude_code\"\n  model: \"claude-opus-5\"\n  system: p\n  tools: [read_file]\n" +
		"  fallbacks:\n    cheap:\n      backend: \"codex\"\n      model: \"gpt-5.4\"\n" +
		"\nprompt p:\n  hi\n\nworkflow w:\n  entry: x\n  permission: deny\n  x -> done\n"
	cr := compileFallbackSrc(t, src)
	if !hasDiag(cr.Diagnostics, DiagFallbackUnsafeCross) {
		t.Fatalf("expected C176 for a workflow-level gate, got %+v", cr.Diagnostics)
	}
}

// TestFallbackUngatedRouteRefusedOnAutoBackend: whether a ROUTE can
// enforce the gate is a property of the route's own backend, so the
// check must not be skipped just because the node's backend resolves at
// run time — which is the shipped default shape.
func TestFallbackUngatedRouteRefusedOnAutoBackend(t *testing.T) {
	src := "agent x:\n  model: \"claude-opus-5\"\n  system: p\n  permission: deny\n  tools: [read_file]\n" +
		"  fallbacks:\n    cheap:\n      backend: \"codex\"\n      model: \"gpt-5.4\"\n" +
		"\nprompt p:\n  hi\n\nworkflow w:\n  entry: x\n  x -> done\n"
	cr := compileFallbackSrc(t, src)
	if !hasDiag(cr.Diagnostics, DiagFallbackUnsafeCross) {
		t.Fatalf("expected C176 with no node backend: declared, got %+v", cr.Diagnostics)
	}
}

// TestFallbackDriftDoesNotLieAboutReasoningEffort: grok DOES carry a
// reasoning-effort dial (--reasoning-effort), so claiming otherwise is
// a diagnostic asserting a fact the engine contradicts — which trains
// authors to ignore the one signal for real capability drift.
func TestFallbackDriftDoesNotLieAboutReasoningEffort(t *testing.T) {
	src := "agent x:\n  backend: \"claude_code\"\n  model: \"claude-opus-5\"\n  system: p\n" +
		"  reasoning_effort: high\n  tools: [read_file]\n" +
		"  fallbacks:\n    g:\n      backend: \"grok\"\n      model: \"grok-code\"\n" +
		"\nprompt p:\n  hi\n\nworkflow w:\n  entry: x\n  x -> done\n"
	cr := compileFallbackSrc(t, src)
	if hasDiag(cr.Diagnostics, DiagFallbackDrift) {
		t.Errorf("C177 claimed grok has no reasoning-effort dial: %+v", cr.Diagnostics)
	}

	// kimi genuinely has none — the warning must still fire there.
	kimi := "agent x:\n  backend: \"claude_code\"\n  model: \"claude-opus-5\"\n  system: p\n" +
		"  reasoning_effort: high\n  tools: [read_file]\n" +
		"  fallbacks:\n    k:\n      backend: \"kimi\"\n      model: \"kimi-code/kimi-for-coding\"\n" +
		"\nprompt p:\n  hi\n\nworkflow w:\n  entry: x\n  x -> done\n"
	if !hasDiag(compileFallbackSrc(t, kimi).Diagnostics, DiagFallbackDrift) {
		t.Error("C177 must still fire for a backend that really has no dial")
	}
}

// TestGatedExternalHookBackendWarnsAboutSandbox: the gate on grok/kimi runs
// through a HOST-side hook, and the shipped default is `sandbox: auto`. Without
// this warning the coupling is invisible until the agent node dies mid-run
// (#498 review, R9ab052) — the headline capability looking unreachable in the
// default configuration.
func TestGatedExternalHookBackendWarnsAboutSandbox(t *testing.T) {
	for _, tc := range externalHookBackends {
		t.Run(tc.backend, func(t *testing.T) {
			src := "agent x:\n  backend: \"" + tc.backend + "\"\n  model: \"" + tc.model + "\"\n  system: p\n  permission: deny\n" +
				"\nprompt p:\n  hi\n\nworkflow w:\n  entry: x\n  x -> done\n"
			cr := compileFallbackSrc(t, src)
			if !hasDiag(cr.Diagnostics, DiagGatedCLIBackendSandbox) {
				t.Fatalf("expected C136 for a gated %s node with no sandbox opt-out: %+v", tc.backend, cr.Diagnostics)
			}
			if cr.HasErrors() {
				t.Errorf("C136 must WARN, not reject: --sandbox none and ITERION_SANDBOX_DEFAULT=none both make this legal at run time")
			}
		})
	}
}

func TestGatedExternalHookBackendSilentWhenSandboxDeclined(t *testing.T) {
	cases := map[string]string{
		"workflow-level": "agent x:\n  backend: \"grok\"\n  model: \"grok-4.6\"\n  system: p\n  permission: deny\n" +
			"\nprompt p:\n  hi\n\nworkflow w:\n  sandbox: none\n  entry: x\n  x -> done\n",
		"node-level": "agent x:\n  backend: \"grok\"\n  model: \"grok-4.6\"\n  system: p\n  permission: deny\n  sandbox: none\n" +
			"\nprompt p:\n  hi\n\nworkflow w:\n  entry: x\n  x -> done\n",
		"gate off": "agent x:\n  backend: \"grok\"\n  model: \"grok-4.6\"\n  system: p\n" +
			"\nprompt p:\n  hi\n\nworkflow w:\n  entry: x\n  x -> done\n",
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			cr := compileFallbackSrc(t, src)
			if hasDiag(cr.Diagnostics, DiagGatedCLIBackendSandbox) {
				t.Fatalf("C136 fired although the run can reach the host-side hook: %+v", cr.Diagnostics)
			}
		})
	}
}

// TestGatedExternalHookFallbackWarnsAboutSandbox: a fallback ROUTE to grok/kimi
// carries the same host-side requirement as a primary one — the fall-through
// would otherwise die mid-run, at the worst possible moment.
func TestGatedExternalHookFallbackWarnsAboutSandbox(t *testing.T) {
	src := "agent x:\n  backend: \"claude_code\"\n  model: \"claude-opus-5\"\n  system: p\n  permission: deny\n  tools: [read_file]\n" +
		"  fallbacks:\n    cheap:\n      backend: \"kimi\"\n      model: \"kimi-code/kimi-for-coding\"\n" +
		"\nprompt p:\n  hi\n\nworkflow w:\n  entry: x\n  x -> done\n"
	cr := compileFallbackSrc(t, src)
	if !hasDiag(cr.Diagnostics, DiagGatedCLIBackendSandbox) {
		t.Fatalf("expected C136 for a gated fallback route to kimi: %+v", cr.Diagnostics)
	}
}

// TestSandboxedClawAskCapablePolicyWarns: claw enforces the gate inside the
// sandbox runner (pre-task policy envelope), EXCEPT an Ask decision — no seam
// pauses the parent run from inside the container. A policy that can produce
// one must warn at compile time (warning, not error: --sandbox none and
// ITERION_SANDBOX_DEFAULT=none make the run legal without the workflow
// saying anything).
func TestSandboxedClawAskCapablePolicyWarns(t *testing.T) {
	cases := map[string]string{
		"mode ask, claw primary": "agent x:\n  backend: \"claw\"\n  model: \"anthropic/claude-opus-5\"\n  system: p\n  permission: ask\n  tools: [read_file]\n" +
			"\nprompt p:\n  hi\n\nworkflow w:\n  entry: x\n  x -> done\n",
		"mode deny + ask rule, claw fallback": "agent x:\n  backend: \"claude_code\"\n  model: \"claude-opus-5\"\n  system: p\n  permission: deny\n  tools: [read_file]\n" +
			"  fallbacks:\n    api:\n      backend: \"claw\"\n      model: \"openai/gpt-5.5\"\n" +
			"\nprompt p:\n  hi\n\nworkflow w:\n  entry: x\n  ask: [\"Bash(git push:*)\"]\n  x -> done\n",
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			cr := compileFallbackSrc(t, src)
			if !hasDiag(cr.Diagnostics, DiagGatedCLIBackendSandbox) {
				t.Fatalf("expected C136 for an ask-capable policy on a sandboxed claw route: %+v", cr.Diagnostics)
			}
			if cr.HasErrors() {
				t.Errorf("C136 must WARN, not reject: the run may be launched unsandboxed")
			}
		})
	}
}

// TestSandboxedClawDenyPolicySilent: a deny policy with no ask rules is
// enforceable inside the sandbox runner — no C136.
func TestSandboxedClawDenyPolicySilent(t *testing.T) {
	cases := map[string]string{
		"claw primary": "agent x:\n  backend: \"claw\"\n  model: \"anthropic/claude-opus-5\"\n  system: p\n  permission: deny\n  tools: [read_file]\n" +
			"\nprompt p:\n  hi\n\nworkflow w:\n  entry: x\n  x -> done\n",
		"claw fallback": "agent x:\n  backend: \"claude_code\"\n  model: \"claude-opus-5\"\n  system: p\n  permission: deny\n  tools: [web_fetch]\n" +
			"  fallbacks:\n    api:\n      backend: \"claw\"\n      model: \"openai/gpt-5.5\"\n" +
			"\nprompt p:\n  hi\n\nworkflow w:\n  entry: x\n  x -> done\n",
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			cr := compileFallbackSrc(t, src)
			if hasDiag(cr.Diagnostics, DiagGatedCLIBackendSandbox) {
				t.Fatalf("C136 fired for a deny-only policy claw can enforce sandboxed: %+v", cr.Diagnostics)
			}
		})
	}
}

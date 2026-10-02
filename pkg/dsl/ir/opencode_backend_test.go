package ir

import "testing"

// TestOpenCodeCommandOverrideIsDiagnosed: delegate.Task.Command is read only
// by the claude_code backend, so a per-node `command:` anywhere else is
// inert. C174 is what tells the author instead of letting it look honoured.
func TestOpenCodeCommandOverrideIsDiagnosed(t *testing.T) {
	// The class, not one site: every backend that resolves argv[0] from its
	// own registry construction rather than from the node.
	for _, backend := range []string{"opencode", "kimi", "grok", "pi", "claw", "codex"} {
		t.Run(backend+"/warns", func(t *testing.T) {
			src := "agent a:\n  model: \"m\"\n  backend: \"" + backend + "\"\n  command: \"/opt/bin/agent\"\n" +
				"workflow w:\n  entry: a\n  a -> done\n"
			if got := countCode(compileFile(t, src), DiagCode("C174")); got != 1 {
				t.Fatalf("backend %q: C174 = %d, want 1 — command: is inert there", backend, got)
			}
		})
	}
	t.Run("claude_code/silent", func(t *testing.T) {
		src := "agent a:\n  model: \"m\"\n  backend: \"claude_code\"\n  command: \"/opt/bin/agent\"\n" +
			"workflow w:\n  entry: a\n  a -> done\n"
		if got := countCode(compileFile(t, src), DiagCode("C174")); got != 0 {
			t.Fatalf("claude_code: C174 = %d, want 0 — it is the one backend that consumes command:", got)
		}
	})
}

// TestOpenCodeProviderHintIsDiagnosed: opencode resolves its own credentials
// from its own auth store, so iterion's credential-routing hint has nowhere
// to land in its argv — and the executor collapses a hint-only chain.
func TestOpenCodeProviderHintIsDiagnosed(t *testing.T) {
	// The class is defined by the RUNTIME: model.providerFallbackEligible
	// walks a multi-element chain for claude_code alone, so on every other
	// backend everything after the first element is a no-op. pi is in the
	// class even though it folds the HEAD hint into its own argv.
	for _, backend := range []string{"opencode", "kimi", "grok", "pi", "claw", "codex"} {
		t.Run(backend+"/warns", func(t *testing.T) {
			src := "agent a:\n  model: \"m\"\n  backend: \"" + backend + "\"\n  provider: \"anthropic,openai\"\n" +
				"workflow w:\n  entry: a\n  a -> done\n"
			if got := countCode(compileFile(t, src), DiagCode("C088")); got != 1 {
				t.Fatalf("backend %q: C088 = %d, want 1 — the provider chain is a no-op there", backend, got)
			}
		})
	}
	// claude_code is the one backend whose chain the runtime walks.
	t.Run("claude_code/silent", func(t *testing.T) {
		src := "agent a:\n  model: \"m\"\n  backend: \"claude_code\"\n  provider: \"anthropic,openai\"\n" +
			"workflow w:\n  entry: a\n  a -> done\n"
		if got := countCode(compileFile(t, src), DiagCode("C088")); got != 0 {
			t.Fatalf("claude_code: C088 = %d, want 0 — its chain IS walked", got)
		}
	})
}

// TestOpenCodeHasAReasoningEffortDial: opencode carries `--variant`, so the
// "no reasoning-effort dial" warning on a route to it would be FALSE — and a
// diagnostic that is wrong teaches authors to ignore it.
func TestOpenCodeHasAReasoningEffortDial(t *testing.T) {
	src := "agent a:\n  model: \"m\"\n  backend: \"claude_code\"\n  reasoning_effort: high\n" +
		"  fallbacks:\n    backup:\n      backend: \"opencode\"\n      model: \"n\"\n" +
		"workflow w:\n  entry: a\n  a -> done\n"
	if got := countCode(compileFile(t, src), DiagCode("C177")); got != 0 {
		t.Fatalf("C177 = %d, want 0 — opencode passes --variant: %+v", got, compileFile(t, src).Diagnostics)
	}

	// Control: a backend with no dial still warns, so the assertion above
	// is not a test that cannot fail.
	noDial := "agent a:\n  model: \"m\"\n  backend: \"claude_code\"\n  reasoning_effort: high\n" +
		"  fallbacks:\n    backup:\n      backend: \"kimi\"\n      model: \"n\"\n" +
		"workflow w:\n  entry: a\n  a -> done\n"
	if got := countCode(compileFile(t, noDial), DiagCode("C177")); got != 1 {
		t.Fatalf("control: C177 on kimi = %d, want 1", got)
	}
}

// TestOpenCodeCannotEnforceThePermissionGate: membership in the gate table is
// earned by a live denial, never declared. opencode has none, so a gated node
// routed to it is refused at COMPILE time rather than run ungated.
func TestOpenCodeCannotEnforceThePermissionGate(t *testing.T) {
	for _, mode := range []string{"ask", "deny"} {
		t.Run(mode, func(t *testing.T) {
			src := "agent a:\n  model: \"m\"\n  backend: \"opencode\"\n" +
				"workflow w:\n  permission: " + mode + "\n  " + mode + ": [\"Bash\"]\n  entry: a\n  a -> done\n"
			if got := countCode(compileFile(t, src), DiagCode("C176")); got != 1 {
				t.Fatalf("permission: %s on opencode: C176 = %d, want 1 — the run would be UNGATED", mode, got)
			}
		})
	}
}

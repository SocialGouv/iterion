package parser

import "testing"

// A bare dotted hostname in the sandbox network rules is read whole —
// the helper's own doc promised `github.com` and the parser stopped at
// the dot.
func TestSandboxRulesReadABareDottedHost(t *testing.T) {
	src := "workflow w:\n  entry: a\n  sandbox:\n    network:\n      mode: allowlist\n      rules: [github.com, \"!**.evil.site\", api.github.com]\n  a -> done\n"
	res := Parse("x.bot", src)
	if len(res.Diagnostics) != 0 {
		t.Fatalf("unexpected diagnostics: %v", res.Diagnostics)
	}
	rules := res.File.Workflows[0].Sandbox.Network.Rules
	if len(rules) != 3 || rules[0] != "github.com" || rules[2] != "api.github.com" {
		t.Fatalf("rules read as %v", rules)
	}
}

// The bare word is the string it spells on the properties a first draft
// writes bare, and a keyword is a word like any other there.
func TestABareWordIsAStringValue(t *testing.T) {
	src := "agent a:\n  backend: claw\n  provider: anthropic\n  description: agent\n  memory:\n    enabled: true\n    scope: shared\n    visibility: project\n\nworkflow w:\n  entry: a\n  a -> done\n"
	res := Parse("x.bot", src)
	if len(res.Diagnostics) != 0 {
		t.Fatalf("unexpected diagnostics: %v", res.Diagnostics)
	}
	a := res.File.Agents[0]
	if a.Backend != "claw" || a.Provider != "anthropic" || a.Description != "agent" {
		t.Fatalf("agent: backend %q provider %q description %q", a.Backend, a.Provider, a.Description)
	}
	if a.Memory == nil || a.Memory.Scope == nil || *a.Memory.Scope != "shared" || a.Memory.Visibility == nil || *a.Memory.Visibility != "project" {
		t.Fatalf("memory: %+v", a.Memory)
	}
}

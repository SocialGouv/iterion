package ir

import "testing"

// The routing fields (model, backend, provider, interaction_model — on the
// node and on its fallbacks routes) resolve vars.* at dispatch and nothing
// else, so the compiler says so: an undeclared var is C033, any other
// `{{…}}` span — a foreign namespace, a misspelt one, an unterminated one —
// is C148; and C087 no longer calls a templated provider "ignored".
// examples/clarify shipped `model: "{{vars.model}}"` and died at the first
// delegation with an "invalid spec" from claw, after a clean validate.
func TestRoutingFieldRefs(t *testing.T) {
	const head = "vars:\n  m: string = \"anthropic/claude-sonnet-4-6\"\n  b: string = \"claw\"\n\nprompt p:\n  Hi.\n\nschema s:\n  ok: bool\n\n"
	const tail = "\nworkflow w:\n  entry: a\n  a -> done\n"
	agent := func(props string) string { return "agent a:\n  system: p\n" + props }
	cases := []struct {
		name string
		body string
		want DiagCode // "" = none of C033, C148, C087
	}{
		{"declared vars resolve in model, backend, provider and interaction_model",
			agent("  model: \"{{vars.m}}\"\n  backend: \"{{vars.b}}\"\n  provider: \"{{vars.b}}\"\n  interaction_model: \"{{vars.m}}\"\n"), ""},
		{"a plain id and an env form are not references",
			agent("  model: \"${M:-anthropic/claude-opus-5}\"\n  backend: \"claw\"\n"), ""},
		{"undeclared var in model", agent("  model: \"{{vars.nope}}\"\n"), DiagUndeclaredVar},
		{"undeclared var in backend", agent("  backend: \"{{vars.nope}}\"\n"), DiagUndeclaredVar},
		{"undeclared var in interaction_model", agent("  interaction_model: \"{{vars.nope}}\"\n"), DiagUndeclaredVar},
		{"an output in model", agent("  model: \"{{outputs.a.m}}\"\n"), DiagRoutingFieldRef},
		{"an input in provider", agent("  provider: \"{{input.p}}\"\n"), DiagRoutingFieldRef},
		{"a loop counter in backend", agent("  backend: \"{{loop.x.iteration}}\"\n"), DiagRoutingFieldRef},
		{"a secret in model", agent("  model: \"{{secrets.token}}\"\n"), DiagRoutingFieldRef},
		{"the singular misspelling var.m", agent("  model: \"{{var.m}}\"\n"), DiagRoutingFieldRef},
		{"a capitalised namespace", agent("  model: \"{{Vars.m}}\"\n"), DiagRoutingFieldRef},
		{"a namespace with no path", agent("  model: \"{{vars}}\"\n"), DiagRoutingFieldRef},
		{"an env namespace that does not exist", agent("  model: \"{{env.HOME}}\"\n"), DiagRoutingFieldRef},
		{"an unterminated reference", agent("  model: \"{{vars.m\"\n"), DiagRoutingFieldRef},
		{"judge model from an undeclared var",
			"judge a:\n  model: \"{{vars.nope}}\"\n  system: p\n  output: s\n", DiagUndeclaredVar},
		{"fallback route model from an undeclared var",
			agent("  model: \"anthropic/claude-sonnet-4-6\"\n  fallbacks:\n    alt:\n      backend: \"claw\"\n      model: \"{{vars.nope}}\"\n"), DiagUndeclaredVar},
		{"fallback route provider from an output",
			agent("  model: \"anthropic/claude-sonnet-4-6\"\n  fallbacks:\n    alt:\n      backend: \"claw\"\n      provider: \"{{outputs.a.p}}\"\n"), DiagRoutingFieldRef},
		{"human companion model from an output",
			"human a:\n  instructions: p\n  output: s\n  interaction: llm\n  system: p\n  model: \"{{outputs.a.m}}\"\n", DiagRoutingFieldRef},
		{"human interaction_model from an output",
			"human a:\n  instructions: p\n  output: s\n  interaction: llm\n  system: p\n  interaction_model: \"{{outputs.a.m}}\"\n", DiagRoutingFieldRef},
		{"supervisor model is not rendered at all",
			agent("  model: \"anthropic/claude-sonnet-4-6\"\n") + "\nsupervisor sup:\n  watches: [a]\n  model: \"{{vars.m}}\"\n", DiagRoutingFieldRef},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, diags := compileSource(t, head+tc.body+tail)
			var seen []DiagCode
			for _, d := range diags {
				switch d.Code {
				case DiagUndeclaredVar, DiagRoutingFieldRef:
					seen = append(seen, d.Code)
					if d.Severity != SeverityError {
						t.Errorf("%s is %v, want an error", d.Code, d.Severity)
					}
				case DiagUnknownProvider:
					t.Errorf("C087 on a templated provider: %s", d.Error())
				}
			}
			switch {
			case tc.want == "" && len(seen) != 0:
				t.Fatalf("unexpected routing diagnostics %v in\n%s", seen, diags)
			case tc.want != "" && len(seen) != 1:
				t.Fatalf("want exactly one %s, got %v in\n%s", tc.want, seen, diags)
			case tc.want != "" && seen[0] != tc.want:
				t.Fatalf("want %s, got %s", tc.want, seen[0])
			}
		})
	}
}

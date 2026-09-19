package ir

import "testing"

// The routing fields (model, backend, provider, interaction_model — on the
// node and on its fallbacks routes) resolve vars.* at dispatch and nothing
// else, so the compiler says so: an undeclared var is C033 (an error, as
// everywhere), any other `{{…}}` span — a foreign namespace, a misspelt one,
// a dotted var path, the raw `{{!…}}` form, an unterminated one — is C148,
// a warning (the runtime failure at the first delegation stays loud, and a
// bot in the field keeps compiling); C087 no longer calls a templated
// provider "ignored"; a supervisor's model, which renders no template, gets
// C148 too. examples/clarify shipped `model: "{{vars.model}}"` and died at
// the first delegation with an "invalid spec" from claw, after a clean
// validate.
func TestRoutingFieldRefs(t *testing.T) {
	const head = "vars:\n  m: string = \"anthropic/claude-sonnet-4-6\"\n  b: string = \"claw\"\n\nprompt p:\n  Hi.\n\nschema s:\n  ok: bool\n\n"
	agent := func(props string) string { return "agent a:\n  system: p\n" + props }
	type tc struct {
		name string
		body string
		wf   string   // extra properties of the workflow block
		want DiagCode // "" = none of C033, C148, C087
	}
	cases := []tc{
		{name: "workflow default_backend from a declared var", body: agent(""), wf: "  default_backend: \"{{vars.b}}\"\n"},
		{name: "workflow default_backend from an undeclared var", body: agent(""), wf: "  default_backend: \"{{vars.nope}}\"\n", want: DiagUndeclaredVar},
		{name: "workflow default_backend from an output", body: agent(""), wf: "  default_backend: \"{{outputs.a.b}}\"\n", want: DiagRoutingFieldRef},
		{name: "declared vars resolve in model, backend, provider and interaction_model",
			body: agent("  model: \"{{vars.m}}\"\n  backend: \"{{vars.b}}\"\n  provider: \"{{vars.b}}\"\n  interaction_model: \"{{vars.m}}\"\n")},
		{name: "a plain id and an env form are not references",
			body: agent("  model: \"${M:-anthropic/claude-opus-5}\"\n  backend: \"claw\"\n")},
		{name: "undeclared var in model", body: agent("  model: \"{{vars.nope}}\"\n"), want: DiagUndeclaredVar},
		{name: "undeclared var in backend", body: agent("  backend: \"{{vars.nope}}\"\n"), want: DiagUndeclaredVar},
		{name: "undeclared var in interaction_model", body: agent("  interaction_model: \"{{vars.nope}}\"\n"), want: DiagUndeclaredVar},
		{name: "an output in model", body: agent("  model: \"{{outputs.a.m}}\"\n"), want: DiagRoutingFieldRef},
		{name: "an input in provider", body: agent("  provider: \"{{input.p}}\"\n"), want: DiagRoutingFieldRef},
		{name: "a loop counter in backend", body: agent("  backend: \"{{loop.x.iteration}}\"\n"), want: DiagRoutingFieldRef},
		{name: "a secret in model", body: agent("  model: \"{{secrets.token}}\"\n"), want: DiagRoutingFieldRef},
		{name: "the singular misspelling var.m", body: agent("  model: \"{{var.m}}\"\n"), want: DiagRoutingFieldRef},
		{name: "a capitalised namespace", body: agent("  model: \"{{Vars.m}}\"\n"), want: DiagRoutingFieldRef},
		{name: "a namespace with no path", body: agent("  model: \"{{vars}}\"\n"), want: DiagRoutingFieldRef},
		{name: "an env namespace that does not exist", body: agent("  model: \"{{env.HOME}}\"\n"), want: DiagRoutingFieldRef},
		{name: "an unterminated reference", body: agent("  model: \"{{vars.m\"\n"), want: DiagRoutingFieldRef},
		{name: "a dotted path under a declared var", body: agent("  model: \"{{vars.m.id}}\"\n"), want: DiagRoutingFieldRef},
		{name: "the raw form the resolver does not read", body: agent("  model: \"{{!vars.m}}\"\n"), want: DiagRoutingFieldRef},
		{name: "judge model from an undeclared var",
			body: "judge a:\n  model: \"{{vars.nope}}\"\n  system: p\n  output: s\n", want: DiagUndeclaredVar},
		{name: "fallback route model from an undeclared var",
			body: agent("  model: \"anthropic/claude-sonnet-4-6\"\n  fallbacks:\n    alt:\n      backend: \"claw\"\n      model: \"{{vars.nope}}\"\n"), want: DiagUndeclaredVar},
		{name: "fallback route provider from an output",
			body: agent("  model: \"anthropic/claude-sonnet-4-6\"\n  fallbacks:\n    alt:\n      backend: \"claw\"\n      provider: \"{{outputs.a.p}}\"\n"), want: DiagRoutingFieldRef},
		{name: "human companion model from an output",
			body: "human a:\n  instructions: p\n  output: s\n  interaction: llm\n  system: p\n  model: \"{{outputs.a.m}}\"\n", want: DiagRoutingFieldRef},
		{name: "human interaction_model from an output",
			body: "human a:\n  instructions: p\n  output: s\n  interaction: llm\n  system: p\n  interaction_model: \"{{outputs.a.m}}\"\n", want: DiagRoutingFieldRef},
		{name: "supervisor model is not rendered at all",
			body: agent("  model: \"anthropic/claude-sonnet-4-6\"\n") + "\nsupervisor sup:\n  watches: [a]\n  model: \"{{vars.m}}\"\n", want: DiagRoutingFieldRef},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, diags := compileSource(t, head+c.body+"\nworkflow w:\n  entry: a\n"+c.wf+"  a -> done\n")
			var seen []DiagCode
			for _, d := range diags {
				switch d.Code {
				case DiagUndeclaredVar:
					seen = append(seen, d.Code)
					if d.Severity != SeverityError {
						t.Errorf("C033 is %v, want an error", d.Severity)
					}
				case DiagRoutingFieldRef:
					seen = append(seen, d.Code)
					if d.Severity != SeverityWarning {
						t.Errorf("C148 is %v, want a warning (a fielded bot keeps compiling; the node fails loud at its first delegation)", d.Severity)
					}
				case DiagUnknownProvider:
					t.Errorf("C087 on a templated provider: %s", d.Error())
				}
			}
			switch {
			case c.want == "" && len(seen) != 0:
				t.Fatalf("unexpected routing diagnostics %v in\n%s", seen, diags)
			case c.want != "" && len(seen) != 1:
				t.Fatalf("want exactly one %s, got %v in\n%s", c.want, seen, diags)
			case c.want != "" && seen[0] != c.want:
				t.Fatalf("want %s, got %s", c.want, seen[0])
			}
		})
	}
}

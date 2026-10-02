package ir

import (
	"strings"
	"testing"
)

// TestCompileAmbientContext carries `ambient_context:` from the parser to the
// IR at the workflow level and on both LLM node kinds, verbatim: a node's
// `none` must not collapse to "" and read as "inherit the workflow".
func TestCompileAmbientContext(t *testing.T) {
	src := `
agent a:
  model: "m"
  ambient_context: none

judge j:
  model: "m"
  ambient_context: operator

workflow w:
  entry: a
  ambient_context: all
  a -> j
  j -> done
`
	w := mustCompile(t, src)
	if w.AmbientContext != "all" {
		t.Errorf("workflow.AmbientContext = %q, want all", w.AmbientContext)
	}
	if a := w.Nodes["a"].(*AgentNode); a.AmbientContext != "none" {
		t.Errorf("agent.AmbientContext = %q, want none", a.AmbientContext)
	}
	if j := w.Nodes["j"].(*JudgeNode); j.AmbientContext != "operator" {
		t.Errorf("judge.AmbientContext = %q, want operator", j.AmbientContext)
	}
}

func TestInvalidAmbientContextIsAnError(t *testing.T) {
	cases := map[string]string{
		"agent":              "agent a:\n  model: \"m\"\n  ambient_context: project\nworkflow w:\n  entry: a\n  a -> done\n",
		"judge":              "judge a:\n  model: \"m\"\n  ambient_context: off\nworkflow w:\n  entry: a\n  a -> done\n",
		"workflow":           "agent a:\n  model: \"m\"\nworkflow w:\n  entry: a\n  ambient_context: everything\n  a -> done\n",
		"legacy source name": "agent a:\n  model: \"m\"\n  ambient_context: user\nworkflow w:\n  entry: a\n  a -> done\n",
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			r := compileFile(t, src)
			if countCode(r, DiagInvalidAmbientContext) == 0 {
				t.Fatalf("no C184 for an invalid value: %+v", r.Diagnostics)
			}
			for _, d := range r.Diagnostics {
				if d.Code == DiagInvalidAmbientContext && d.Severity != SeverityError {
					t.Fatalf("C184 is a %v, want an error", d.Severity)
				}
			}
		})
	}
}

func TestAmbientContextNotEnforcedWarnsOnExplicitValuesOnly(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want int
	}{
		{
			name: "explicit value on a non-enforcing node backend",
			src:  "agent a:\n  backend: opencode\n  model: \"m\"\n  ambient_context: none\nworkflow w:\n  entry: a\n  a -> done\n",
			want: 1,
		},
		{
			name: "explicit value, backend inherited from default_backend",
			src:  "agent a:\n  model: \"m\"\n  ambient_context: workspace\nworkflow w:\n  entry: a\n  default_backend: kimi\n  a -> done\n",
			want: 1,
		},
		{
			name: "explicit value on an enforcing backend",
			src:  "agent a:\n  backend: claude_code\n  model: \"m\"\n  ambient_context: none\nworkflow w:\n  entry: a\n  a -> done\n",
			want: 0,
		},
		{
			name: "no value: the default is documented, not warned",
			src:  "agent a:\n  backend: grok\n  model: \"m\"\nworkflow w:\n  entry: a\n  a -> done\n",
			want: 0,
		},
		{
			name: "unresolved backend: the compiler cannot know",
			src:  "agent a:\n  model: \"m\"\n  ambient_context: none\nworkflow w:\n  entry: a\n  a -> done\n",
			want: 0,
		},
		{
			name: "a fallback route to a non-enforcing backend",
			src:  "agent a:\n  backend: claude_code\n  model: \"m\"\n  ambient_context: none\n  fallbacks:\n    backup:\n      backend: opencode\n      model: \"n\"\nworkflow w:\n  entry: a\n  a -> done\n",
			want: 1,
		},
		{
			name: "a skip route runs no backend",
			src:  "agent a:\n  backend: claude_code\n  model: \"m\"\n  ambient_context: none\n  fallbacks:\n    give_up:\n      action: skip\nworkflow w:\n  entry: a\n  a -> done\n",
			want: 0,
		},
		{
			name: "workflow value no node can honour",
			src:  "agent a:\n  backend: grok\n  model: \"m\"\nworkflow w:\n  entry: a\n  ambient_context: none\n  a -> done\n",
			want: 1,
		},
		{
			name: "workflow value one node honours",
			src:  "agent a:\n  backend: grok\n  model: \"m\"\nagent b:\n  backend: claw\n  model: \"m\"\nworkflow w:\n  entry: a\n  ambient_context: none\n  a -> b\n  b -> done\n",
			want: 0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := compileFile(t, c.src)
			if got := countCode(r, DiagAmbientContextNotEnforced); got != c.want {
				t.Fatalf("C185 count = %d, want %d: %+v", got, c.want, r.Diagnostics)
			}
			for _, d := range r.Diagnostics {
				if d.Code == DiagAmbientContextNotEnforced {
					if d.Severity != SeverityWarning {
						t.Fatalf("C185 is a %v, want a warning", d.Severity)
					}
					if !strings.Contains(d.Message, "ambient_context") {
						t.Fatalf("C185 does not name the field: %q", d.Message)
					}
				}
			}
		})
	}
}

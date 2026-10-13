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

func TestAmbientContextNotEnforcedWarnsOnUnmeasuredValuesOnly(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want int
	}{
		{
			name: "explicit value on an unmeasured backend",
			src:  "agent a:\n  backend: grok\n  model: \"m\"\n  ambient_context: workspace\nworkflow w:\n  entry: a\n  a -> done\n",
			want: 1,
		},
		{
			name: "explicit value, backend inherited from default_backend",
			src:  "agent a:\n  model: \"m\"\n  ambient_context: all\nworkflow w:\n  entry: a\n  default_backend: grok\n  a -> done\n",
			want: 1,
		},
		{
			name: "explicit value on an enforcing backend",
			src:  "agent a:\n  backend: claude_code\n  model: \"m\"\n  ambient_context: none\nworkflow w:\n  entry: a\n  a -> done\n",
			want: 0,
		},
		{
			name: "workspace on kimi is applied",
			src:  "agent a:\n  backend: kimi\n  model: \"m\"\n  ambient_context: workspace\nworkflow w:\n  entry: a\n  a -> done\n",
			want: 0,
		},
		{
			name: "all on opencode is its native loading",
			src:  "agent a:\n  backend: opencode\n  model: \"m\"\n  ambient_context: all\nworkflow w:\n  entry: a\n  a -> done\n",
			want: 0,
		},
		{
			name: "a refused value is an error, not this warning",
			src:  "agent a:\n  backend: opencode\n  model: \"m\"\n  ambient_context: none\nworkflow w:\n  entry: a\n  a -> done\n",
			want: 0,
		},
		{
			name: "no value: the default is documented, not warned",
			src:  "agent a:\n  backend: grok\n  model: \"m\"\nworkflow w:\n  entry: a\n  a -> done\n",
			want: 0,
		},
		{
			name: "unresolved backend: the compiler cannot know",
			src:  "agent a:\n  model: \"m\"\n  ambient_context: workspace\nworkflow w:\n  entry: a\n  a -> done\n",
			want: 0,
		},
		{
			name: "a fallback route to an unmeasured backend",
			src:  "agent a:\n  backend: claude_code\n  model: \"m\"\n  ambient_context: workspace\n  fallbacks:\n    backup:\n      backend: grok\n      model: \"n\"\nworkflow w:\n  entry: a\n  a -> done\n",
			want: 1,
		},
		{
			name: "a skip route runs no backend",
			src:  "agent a:\n  backend: claude_code\n  model: \"m\"\n  ambient_context: workspace\n  fallbacks:\n    give_up:\n      action: skip\nworkflow w:\n  entry: a\n  a -> done\n",
			want: 0,
		},
		{
			name: "workflow value no inheriting node can honour",
			src:  "agent a:\n  backend: grok\n  model: \"m\"\nworkflow w:\n  entry: a\n  ambient_context: workspace\n  a -> done\n",
			want: 1,
		},
		{
			name: "workflow value one inheriting node honours",
			src:  "agent a:\n  backend: grok\n  model: \"m\"\nagent b:\n  backend: claw\n  model: \"m\"\nworkflow w:\n  entry: a\n  ambient_context: all\n  a -> b\n  b -> done\n",
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

// TestAmbientContextRefusedIsAnError: a declared value a backend loads past
// is refused at dispatch, so the compiler refuses it first wherever it can
// see the backend — the node's own value, a workflow value the node
// inherits, and every fallback route.
func TestAmbientContextRefusedIsAnError(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want int
		says string
	}{
		{
			name: "none on kimi",
			src:  "agent a:\n  backend: kimi\n  model: \"m\"\n  ambient_context: none\nworkflow w:\n  entry: a\n  a -> done\n",
			want: 1, says: "kimi reads every AGENTS.md",
		},
		{
			name: "operator on kimi",
			src:  "agent a:\n  backend: kimi\n  model: \"m\"\n  ambient_context: operator\nworkflow w:\n  entry: a\n  a -> done\n",
			want: 1, says: "kimi reads every AGENTS.md",
		},
		{
			name: "workspace on opencode",
			src:  "judge a:\n  backend: opencode\n  model: \"m\"\n  ambient_context: workspace\nworkflow w:\n  entry: a\n  a -> done\n",
			want: 1, says: "global AGENTS.md",
		},
		{
			name: "none on grok",
			src:  "agent a:\n  backend: grok\n  model: \"m\"\n  ambient_context: none\nworkflow w:\n  entry: a\n  a -> done\n",
			want: 1, says: "trusts a repository",
		},
		{
			name: "backend inherited from default_backend",
			src:  "agent a:\n  model: \"m\"\n  ambient_context: none\nworkflow w:\n  entry: a\n  default_backend: opencode\n  a -> done\n",
			want: 1, says: "reads a file in",
		},
		{
			name: "a workflow value the node inherits",
			src:  "agent a:\n  backend: kimi\n  model: \"m\"\nagent b:\n  backend: claw\n  model: \"m\"\nworkflow w:\n  entry: a\n  ambient_context: none\n  a -> b\n  b -> done\n",
			want: 1, says: "inherits ambient_context: none from workflow",
		},
		{
			name: "the node's own value overrides the workflow's",
			src:  "agent a:\n  backend: kimi\n  model: \"m\"\n  ambient_context: all\nworkflow w:\n  entry: a\n  ambient_context: none\n  a -> done\n",
			want: 0,
		},
		{
			name: "a fallback route that cannot honour the value",
			src:  "agent a:\n  backend: claude_code\n  model: \"m\"\n  ambient_context: none\n  fallbacks:\n    backup:\n      backend: kimi\n      model: \"n\"\nworkflow w:\n  entry: a\n  a -> done\n",
			want: 1, says: "fallback route \"backup\"",
		},
		{
			name: "a fallback route dialled to a backend that cannot honour the value",
			src:  "agent a:\n  backend: claude_code\n  model: \"m\"\n  ambient_context: none\n  fallbacks:\n    backup:\n      backend: \"${AMBIENT_ROUTE_BACKEND:-kimi}\"\n      model: \"n\"\nworkflow w:\n  entry: a\n  a -> done\n",
			want: 1, says: "backend=\"kimi\"",
		},
		{
			name: "a fallback route that honours the value",
			src:  "agent a:\n  backend: claude_code\n  model: \"m\"\n  ambient_context: all\n  fallbacks:\n    backup:\n      backend: opencode\n      model: \"n\"\nworkflow w:\n  entry: a\n  a -> done\n",
			want: 0,
		},
		{
			name: "a skip route runs no backend",
			src:  "agent a:\n  backend: claude_code\n  model: \"m\"\n  ambient_context: none\n  fallbacks:\n    give_up:\n      action: skip\nworkflow w:\n  entry: a\n  a -> done\n",
			want: 0,
		},
		{
			name: "no value: each backend applies the default as far as it can",
			src:  "agent a:\n  backend: opencode\n  model: \"m\"\nworkflow w:\n  entry: a\n  a -> done\n",
			want: 0,
		},
		{
			name: "unresolved backend: the compiler cannot know",
			src:  "agent a:\n  model: \"m\"\n  ambient_context: none\nworkflow w:\n  entry: a\n  a -> done\n",
			want: 0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := compileFile(t, c.src)
			if got := countCode(r, DiagAmbientContextRefused); got != c.want {
				t.Fatalf("C187 count = %d, want %d: %+v", got, c.want, r.Diagnostics)
			}
			for _, d := range r.Diagnostics {
				if d.Code != DiagAmbientContextRefused {
					continue
				}
				if d.Severity != SeverityError {
					t.Fatalf("C187 is a %v, want an error", d.Severity)
				}
				if !strings.Contains(d.Message, "ambient_context") || !strings.Contains(d.Message, c.says) {
					t.Fatalf("C187 %q does not name the field and say %q", d.Message, c.says)
				}
			}
		})
	}
}

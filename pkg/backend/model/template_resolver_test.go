package model

import (
	"strings"
	"testing"
)

// The shared resolver renders what it can, keeps the rest as written, and
// names to a listener each reference it kept — the executor listens to
// none, a dry run to all.
func TestTemplateResolverNamesWhatItKeeps(t *testing.T) {
	var kept []string
	r := &TemplateResolver{Vars: map[string]any{"x": "1"}, Unresolved: func(ref string) { kept = append(kept, ref) }}
	td := &TemplateData{Outputs: map[string]map[string]any{"n": {"f": "out"}}, RunID: "r1"}
	got := r.Resolve("a {{vars.x}} {{vars.nope}} {{input.f}} {{outputs.n.f}} {{outputs.n.g}} {{run.id}} {{loop.l.iteration}}", map[string]any{"f": 2}, td)
	if want := "a 1 {{vars.nope}} 2 out {{outputs.n.g}} r1 0"; got != want {
		t.Fatalf("rendered %q, want %q", got, want)
	}
	if strings.Join(kept, ",") != "vars.nope,outputs.n.g" {
		t.Fatalf("kept %v", kept)
	}
	// Without a listener the same text renders the same, silently.
	silent := &TemplateResolver{Vars: map[string]any{"x": "1"}}
	if again := silent.Resolve("a {{vars.x}} {{vars.nope}} {{input.f}} {{outputs.n.f}} {{outputs.n.g}} {{run.id}} {{loop.l.iteration}}", map[string]any{"f": 2}, td); again != got {
		t.Fatalf("a listener changed the rendering: %q vs %q", again, got)
	}
	// Cross-namespace references need the run's data.
	if out := silent.Resolve("{{outputs.n.f}}", nil, nil); out != "{{outputs.n.f}}" {
		t.Fatalf("an outputs reference resolved without template data: %q", out)
	}
}

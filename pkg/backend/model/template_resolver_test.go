package model

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// The shared resolver renders what it can, keeps the rest as written, and
// names to a listener each reference it kept — the executor listens to
// none, a dry run to all.
func TestTemplateResolverNamesWhatItKeeps(t *testing.T) {
	var kept []string
	r := &TemplateResolver{Vars: map[string]any{"x": "1"}, Unresolved: func(ref string) { kept = append(kept, ref) }}
	td := &TemplateData{Outputs: map[string]map[string]any{"n": {"f": "out"}}, RunID: "r1", LoopMaxIterations: map[string]int{"l": 3}}
	got := r.Resolve("a {{vars.x}} {{vars.nope}} {{input.f}} {{outputs.n.f}} {{outputs.n.g}} {{run.id}} {{loop.l.iteration}} {{loop.nope.iteration}}", map[string]any{"f": 2}, td)
	if want := "a 1 {{vars.nope}} 2 out {{outputs.n.g}} r1 0 {{loop.nope.iteration}}"; got != want {
		t.Fatalf("rendered %q, want %q", got, want)
	}
	if strings.Join(kept, ",") != "vars.nope,outputs.n.g,loop.nope.iteration" {
		t.Fatalf("kept %v", kept)
	}
	// Without a listener the same text renders the same, silently.
	silent := &TemplateResolver{Vars: map[string]any{"x": "1"}}
	if again := silent.Resolve("a {{vars.x}} {{vars.nope}} {{input.f}} {{outputs.n.f}} {{outputs.n.g}} {{run.id}} {{loop.l.iteration}} {{loop.nope.iteration}}", map[string]any{"f": 2}, td); again != got {
		t.Fatalf("a listener changed the rendering: %q vs %q", again, got)
	}
	// Cross-namespace references need the run's data.
	if out := silent.Resolve("{{outputs.n.f}}", nil, nil); out != "{{outputs.n.f}}" {
		t.Fatalf("an outputs reference resolved without template data: %q", out)
	}
}

// A command resolves every namespace a prompt does — an attachment's path
// and fields, a loop counter, an artifact field — and keeps as written
// what resolves to nothing, so the shell sees it.
func TestCommandsResolveEveryNamespaceAPromptDoes(t *testing.T) {
	td := &TemplateData{
		Attachments:  map[string]AttachmentInfo{"spec": {Name: "spec", Path: "/tmp/spec.md", MIME: "text/markdown"}},
		LoopCounters: map[string]int{"fix": 2},
		Artifacts:    map[string]map[string]any{"brief": {"url": "https://x"}},
	}
	command := "cat {{attachments.spec}} {{attachments.spec.mime}} {{loop.fix.iteration}} {{artifacts.brief.url}} {{attachments.nope}}"
	refs, err := ir.ParseRefs(command)
	if err != nil {
		t.Fatal(err)
	}
	got := RenderCommand(command, refs, nil, nil, td, "r1", nil)
	for _, want := range []string{"/tmp/spec.md", "text/markdown", "2", "https://x", "{{attachments.nope}}"} {
		if !strings.Contains(got, want) {
			t.Errorf("the command lacks %q: %s", want, got)
		}
	}
	if strings.Contains(got, "{{attachments.spec") || strings.Contains(got, "{{loop") || strings.Contains(got, "{{artifacts") {
		t.Fatalf("a namespace a prompt resolves stayed literal in the command: %s", got)
	}
}

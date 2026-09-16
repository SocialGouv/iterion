package model

import (
	"strings"
	"testing"
)

// A tool body gets the VALUE of an artifact, an attachment field or a loop
// counter — a number as a number, an object as an object — through the
// body's own renderer, like every other namespace; a prompt shows the text
// of it, as before.
func TestToolBodiesGetTheValueNotThePromptsText(t *testing.T) {
	td := &TemplateData{
		LoopCounters:      map[string]int{"r": 3},
		LoopMaxIterations: map[string]int{"r": 10},
		Artifacts:         map[string]map[string]any{"report": {"score": 3}},
		Attachments:       map[string]AttachmentInfo{"d": {Path: "/p/d", Size: 1024}},
	}
	body := "n = {{loop.r.iteration}}; m = {{loop.r.max}}; a = {{artifacts.report}}; s = {{attachments.d.size}}; p = {{attachments.d}}; sc = {{artifacts.report.score}}"
	got := RenderScript(body, mustRefs(body), nil, nil, td, "run-1", nil)
	if want := `n = 3; m = 10; a = {"score":3}; s = 1024; p = "/p/d"; sc = 3`; got != want {
		t.Fatalf("script rendered\n%s\nwant\n%s", got, want)
	}
	cmd := RenderCommand(body, mustRefs(body), nil, nil, td, "run-1", nil)
	for _, want := range []string{"n = '3'", "a = '{\"score\":3}'", "s = '1024'", "p = '/p/d'"} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("command rendered %q, lacks %q", cmd, want)
		}
	}
	prompt := (&TemplateResolver{}).Resolve(body, nil, td)
	if want := `n = 3; m = 10; a = {"score":3}; s = 1024; p = /p/d; sc = 3`; prompt != want {
		t.Fatalf("prompt rendered\n%s\nwant\n%s", prompt, want)
	}
	if v, ok := (&TemplateResolver{}).ResolveValue("loop.r.iteration", nil, td); !ok || v != int64(3) {
		t.Fatalf("ResolveValue(loop.r.iteration) = %#v %v", v, ok)
	}
}

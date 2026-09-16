package model

import (
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// A bang reference with a space after the bang — `{{! attachments.spec}}`
// — resolves like the tight one: the renderer reads the parsed reference,
// not its raw text.
func TestABangReferenceWithASpaceResolvesLikeATightOne(t *testing.T) {
	td := &TemplateData{Attachments: map[string]AttachmentInfo{"spec": {Path: "/p/a"}}}
	for _, body := range []string{"p = {{!attachments.spec}}", "p = {{! attachments.spec}}", "p = {{ ! attachments.spec }}"} {
		refs, err := ir.ParseRefs(body)
		if err != nil {
			t.Fatal(err)
		}
		var left []string
		got := RenderScript(body, refs, nil, nil, td, "run-1", func(ref string) { left = append(left, ref) })
		if got != "p = /p/a" || len(left) != 0 {
			t.Fatalf("%q rendered %q, unresolved %v", body, got, left)
		}
	}
}

// What the renderer leaves is said once, as `namespace.path`, for a shell
// body (the hole kept as written) and a script body (the hole a null) alike
// — and a value that carries braces is not re-read as a reference.
func TestTheRendererNamesWhatItLeaves(t *testing.T) {
	td := &TemplateData{Outputs: map[string]map[string]any{"survey": {"note": "fill {{vars.goal}} in"}}}
	body := "echo {{outputs.survey.note}} {{input.missing}} {{ ! vars.absent }} {{each.items.item}}"
	refs, err := ir.ParseRefs(body)
	if err != nil {
		t.Fatal(err)
	}
	var left []string
	got := RenderCommand(body, refs, map[string]any{}, map[string]any{}, td, "run-1", func(ref string) { left = append(left, ref) })
	if got != "echo 'fill {{vars.goal}} in' {{input.missing}} {{ ! vars.absent }} {{each.items.item}}" {
		t.Fatalf("command rendered %q", got)
	}
	// Two holes and a namespace a tool body does not carry: three names.
	if strings.Join(left, " ") != "input.missing vars.absent each.items.item" {
		t.Fatalf("the renderer named %v, want the holes as namespace.path", left)
	}
	left = nil
	got = RenderScript("x = {{input.missing}}", mustRefs("x = {{input.missing}}"), map[string]any{}, nil, td, "run-1", func(ref string) { left = append(left, ref) })
	if got != "x = null" || strings.Join(left, " ") != "input.missing" {
		t.Fatalf("script rendered %q, named %v", got, left)
	}
}

// An attachment URL that cannot be signed is not found — no signer wired,
// or the signer failing, said to Warn — never an empty value.
func TestAnUnsignedAttachmentURLIsNotFound(t *testing.T) {
	var warned []string
	r := &TemplateResolver{Warn: func(format string, args ...any) { warned = append(warned, format) }}
	unsigned := &TemplateData{Attachments: map[string]AttachmentInfo{"spec": {Path: "/p/a"}}}
	if v, ok := r.ResolveRef("attachments.spec.url", nil, unsigned); ok || v != "" {
		t.Fatalf("no signer wired and the url resolved to %q", v)
	}
	failing := &TemplateData{Attachments: map[string]AttachmentInfo{"spec": {Path: "/p/a", PresignURL: func() (string, error) {
		return "", errors.New("signing key revoked")
	}}}}
	if v, ok := r.ResolveRef("attachments.spec.url", nil, failing); ok || v != "" {
		t.Fatalf("the signer failed and the url resolved to %q", v)
	}
	if len(warned) != 1 {
		t.Fatalf("the failure was not said to Warn exactly once: %v", warned)
	}
	signed := &TemplateData{Attachments: map[string]AttachmentInfo{"spec": {Path: "/p/a", PresignURL: func() (string, error) {
		return "https://x/spec?sig=1", nil
	}}}}
	if v, ok := r.ResolveRef("attachments.spec.url", nil, signed); !ok || v != "https://x/spec?sig=1" {
		t.Fatalf("a signed url did not resolve: %q %v", v, ok)
	}
	body := "curl -o out {{attachments.spec.url}}"
	var left []string
	got := RenderCommand(body, mustRefs(body), nil, nil, failing, "run-1", func(ref string) { left = append(left, ref) })
	if got != body || strings.Join(left, " ") != "attachments.spec.url" {
		t.Fatalf("a command with an unsignable url rendered %q, named %v", got, left)
	}
}

// A loop no edge declares resolves to nothing — not to a 0 that a guard
// reads as "never": the reference stays as written in a command, is null
// in a script, and a declared loop resolves before and after its first
// crossing.
func TestAnUndeclaredLoopResolvesToNothing(t *testing.T) {
	td := &TemplateData{LoopMaxIterations: map[string]int{"retry": 3}}
	r := &TemplateResolver{}
	if v, ok := r.ResolveValue("loop.retry.iteration", nil, td); !ok || v != int64(0) {
		t.Fatalf("a declared loop before its first crossing: %#v %v", v, ok)
	}
	if _, ok := r.ResolveValue("loop.TYPO.iteration", nil, td); ok {
		t.Fatal("an undeclared loop resolved")
	}
	crossed := &TemplateData{LoopCounters: map[string]int{"fix": 2}}
	if v, ok := r.ResolveValue("loop.fix.iteration", nil, crossed); !ok || v != int64(2) {
		t.Fatalf("a crossed loop known by its counter alone: %#v %v", v, ok)
	}
	body := "if [ {{loop.TYPO.iteration}} -ge {{loop.TYPO.max}} ]; then exit 1; fi"
	var left []string
	got := RenderCommand(body, mustRefs(body), nil, nil, td, "run-1", func(ref string) { left = append(left, ref) })
	if got != body || len(left) != 2 {
		t.Fatalf("an undeclared loop in a command rendered %q, named %v", got, left)
	}
}

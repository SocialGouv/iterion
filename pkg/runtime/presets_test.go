package runtime

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// focusExec is a minimal NodeExecutor that records SetPresetFocus calls.
type focusExec struct {
	prompt string
	skills []string
}

func (f *focusExec) Execute(context.Context, ir.Node, map[string]any) (map[string]any, error) {
	return nil, nil
}
func (f *focusExec) SetPresetFocus(p string, s []string) { f.prompt, f.skills = p, s }

func TestApplyPresetFocus_PushesToExecutor(t *testing.T) {
	wf := &ir.Workflow{Presets: map[string]ir.Preset{
		"sre": {Name: "sre", Prompt: "Be an SRE.", Skills: []string{"fallow"}},
	}}
	fe := &focusExec{}
	New(wf, tmpStore(t), fe, WithPreset("sre")).applyPresetFocus()
	if fe.prompt != "Be an SRE." {
		t.Errorf("prompt: %q", fe.prompt)
	}
	if len(fe.skills) != 1 || fe.skills[0] != "fallow" {
		t.Errorf("skills: %v", fe.skills)
	}
}

func TestApplyPresetFocus_NoPresetNoPush(t *testing.T) {
	wf := &ir.Workflow{Presets: map[string]ir.Preset{"sre": {Name: "sre", Prompt: "x"}}}
	fe := &focusExec{prompt: "SENTINEL"}
	New(wf, tmpStore(t), fe).applyPresetFocus() // no WithPreset
	if fe.prompt != "SENTINEL" {
		t.Errorf("expected no push, prompt became %q", fe.prompt)
	}
}

func TestApplyPresetFocus_VarOnlyPresetNoPush(t *testing.T) {
	wf := &ir.Workflow{Presets: map[string]ir.Preset{"q": {Name: "q", Values: map[string]any{"x": 1}}}}
	fe := &focusExec{prompt: "SENTINEL"}
	New(wf, tmpStore(t), fe, WithPreset("q")).applyPresetFocus()
	if fe.prompt != "SENTINEL" {
		t.Errorf("var-only preset must not push focus; got %q", fe.prompt)
	}
}

// TestMergeBundlePresets verifies that a bundle's presets/<name>.md files are
// folded into the workflow's preset set, carrying the prompt + skills the
// in-source presets: block can't express, and that a file preset OVERWRITES
// an in-source preset of the same name (the explicit, richer artifact wins).
func TestMergeBundlePresets(t *testing.T) {
	dir := t.TempDir()
	// assembleBundle only stats main.bot; it never parses it, so any content
	// is fine for exercising the presets/ discovery + merge.
	if err := os.WriteFile(filepath.Join(dir, "main.bot"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	pdir := filepath.Join(dir, "presets")
	if err := os.MkdirAll(pdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pdir, "sre.md"), []byte("---\nname: sre\nskills: [fallow]\nvars:\n  focus: \"reliability\"\n---\nBe an SRE."), 0o644); err != nil {
		t.Fatal(err)
	}

	b, err := bundle.OpenDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if b.PresetsDir == "" {
		t.Fatal("PresetsDir not set by OpenDir")
	}

	wf := &ir.Workflow{Presets: map[string]ir.Preset{
		// An in-source, var-only preset of the same name — must be overwritten.
		"sre": {Name: "sre", Values: map[string]any{"focus": "STALE"}},
	}}
	MergeBundlePresets(wf, b, nil)

	p, ok := wf.Presets["sre"]
	if !ok {
		t.Fatal("sre preset missing after merge")
	}
	if p.Prompt != "Be an SRE." {
		t.Errorf("prompt: %q (file preset should win)", p.Prompt)
	}
	if got := p.Values["focus"]; got != "reliability" {
		t.Errorf("values.focus: %v (file should win over in-source STALE)", got)
	}
	if len(p.Skills) != 1 || p.Skills[0] != "fallow" {
		t.Errorf("skills: %v", p.Skills)
	}
}

// TestMergeBundlePresets_WillyRealFiles parses the shipped Willy bundle's
// presets/ so a malformed authored file fails CI rather than only at run time.
func TestMergeBundlePresets_WillyRealFiles(t *testing.T) {
	dir := filepath.Join("..", "..", "bots", "whole-improve-loop")
	if _, err := os.Stat(filepath.Join(dir, "presets")); err != nil {
		t.Skipf("Willy bundle presets/ not reachable from cwd: %v", err)
	}
	b, err := bundle.OpenDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	wf := &ir.Workflow{}
	MergeBundlePresets(wf, b, nil)
	for _, name := range []string{"improve-quality", "production-ready", "code-quality", "rgaa", "rgpd"} {
		p, ok := wf.Presets[name]
		if !ok {
			t.Errorf("missing preset %q", name)
			continue
		}
		if p.Prompt == "" {
			t.Errorf("preset %q has empty prompt body", name)
		}
		if p.DisplayName == "" {
			t.Errorf("preset %q has empty display_name", name)
		}
	}
	iq := wf.Presets["improve-quality"]
	if len(iq.Skills) == 0 || iq.Skills[0] != "lang-js-fallow" {
		t.Errorf("improve-quality should reference lang-js-fallow, got %v", iq.Skills)
	}
	if _, ok := iq.Values["improvement_prompt"]; !ok {
		t.Errorf("improve-quality should override improvement_prompt")
	}
}

func TestMergeBundlePresets_NilSafe(t *testing.T) {
	// None of these should panic.
	MergeBundlePresets(nil, nil, nil)
	wf := &ir.Workflow{}
	MergeBundlePresets(wf, nil, nil)
	MergeBundlePresets(wf, &bundle.Bundle{}, nil) // no PresetsDir
	if len(wf.Presets) != 0 {
		t.Fatalf("expected no presets, got %d", len(wf.Presets))
	}
}

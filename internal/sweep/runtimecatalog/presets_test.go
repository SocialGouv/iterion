package runtimecatalog

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/internal/sweepwalk"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runtime"
)

// TestMergeBundlePresets_WillyRealFiles parses the shipped Willy bundle's
// presets/ so a malformed authored file fails CI rather than only at run time.
func TestMergeBundlePresets_WillyRealFiles(t *testing.T) {
	dir := filepath.Join(sweepwalk.ModuleRoot(t), "bots", "whole-improve-loop")
	if _, err := os.Stat(filepath.Join(dir, "presets")); err != nil {
		t.Fatalf("the Willy bundle presets/ is unreadable at the module root: %v", err)
	}
	b, err := bundle.OpenDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	wf := &ir.Workflow{}
	runtime.MergeBundlePresets(wf, b, "", nil)
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

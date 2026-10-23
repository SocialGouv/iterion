package botregistry

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func boolPtr(b bool) *bool { return &b }

func TestLoadOverrides_MissingIsEmpty(t *testing.T) {
	ov, err := LoadOverrides(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOverrides: %v", err)
	}
	if ov == nil || len(ov.Bots) != 0 {
		t.Errorf("expected empty overrides, got %+v", ov)
	}
}

func TestResolveEnabled_OverlayWins(t *testing.T) {
	dir := t.TempDir()
	if err := SetOverlayEnabled(context.Background(), dir, "feature_dev", boolPtr(false)); err != nil {
		t.Fatalf("SetOverlayEnabled: %v", err)
	}
	ov, err := LoadOverrides(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Manifest says enabled; overlay disables → resolved false.
	if ResolveEnabled("feature_dev", true, ov) {
		t.Error("overlay disable should win over manifest enabled")
	}
	// Normalised lookup: kebab form matches the snake-cased override.
	if ResolveEnabled("feature-dev", true, ov) {
		t.Error("normalized lookup should match overlay entry")
	}
}

func TestResolveEnabled_OverlayCanReEnable(t *testing.T) {
	dir := t.TempDir()
	if err := SetOverlayEnabled(context.Background(), dir, "willy", boolPtr(true)); err != nil {
		t.Fatal(err)
	}
	ov, _ := LoadOverrides(dir)
	// Manifest disabled; overlay re-enables → resolved true.
	if !ResolveEnabled("willy", false, ov) {
		t.Error("overlay enable should win over manifest disabled")
	}
}

func TestSetOverlayEnabled_NilClearsOverride(t *testing.T) {
	dir := t.TempDir()
	if err := SetOverlayEnabled(context.Background(), dir, "doki", boolPtr(false)); err != nil {
		t.Fatal(err)
	}
	if err := SetOverlayEnabled(context.Background(), dir, "doki", nil); err != nil {
		t.Fatal(err)
	}
	ov, _ := LoadOverrides(dir)
	if _, ok := ov.lookup("doki"); ok {
		t.Error("nil should clear the override entry")
	}
	// With no override, the manifest default stands.
	if ResolveEnabled("doki", true, ov) != true {
		t.Error("cleared override should fall back to manifest default")
	}
}

func TestSetOverlayEnabled_Persists(t *testing.T) {
	dir := t.TempDir()
	if err := SetOverlayEnabled(context.Background(), dir, "seki", boolPtr(false)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, overridesRelPath)); err != nil {
		t.Errorf("overlay file not written: %v", err)
	}
	// Round-trips across a fresh load.
	ov, err := LoadOverrides(dir)
	if err != nil {
		t.Fatal(err)
	}
	if ResolveEnabled("seki", true, ov) {
		t.Error("persisted disable not honoured after reload")
	}
}

func TestResolveEnabled_NilOverridesSafe(t *testing.T) {
	if !ResolveEnabled("anything", true, nil) {
		t.Error("nil overrides should fall back to manifest default")
	}
}

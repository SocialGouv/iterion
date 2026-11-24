package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildSystemContextHonorsToggles(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAW_MEMORY_DIR", "")
	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, "CLAUDE.md"), []byte("Use tabs."), 0o644); err != nil {
		t.Fatal(err)
	}

	full := BuildSystemContext(workDir, DefaultPromptConfig())
	for _, want := range []string{"# Environment", "# Project Instructions (CLAUDE.md)", "Use tabs.", "# Auto memory"} {
		if !strings.Contains(full, want) {
			t.Errorf("default config: missing %q:\n%s", want, full)
		}
	}
	if strings.Contains(full, "# Operating posture") {
		t.Errorf("BuildSystemContext must not include the posture:\n%s", full)
	}

	onlyMemory := MinimalPromptConfig()
	onlyMemory.ProjectInstructions = true
	got := BuildSystemContext(workDir, onlyMemory)
	if !strings.Contains(got, "Use tabs.") || strings.Contains(got, "# Environment") {
		t.Errorf("selective config wrong:\n%s", got)
	}
}

func TestBuildSystemContextMinimalIsEmpty(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAW_MEMORY_DIR", "")
	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, "CLAUDE.md"), []byte("memo"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := BuildSystemContext(workDir, MinimalPromptConfig()); got != "" {
		t.Errorf("minimal config must render empty context, got:\n%s", got)
	}
}

func TestResolvePromptSections(t *testing.T) {
	cfg, err := ResolvePromptSections(false, []string{"project-instructions"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.ProjectInstructions || cfg.Environment || cfg.Posture {
		t.Errorf("only-list resolution wrong: %+v", cfg)
	}
	if _, err := ResolvePromptSections(false, []string{"nope"}, nil); err == nil {
		t.Error("expected error for unknown section")
	}
}

func TestOperatingPostureStable(t *testing.T) {
	p := OperatingPosture()
	if !strings.HasPrefix(p, "# Operating posture") {
		t.Errorf("posture header changed: %q", p[:40])
	}
	if len(p) < 200 {
		t.Errorf("posture suspiciously short: %d bytes", len(p))
	}
}

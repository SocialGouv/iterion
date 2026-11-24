package context

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newMemoryWorkDir creates a workdir with a CLAUDE.md and points HOME at an
// empty temp dir so the user-global CLAUDE.md can't leak into assertions.
func newMemoryWorkDir(t *testing.T) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, "CLAUDE.md"), []byte("Use tabs."), 0o644); err != nil {
		t.Fatal(err)
	}
	return workDir
}

func TestAssembleDefaultsEmitAllSections(t *testing.T) {
	workDir := newMemoryWorkDir(t)
	out := NewAssembler(workDir).Assemble()
	if !strings.Contains(out, "# Environment") {
		t.Errorf("missing # Environment:\n%s", out)
	}
	if !strings.Contains(out, "# Project Instructions (CLAUDE.md)") {
		t.Errorf("missing # Project Instructions:\n%s", out)
	}
	if !strings.Contains(out, "Use tabs.") {
		t.Errorf("missing CLAUDE.md content:\n%s", out)
	}
}

func TestAssembleOptionsGateEachSection(t *testing.T) {
	workDir := newMemoryWorkDir(t)

	cases := []struct {
		name   string
		opts   AssembleOptions
		absent string
	}{
		{"environment off", func() AssembleOptions {
			o := DefaultAssembleOptions()
			o.Environment = false
			return o
		}(), "# Environment"},
		{"git status off", func() AssembleOptions {
			o := DefaultAssembleOptions()
			o.GitStatus = false
			return o
		}(), "# Git Status"},
		{"project instructions off", func() AssembleOptions {
			o := DefaultAssembleOptions()
			o.ProjectInstructions = false
			return o
		}(), "# Project Instructions"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := NewAssemblerWithOptions(workDir, tc.opts).Assemble()
			if strings.Contains(out, tc.absent) {
				t.Errorf("section %q should be gated off:\n%s", tc.absent, out)
			}
		})
	}
}

func TestAssembleAllOff(t *testing.T) {
	workDir := newMemoryWorkDir(t)
	out := NewAssemblerWithOptions(workDir, AssembleOptions{}).Assemble()
	if out != "" {
		t.Errorf("all-off options should produce empty context, got:\n%s", out)
	}
}

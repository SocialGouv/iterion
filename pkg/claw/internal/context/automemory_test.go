package context

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAutoMemoryDirDefaultAndOverride(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAW_MEMORY_DIR", "")
	workDir := "/some/workspace"

	want := filepath.Join(home, ".claw-code", "memory", WorkspaceFingerprint(workDir))
	if got := AutoMemoryDir(workDir); got != want {
		t.Errorf("AutoMemoryDir = %q, want %q", got, want)
	}

	override := t.TempDir()
	t.Setenv("CLAW_MEMORY_DIR", override)
	want = filepath.Join(override, WorkspaceFingerprint(workDir))
	if got := AutoMemoryDir(workDir); got != want {
		t.Errorf("AutoMemoryDir with override = %q, want %q", got, want)
	}
}

func TestLoadAutoMemorySectionEmpty(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAW_MEMORY_DIR", "")
	got := LoadAutoMemorySection(t.TempDir())
	if !strings.Contains(got, "persistent memory directory") {
		t.Errorf("instructions missing when MEMORY.md absent:\n%s", got)
	}
	if !strings.Contains(got, "(empty — no memory recorded yet)") {
		t.Errorf("empty placeholder missing:\n%s", got)
	}
}

func TestLoadAutoMemorySectionContent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	memRoot := t.TempDir()
	t.Setenv("CLAW_MEMORY_DIR", memRoot)
	workDir := t.TempDir()

	dir := filepath.Join(memRoot, WorkspaceFingerprint(workDir))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte("- build: go build ./..."), 0o644); err != nil {
		t.Fatal(err)
	}

	got := LoadAutoMemorySection(workDir)
	if !strings.Contains(got, "- build: go build ./...") {
		t.Errorf("MEMORY.md content missing:\n%s", got)
	}
	if strings.Contains(got, "(empty") {
		t.Errorf("empty placeholder shown despite content:\n%s", got)
	}
}

func TestLoadAutoMemorySectionTruncates(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	memRoot := t.TempDir()
	t.Setenv("CLAW_MEMORY_DIR", memRoot)
	workDir := t.TempDir()

	dir := filepath.Join(memRoot, WorkspaceFingerprint(workDir))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	big := strings.Repeat("m", autoMemoryMaxBytes+500)
	if err := os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}

	got := LoadAutoMemorySection(workDir)
	if !strings.Contains(got, "... (truncated") {
		t.Errorf("expected truncation notice for oversized MEMORY.md")
	}
	if len(got) > autoMemoryMaxBytes+1024 {
		t.Errorf("section not capped: %d bytes", len(got))
	}
}

func TestAssembleAutoMemoryToggle(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAW_MEMORY_DIR", "")
	workDir := t.TempDir()

	out := NewAssembler(workDir).Assemble()
	if !strings.Contains(out, "# Auto memory") {
		t.Errorf("default options must emit # Auto memory:\n%s", out)
	}

	opts := DefaultAssembleOptions()
	opts.AutoMemory = false
	out = NewAssemblerWithOptions(workDir, opts).Assemble()
	if strings.Contains(out, "# Auto memory") {
		t.Errorf("# Auto memory leaked with toggle off:\n%s", out)
	}
}

func TestWorkspaceFingerprintStable(t *testing.T) {
	// FNV-1a golden: fingerprints must stay stable across refactors (session
	// store namespacing and memory dirs both depend on it).
	if got := WorkspaceFingerprint(""); got != "cbf29ce484222325" {
		t.Errorf("fingerprint of empty string = %q, want FNV-1a offset basis", got)
	}
	if a, b := WorkspaceFingerprint("/w1"), WorkspaceFingerprint("/w2"); a == b {
		t.Errorf("distinct workspaces collide: %q", a)
	}
}

func TestLoadAutoMemorySectionAt_ExplicitDirWins(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte("- pinned: yes"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := LoadAutoMemorySectionAt(dir)
	if !strings.Contains(got, "- pinned: yes") {
		t.Errorf("explicit dir content missing:\n%s", got)
	}
	if !strings.Contains(got, dir) {
		t.Errorf("explicit dir path missing from instructions:\n%s", got)
	}
}

func TestLoadAutoMemorySectionAt_EmptyDir(t *testing.T) {
	if got := LoadAutoMemorySectionAt(""); got != "" {
		t.Errorf("empty dir must render nothing, got %q", got)
	}
}

// A workDir-derived directory is the wrong default for a host that runs each
// session in a fresh directory: two workDirs fingerprint apart, so the agent
// would start empty every time. The override is what closes that.
func TestAssembler_AutoMemoryDirOverridesWorkDirFingerprint(t *testing.T) {
	memDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(memDir, "MEMORY.md"), []byte("- shared across worktrees"), 0o644); err != nil {
		t.Fatal(err)
	}
	opts := AssembleOptions{AutoMemory: true, AutoMemoryDir: memDir}
	for _, workDir := range []string{t.TempDir(), t.TempDir()} {
		got := NewAssemblerWithOptions(workDir, opts).Assemble()
		if !strings.Contains(got, "- shared across worktrees") {
			t.Errorf("workDir %s did not see the pinned memory:\n%s", workDir, got)
		}
	}
}

package context

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadMemoryWalkUp(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	workDir := filepath.Join(root, "a", "b")
	writeFile(t, filepath.Join(root, "CLAUDE.md"), "root instructions")
	writeFile(t, filepath.Join(root, "a", "CLAUDE.md"), "mid instructions")
	writeFile(t, filepath.Join(workDir, "CLAUDE.md"), "leaf instructions")

	got, _ := LoadMemory(workDir, MemoryOptions{WalkUp: true})
	for _, want := range []string{"root instructions", "mid instructions", "leaf instructions"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	// Order: root-most first, leaf (project root) last.
	if strings.Index(got, "root instructions") > strings.Index(got, "mid instructions") ||
		strings.Index(got, "mid instructions") > strings.Index(got, "leaf instructions") {
		t.Errorf("wrong order (want root → mid → leaf):\n%s", got)
	}

	// WalkUp off: ancestors excluded, project file kept.
	got, _ = LoadMemory(workDir, MemoryOptions{})
	if strings.Contains(got, "root instructions") || strings.Contains(got, "mid instructions") {
		t.Errorf("ancestors leaked with WalkUp off:\n%s", got)
	}
	if !strings.Contains(got, "leaf instructions") {
		t.Errorf("project file missing with WalkUp off:\n%s", got)
	}
}

func TestLoadMemoryDedupsHomeInAncestry(t *testing.T) {
	// HOME placed on the ancestor chain: ~/.claude/CLAUDE.md is a distinct
	// path, but ~/CLAUDE.md must not be double-loaded as both ancestor and
	// user-global.
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeFile(t, filepath.Join(home, "CLAUDE.md"), "home-root memo")
	workDir := filepath.Join(home, "proj")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}

	got, _ := LoadMemory(workDir, MemoryOptions{WalkUp: true})
	if n := strings.Count(got, "home-root memo"); n != 1 {
		t.Errorf("home CLAUDE.md loaded %d times, want 1:\n%s", n, got)
	}
}

func TestLoadMemoryImports(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	workDir := t.TempDir()
	writeFile(t, filepath.Join(home, "notes.md"), "home note content")
	writeFile(t, filepath.Join(workDir, "extra.md"), "extra content")
	writeFile(t, filepath.Join(workDir, "CLAUDE.md"),
		"See @./extra.md and @~/notes.md\n\n```\n@./fenced.md\n```\nand `@./inline.md` too")
	writeFile(t, filepath.Join(workDir, "fenced.md"), "fenced content")
	writeFile(t, filepath.Join(workDir, "inline.md"), "inline content")

	got, mtimes := LoadMemory(workDir, MemoryOptions{Imports: true})
	if !strings.Contains(got, "extra content") {
		t.Errorf("relative import missing:\n%s", got)
	}
	if !strings.Contains(got, "home note content") {
		t.Errorf("~/ import missing:\n%s", got)
	}
	if strings.Contains(got, "fenced content") {
		t.Errorf("import inside code fence must be ignored:\n%s", got)
	}
	if strings.Contains(got, "inline content") {
		t.Errorf("import inside inline code must be ignored:\n%s", got)
	}
	if _, ok := mtimes[filepath.Join(workDir, "extra.md")]; !ok {
		t.Errorf("imported file missing from mtime map: %v", mtimes)
	}

	// Imports off.
	got, _ = LoadMemory(workDir, MemoryOptions{})
	if strings.Contains(got, "extra content") {
		t.Errorf("import expanded with Imports off:\n%s", got)
	}
}

func TestLoadMemoryImportDepthAndCycles(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	workDir := t.TempDir()

	// Chain: CLAUDE.md → d1 → d2 → d3 → d4 → d5 → d6 (6 hops; depth 5 cuts d6).
	writeFile(t, filepath.Join(workDir, "CLAUDE.md"), "start @./d1.md")
	for i := 1; i <= 6; i++ {
		content := fmt.Sprintf("depth-%d content", i)
		if i < 6 {
			content += fmt.Sprintf(" @./d%d.md", i+1)
		}
		writeFile(t, filepath.Join(workDir, fmt.Sprintf("d%d.md", i)), content)
	}
	got, _ := LoadMemory(workDir, MemoryOptions{Imports: true})
	if !strings.Contains(got, "depth-5 content") {
		t.Errorf("hop 5 should be included:\n%s", got)
	}
	if strings.Contains(got, "depth-6 content") {
		t.Errorf("hop 6 exceeds max depth 5:\n%s", got)
	}

	// Cycle: A → B → A terminates and loads each once.
	cyc := t.TempDir()
	writeFile(t, filepath.Join(cyc, "CLAUDE.md"), "@./a.md")
	writeFile(t, filepath.Join(cyc, "a.md"), "alpha content @./b.md")
	writeFile(t, filepath.Join(cyc, "b.md"), "beta content @./a.md")
	got, _ = LoadMemory(cyc, MemoryOptions{Imports: true})
	if strings.Count(got, "alpha content") != 1 || strings.Count(got, "beta content") != 1 {
		t.Errorf("cycle not deduplicated:\n%s", got)
	}
}

func TestLoadMemorySizeCap(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	workDir := t.TempDir()
	writeFile(t, filepath.Join(workDir, "CLAUDE.md"), strings.Repeat("x", 500))

	got, _ := LoadMemory(workDir, MemoryOptions{MaxBytes: 100})
	if !strings.Contains(got, "... (truncated)") {
		t.Errorf("expected truncation marker:\n%s", got)
	}
	if len(got) > 200 {
		t.Errorf("content not capped: %d bytes", len(got))
	}
}

func TestLoadMemoryStripsFrontmatter(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	workDir := t.TempDir()
	writeFile(t, filepath.Join(workDir, "CLAUDE.md"), "---\nmodel: some-model\n---\nActual instructions.")

	got, _ := LoadMemory(workDir, MemoryOptions{})
	if strings.Contains(got, "some-model") {
		t.Errorf("frontmatter leaked into injected memory:\n%s", got)
	}
	if !strings.Contains(got, "Actual instructions.") {
		t.Errorf("body missing:\n%s", got)
	}
}

func TestAssemblerMemoryCacheInvalidation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	workDir := filepath.Join(root, "proj")
	writeFile(t, filepath.Join(workDir, "CLAUDE.md"), "v1 @./imp.md")
	writeFile(t, filepath.Join(workDir, "imp.md"), "import v1")

	a := NewAssembler(workDir)
	out := a.Assemble()
	if !strings.Contains(out, "import v1") {
		t.Fatalf("initial import missing:\n%s", out)
	}

	// Editing an imported file invalidates the cache.
	writeFile(t, filepath.Join(workDir, "imp.md"), "import v2")
	bumpMtime(t, filepath.Join(workDir, "imp.md"))
	if out = a.Assemble(); !strings.Contains(out, "import v2") {
		t.Errorf("edited import not picked up:\n%s", out)
	}

	// Creating a new ancestor CLAUDE.md invalidates the cache.
	writeFile(t, filepath.Join(root, "CLAUDE.md"), "new ancestor memo")
	if out = a.Assemble(); !strings.Contains(out, "new ancestor memo") {
		t.Errorf("new ancestor CLAUDE.md not picked up:\n%s", out)
	}
}

// bumpMtime pushes a file's mtime forward so mtime-based caches can't miss a
// same-instant rewrite.
func bumpMtime(t *testing.T, path string) {
	t.Helper()
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
}

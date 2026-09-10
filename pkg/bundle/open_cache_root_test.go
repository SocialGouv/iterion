package bundle

import (
	"path/filepath"
	"strings"
	"testing"
)

// A relative cache root is made absolute by Open itself: the extracted
// bundle's prompts/ sit under it, and a prompt's {{include}} resolves only
// beside an absolutely named file, so a caller's relative spelling of the
// root must not be what a bundle prompt's Span.File inherits.
func TestOpenAbsolutisesARelativeCacheRoot(t *testing.T) {
	path := fixtureBundleWithSkillsPrompts(t)
	cwd := t.TempDir()
	t.Chdir(cwd)
	b, cleanup, err := Open(path, "cache")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer cleanup()
	// A temp dir may be reached through a symlink (darwin's /var → /private/var):
	// compare the resolved paths, on both sides.
	want, err := filepath.EvalSymlinks(filepath.Join(cwd, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	for what, dir := range map[string]string{"Dir": b.Dir, "PromptsDir": b.PromptsDir, "IterPath": b.IterPath} {
		if dir == "" {
			t.Fatalf("%s is empty", what)
		}
		if !filepath.IsAbs(dir) {
			t.Errorf("%s = %q is not absolute", what, dir)
		}
		got, err := filepath.EvalSymlinks(dir)
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if got != want && !strings.HasPrefix(got, want+string(filepath.Separator)) {
			t.Errorf("%s = %q is not under the cache root %q", what, got, want)
		}
	}
}

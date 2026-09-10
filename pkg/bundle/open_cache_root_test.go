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
	for what, dir := range map[string]string{"Dir": b.Dir, "PromptsDir": b.PromptsDir, "IterPath": b.IterPath} {
		if dir == "" {
			t.Fatalf("%s is empty", what)
		}
		if !filepath.IsAbs(dir) {
			t.Errorf("%s = %q is not absolute", what, dir)
		}
		if !strings.HasPrefix(dir, filepath.Join(cwd, "cache")) {
			t.Errorf("%s = %q is not under the cache root %q", what, dir, filepath.Join(cwd, "cache"))
		}
	}
}

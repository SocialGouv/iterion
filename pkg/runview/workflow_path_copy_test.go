package runview

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// A studio run of a bundle's SIBLING entry records the materialised copy
// (`<hash>-worker.bot`, outside the bundle) as FilePath and the bundle as
// BundlePath: the view compiles the entry the copy was made of — the
// source as it is NOW, beside its fragments and prompts — never the stale
// copy bare (C003 on a prompt-using sibling), and never main.bot (#1367
// review, MEDIUM 2).
func TestResolveWorkflowPath_RedirectsAMaterialisedCopyToItsBundleEntry(t *testing.T) {
	dir := t.TempDir()
	write := func(name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte("workflow x:\n  entry: done\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("main.bot")
	write("worker.bot")
	copyDir := t.TempDir()
	copyOf := func(entry string) string {
		t.Helper()
		p := filepath.Join(copyDir, "a1b2c3d4e5f6-"+entry)
		if err := os.WriteFile(p, []byte("workflow stale:\n  entry: done\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if got := resolveWorkflowPath(&store.Run{FilePath: copyOf("worker.bot"), BundlePath: dir}); got != filepath.Join(dir, "worker.bot") {
		t.Errorf("sibling copy: got %q, want the bundle's worker.bot", got)
	}
	if got := resolveWorkflowPath(&store.Run{FilePath: copyOf("main.bot"), BundlePath: dir}); got != filepath.Join(dir, "main.bot") {
		t.Errorf("main copy: got %q, want the bundle's main.bot", got)
	}
}

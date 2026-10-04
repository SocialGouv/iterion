package runtime

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestResumeBundleWorkflowResolvesAMaterialisedCopyToItsEntry: a studio
// launch records the store's materialised copy (`<hash>-<entry>`,
// OUTSIDE the bundle) as the run's FilePath and the bundle as BundlePath.
// The resume must compile the entry the copy was made of: the sibling for
// a sibling launch — else the persisted hash refuses a source that never
// changed, and --force would run main.bot against the sibling's
// checkpoint — and main.bot for a main launch (#1367 review, HIGH 1).
func TestResumeBundleWorkflowResolvesAMaterialisedCopyToItsEntry(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"main.bot":      "workflow main_w:\n  entry: done\n",
		"worker.bot":    "workflow worker_w:\n  entry: done\n",
		"manifest.yaml": "schema_version: 1\nname: x\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	b, err := bundle.OpenDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	cache := t.TempDir()
	copyOf := func(entry string) string {
		p := filepath.Join(cache, "a1b2c3d4e5f6-"+entry)
		if err := os.WriteFile(p, []byte("workflow stale:\n  entry: done\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	r := &store.Run{ID: "run-1"} // no BundleWorkflow: a path-launched entry
	if got, _, err := ResumeBundleWorkflow(r, b, copyOf("worker.bot")); err != nil || got != filepath.Join(dir, "worker.bot") {
		t.Fatalf("ResumeBundleWorkflow(copy of worker.bot) = (%q, %v), want the bundle's worker.bot — a sibling run must not resume as main.bot", got, err)
	}
	if got, _, err := ResumeBundleWorkflow(r, b, copyOf("main.bot")); err != nil || got != filepath.Join(dir, "main.bot") {
		t.Fatalf("ResumeBundleWorkflow(copy of main.bot) = (%q, %v), want the bundle's main.bot", got, err)
	}
	// A copy whose entry is gone resolves to the bundle's main, as any
	// unplaceable persisted path does.
	if got, _, err := ResumeBundleWorkflow(r, b, copyOf("ghost.bot")); err != nil || got != b.IterPath {
		t.Fatalf("ResumeBundleWorkflow(copy of a missing entry) = (%q, %v), want IterPath %q", got, err, b.IterPath)
	}
}

package runview

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The no-storeDir merge clone lives in a per-run private temp dir, not in
// the fixed `$TMPDIR/iterion-merges/<runID>` a local attacker could
// pre-plant: the path is unpredictable (an iterion-merge- MkdirTemp),
// 0700, never a pre-existing path, and stable for the service's lifetime
// so the clone materialises once and the later merge attempts reuse it.
// The pre-planted fixed path — .git included, the reuse signal the old
// code read — is never looked at.
func TestTheTempMergeCloneRefusesAPreplantedPath(t *testing.T) {
	runID := "run-merge-clone"
	fixed := filepath.Join(os.TempDir(), "iterion-merges", runID)
	if err := os.MkdirAll(filepath.Join(fixed, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Join(os.TempDir(), "iterion-merges")) })

	s := &Service{} // no store dir: the temp fallback is the only root
	root := s.repoTargetedMergeRoot(runID)
	if root == fixed {
		t.Fatalf("the merge clone root is the pre-plantable fixed path: %q", root)
	}
	if !strings.HasPrefix(root, os.TempDir()) || !strings.Contains(filepath.Base(root), "iterion-merge-") {
		t.Fatalf("the root is not a per-run MkdirTemp under the temp dir: %q", root)
	}
	if info, err := os.Stat(root); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("the root is not a private 0700 directory: %v %v", info, err)
	}
	// The pre-planted .git is not read: the fresh root holds none, and the
	// fixed path's does not count.
	if s.hasRepoTargetedMergeRoot(runID) {
		t.Fatal("a pre-planted merge clone was adopted")
	}
	// Stable for the service's lifetime: the clone materialises once and
	// the later attempts reuse it.
	if again := s.repoTargetedMergeRoot(runID); again != root {
		t.Fatalf("the root moved between two calls: %q then %q", root, again)
	}
	// A second run gets its own root.
	if other := (&Service{}).repoTargetedMergeRoot("run-other"); other == root {
		t.Fatal("two runs share one temp merge root")
	}
}

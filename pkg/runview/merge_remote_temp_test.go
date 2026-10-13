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
	other := (&Service{}).repoTargetedMergeRoot("run-other")
	if other == root {
		t.Fatal("two runs share one temp merge root")
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(root)
		_ = os.RemoveAll(other)
	})
}

// The first temp merge root this process creates sweeps the orphaned
// iterion-merge-* dirs a previous process left behind — a restart during a
// merge orphaned a repo-sized clone the fresh instance could not reach
// (the mapping is the in-memory cache). Non-matching dirs survive.
func TestTheFirstTempMergeRootSweepsTheOrphans(t *testing.T) {
	orphan, err := os.MkdirTemp("", "iterion-merge-orphan-")
	if err != nil {
		t.Fatal(err)
	}
	keeper, err := os.MkdirTemp("", "iterion-unrelated-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(orphan)
		_ = os.RemoveAll(keeper)
	})

	sweepOrphanMergeTemps() // the sweep body under test; the Once wraps it in production
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("the orphaned clone survived the sweep: %v", err)
	}
	if _, err := os.Stat(keeper); err != nil {
		t.Fatalf("the sweep took a directory that is not its own: %v", err)
	}
}

// The read paths (the existence probe, the removal) create nothing: a run
// whose clone was never materialised answers false and removes nothing,
// and the service's temp map stays empty.
func TestTheMergeRootReadPathsCreateNothing(t *testing.T) {
	s := &Service{}
	if s.hasRepoTargetedMergeRoot("run-never-materialised") {
		t.Fatal("a run with no clone reads as materialised")
	}
	s.removeRepoTargetedMergeRoot("run-never-materialised")
	s.mergeTempsMu.Lock()
	n := len(s.mergeTemps)
	s.mergeTempsMu.Unlock()
	if n != 0 {
		t.Fatalf("the read paths created %d temp roots", n)
	}
}

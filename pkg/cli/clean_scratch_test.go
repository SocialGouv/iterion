package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/memory"
)

// A workspace can pile up gigabytes of ${PROJECT_SCRATCH_DIR} while never
// running a `worktree: auto` bot. Discovery keyed on worktrees/ alone made
// those stores invisible to --all-projects, so the sweep silently covered
// part of the machine and reported success — found by probing the real
// command with an aged scratch entry, not by reading the code.
//
// Mutation coverage: narrow isSweepableStore back to hasWorktreeDir → the
// scratch-only store is no longer discovered and this fails.
func TestSweepableStoreIncludesScratchOnlyStores(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	scratchOnly := filepath.Join(root, "scratch-only")
	if err := os.MkdirAll(filepath.Join(scratchOnly, "scratch"), 0o755); err != nil {
		t.Fatal(err)
	}
	worktreeOnly := filepath.Join(root, "worktree-only")
	if err := os.MkdirAll(filepath.Join(worktreeOnly, "worktrees"), 0o755); err != nil {
		t.Fatal(err)
	}
	neither := filepath.Join(root, "neither")
	if err := os.MkdirAll(neither, 0o755); err != nil {
		t.Fatal(err)
	}

	if !isSweepableStore(scratchOnly) {
		t.Error("a store with scratch but no worktrees is not swept — its scratch grows forever")
	}
	if !isSweepableStore(worktreeOnly) {
		t.Error("a store with worktrees is not swept")
	}
	if isSweepableStore(neither) {
		t.Error("a directory holding neither is treated as a store")
	}
}

// The two store layouts put scratch in different places, and assuming one
// is how a sweep quietly misses half of them: a per-project store keeps
// scratch as a sibling of runs/, while a store INSIDE a workspace
// (<repo>/.iterion) has its scratch in the global data dir, keyed by the
// repo root.
func TestScratchRootForStoreHandlesBothLayouts(t *testing.T) {
	repo := t.TempDir()
	localStore := filepath.Join(repo, ".iterion")

	got := scratchRootForStore(localStore)
	want := memory.WorkspaceScratchDir(repo)
	if got != want {
		t.Errorf("workspace store → %q, want %q (scratch never lives inside a local store)", got, want)
	}
}

// The sweep must report what it would take without taking it, like every
// other half of this command.
func TestSweepStoreScratchDryRunReportsWithoutDeleting(t *testing.T) {
	// A per-project store: scratch is a sibling of runs/, which is the
	// layout --all-projects walks. Redirect the data dir so the test never
	// reads or writes the operator's own ~/.iterion.
	home := t.TempDir()
	t.Setenv("ITERION_HOME", home)
	store := filepath.Join(home, "projects", "-some-workspace")
	entry := filepath.Join(store, "scratch", "old-state")
	if err := os.MkdirAll(entry, 0o755); err != nil {
		t.Fatal(err)
	}
	blob := filepath.Join(entry, "blob.bin")
	if err := os.WriteFile(blob, make([]byte, 2048), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-30 * 24 * time.Hour)
	for _, p := range []string{blob, entry} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}

	now := func() time.Time { return time.Now() }
	var r CleanResult
	r.DryRun = true
	sweepStoreScratch([]string{store}, CleanOptions{OlderThan: 168 * time.Hour}, now, &r)

	if len(r.Scratch) != 1 || r.ScratchBytes == 0 {
		t.Fatalf("dry run reported %d entries / %d bytes, want the one it would take", len(r.Scratch), r.ScratchBytes)
	}
	if _, err := os.Stat(entry); err != nil {
		t.Error("dry run deleted the entry")
	}

	// …and with --apply it goes.
	var applied CleanResult
	sweepStoreScratch([]string{store}, CleanOptions{OlderThan: 168 * time.Hour, Apply: true}, now, &applied)
	if _, err := os.Stat(entry); !os.IsNotExist(err) {
		t.Error("--apply left the stale entry behind")
	}
}

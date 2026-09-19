package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
)

// runOutputPaths decides what counts as the pass's work; the tree-noise list
// (pkg/treenoise) is what it sets aside. F6 of the plan review: git's
// `:(exclude,top).claude` hides a top-level FILE named .claude too, so the
// predicate must match the exact name as well as the prefix — otherwise a
// worktree "dirty" with noise alone sends finalize into an empty commit and
// a PreserveWorktree warning.
func TestRunOutputPathsLeaveTheTreeNoiseOut(t *testing.T) {
	porcelain := strings.Join([]string{
		" M docs/adr/0009-record.md", // real work, tracked modification
		"?? .claude/settings.json",   // the engine's mirror
		"?? .claude",                 // a top-level FILE named .claude (F6)
		" M devbox.lock",             // the drift every devbox run writes
		"?? devbox.json",             // NOT noise: a dependency bot's deliverable
		"?? .claudeish",              // a sibling that merely starts alike
		"R  old.md -> docs/new.md",   // a rename: the destination decides
	}, "\n")
	got := runOutputPaths(porcelain)
	want := []string{"docs/adr/0009-record.md", "devbox.json", ".claudeish", "docs/new.md"}
	if len(got) != len(want) {
		t.Fatalf("runOutputPaths = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("runOutputPaths[%d] = %q, want %q (full: %q)", i, got[i], want[i], got)
		}
	}
}

// F4 (plan review): a dependency bot that dies before its commit leaves its
// DELIVERABLE dirty — devbox.json (real work, banked) beside the lock that
// travels with it (noise, dropped). Dropping the lock is safe on purpose:
// the lock is derivable from devbox.json, and banking it would pin a half-
// written resolution into a wip commit nothing ever merges.
func TestRunOutputPathsKeepTheDeliverableAndDropItsLock(t *testing.T) {
	porcelain := strings.Join([]string{
		" M devbox.json",
		" M devbox.lock",
	}, "\n")
	got := runOutputPaths(porcelain)
	if len(got) != 1 || got[0] != "devbox.json" {
		t.Fatalf("runOutputPaths = %q, want [devbox.json] — the deliverable is banked, the lock travels with it unbanked", got)
	}
}

// The staging gesture must agree with the probe that decides whether
// staging is needed at all: both carry the tree-noise pathspecs, or the
// wip bank sweeps the mirror and the drifted lock the moment anything real
// is dirty — the disagreement #1364's fix removed for the probe alone.
func TestStageWorkArgsCarryTheTreeNoisePathspecs(t *testing.T) {
	got := stageWorkArgs()
	want := []string{"add", "-A", "--", ":/", ":(exclude,top).claude", ":(exclude,top)devbox.lock"}
	if len(got) != len(want) {
		t.Fatalf("stageWorkArgs() = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("stageWorkArgs()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// End to end, on the real finalize path: a worktree whose dirt is the
// engine's mirror, a drifted lock and ONE real file banks the real file
// only — the wip commit the operator is shown never carries tree noise.
func TestFinalizeWorktree_WipBankLeavesTheTreeNoiseOut(t *testing.T) {
	repo, originalTip := initBareishRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	gittest.Run(t, repo, "worktree", "add", wt, "HEAD")
	t.Cleanup(func() { _, _ = gittest.Try(repo, "worktree", "remove", "--force", wt) })

	// The tracked lock, as the run found it.
	addCommit(t, wt, "devbox.lock", "plugin_version: 0.0.4\n", "baseline with a lock")

	// The noise (the mirror, untracked; the lock, drifted) and the run's
	// own work beside it.
	if err := os.MkdirAll(filepath.Join(wt, ".claude", "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(wt, ".claude", "skills", "mirrored.md"), "iterion wrote this\n")
	writeFile(t, filepath.Join(wt, "devbox.lock"), "plugin_version: 0.0.5\n")
	writeFile(t, filepath.Join(wt, "real.md"), "the pass's work\n")

	res := finalizeWorktree(worktreeContext{
		repoRoot:       repo,
		wtPath:         wt,
		originalBranch: "main",
		originalTip:    originalTip,
	}, finalizeOptions{runName: "wip-noise-test", runID: "run_n", autoMerge: true, mergeStrategy: "merge"}, nil)

	if !res.WipBanked {
		t.Fatalf("expected WipBanked=true, got %+v", res)
	}
	show := gittest.Run(t, repo, "show", "--stat", "--format=%s", res.FinalCommit)
	if !strings.Contains(show, "real.md") {
		t.Fatalf("banked commit missing the run's work:\n%s", show)
	}
	if strings.Contains(show, ".claude") {
		t.Fatalf("banked commit carries the .claude/ mirror:\n%s", show)
	}
	lockShow, lockErr := gittest.Try(repo, "show", res.FinalCommit+":devbox.lock")
	if lockErr == nil && strings.Contains(lockShow, "0.0.5") {
		t.Fatalf("banked commit carries the drifted devbox.lock:\n%s", lockShow)
	}
}

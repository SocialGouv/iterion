package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
)

// A run's workdir nested INSIDE an enclosing checkout climbs, for every git
// lookup, into that checkout: the bank would commit the checkout's
// uncommitted work onto ITS current branch (the 2026-09-23 incident — 19
// files, this repository's). The wip bank now proves the directory through
// git and refuses: nothing of the enclosing checkout is committed, and the
// finalisation says why (#1782).
func TestFinalizeWorktree_RefusesAWorkdirNestedInAForeignCheckout(t *testing.T) {
	// The enclosing checkout: its own repository, a branch, uncommitted work.
	scratch := filepath.Join(t.TempDir(), "scratch")
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		t.Fatal(err)
	}
	gittest.Run(t, scratch, "init", "-b", "review")
	gittest.Run(t, scratch, "config", "user.email", "test@iterion.dev")
	gittest.Run(t, scratch, "config", "user.name", "test")
	writeFile(t, filepath.Join(scratch, "seed.txt"), "seed\n")
	gittest.Run(t, scratch, "add", "-A")
	gittest.Run(t, scratch, "commit", "-m", "seed")
	scratchTip := gittest.Run(t, scratch, "rev-parse", "review")
	writeFile(t, filepath.Join(scratch, "reviewer-notes.txt"), "uncommitted notes\n")
	writeFile(t, filepath.Join(scratch, "wip.go"), "package review\n")

	// The run's workdir: a plain directory INSIDE the scratch checkout —
	// no .git of its own, every git lookup climbs into the scratch.
	wt := filepath.Join(scratch, "agent-scratch", "run")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(wt, "run-output.txt"), "the run's own work\n")

	res := finalizeWorktree(worktreeContext{
		repoRoot:       scratch,
		wtPath:         wt,
		originalBranch: "review",
		originalTip:    scratchTip,
	}, finalizeOptions{runName: "nested-checkout-test", runID: "run_nested"}, nil)

	if res.WipBanked {
		t.Fatalf("the nested workdir was banked — the scratch checkout's work would ride the run's commit: %+v", res)
	}
	if !res.PreserveWorktree {
		t.Fatalf("a refused bank must preserve the worktree, got %+v", res)
	}
	tip := gittest.Run(t, scratch, "rev-parse", "review")
	if tip != scratchTip {
		t.Fatalf("the scratch checkout's branch moved to %s — nothing may be committed in a repository the run did not create its worktree in", tip)
	}
	porcelain := gittest.Run(t, scratch, "status", "--porcelain")
	for _, want := range []string{"reviewer-notes.txt", "wip.go"} {
		if !strings.Contains(porcelain, want) {
			t.Fatalf("the scratch checkout's uncommitted file %s is no longer uncommitted:\n%s", want, porcelain)
		}
	}
	if strings.Contains(porcelain, "run-output.txt") {
		t.Errorf("the run's own output landed in the scratch checkout:\n%s", porcelain)
	}
}

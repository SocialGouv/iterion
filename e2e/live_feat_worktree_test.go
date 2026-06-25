//go:build live

package e2e

import (
	"os"
	"testing"
	"time"
)

// TestLive_Feat_Worktree exercises worktree:auto finalization with a
// tool-only workflow (no LLM): a tool node commits inside the auto-created
// worktree, so the engine's finalizeWorktree must record a persistent
// FinalBranch/FinalCommit on the run.
//
// Requires: git only (no credentials — deterministic). Expected: seconds.
func TestLive_Feat_Worktree(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	loadDotEnv(t)
	requireBinaryInPath(t, "git")

	workspaceDir, err := os.MkdirTemp("", "iterion-feat-worktree-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	seedGitRepo(t, workspaceDir)

	res := runBotLive(t, liveSpec{
		runIDBase:    "live-feat-worktree",
		botFile:      "feat_worktree.bot",
		workspaceDir: workspaceDir,
		timeout:      3 * time.Minute,
		withWorkDir:  true,
	})

	assertNodesFinished(t, res.events, "make_commit")
	if res.run.FinalBranch == "" && res.run.FinalCommit == "" {
		t.Errorf("expected worktree finalization to set FinalBranch/FinalCommit; got branch=%q commit=%q",
			res.run.FinalBranch, res.run.FinalCommit)
	} else {
		t.Logf("worktree finalized: branch=%q commit=%q merged_into=%q", res.run.FinalBranch, res.run.FinalCommit, res.run.MergedInto)
	}
}

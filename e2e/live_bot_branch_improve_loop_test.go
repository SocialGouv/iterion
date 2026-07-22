//go:build live

package e2e

import (
	"os"
	"testing"
	"time"
)

// TestLive_Bot_BranchImproveLoop runs the branch-improve-loop bot (Billy)
// against a base..branch diff containing a planted logic bug. In v2 (ADR-058)
// Billy is ONE adaptive `campaign` agent: it reads the branch diff, reviews +
// improves it, and commits each fix in stride inside a fresh worktree
// (worktree: auto; no sandbox block per ADR-082 — direct engine runs are host-side); a deterministic build/test gate re-checks
// the tree each pass until the branch is clean and green.
//
// Reliability invariants: the campaign fires and the deterministic verify gate
// (verify_run) is evaluated (the loop ran); commits are logged. The quality
// panel grades the fixes + value.
//
// Requires: claude CLI + OpenAI.
// Expected: ~40-70 min.
func TestLive_Bot_BranchImproveLoop(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	loadDotEnv(t)
	requireCLI(t, "claude")
	requireOpenAI(t)

	workspaceDir, err := os.MkdirTemp("", "iterion-branch-improve-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Logf("Workspace (persists): %s", workspaceDir)

	base := map[string]string{
		"go.mod": "module iterion-live-fixture\n\ngo 1.25\n",
		"add.go": "package fixture\n\n// Add returns a + b.\nfunc Add(a, b int) int { return a + b }\n",
	}
	branch := map[string]string{
		"multiply.go": `package fixture

// Multiply returns a*b but has an off-by-one in the loop bound:
// it computes (a-1)*b for positive a.
func Multiply(a, b int) int {
	result := 0
	for i := 0; i < a-1; i++ {
		result += b
	}
	return result
}
`,
	}
	seedBranchDiffFixture(t, workspaceDir, "main", base, branch, "feature")
	seedCommits := workspaceCommitCount(t, workspaceDir)

	// worktree:auto → do NOT set workspace_dir (defaults to ${PROJECT_DIR},
	// remapped to the worktree). withWorkDir mounts the seeded repo.
	vars := map[string]any{
		"base_ref":    "main",
		"open_mr":     false,
		"scope_notes": "Review the diff for correctness; fix the off-by-one in Multiply.",
	}
	res := runBotLive(t, liveSpec{
		runIDBase:    "live-branch-improve-loop",
		botFile:      "branch-improve-loop/main.bot",
		workspaceDir: workspaceDir,
		vars:         vars,
		inputs:       vars,
		timeout:      70 * time.Minute,
		withWorkDir:  true,
	})

	if countFinished(res.events, "campaign") == 0 {
		t.Errorf("expected the campaign agent to fire")
	}
	if countFinished(res.events, "verify_run") == 0 {
		t.Errorf("expected the deterministic verify gate (verify_run) to be evaluated")
	}
	t.Logf("commits after run: %d (seed %d)", workspaceCommitCount(t, workspaceDir), seedCommits)

	assessQuality(t, res, qualityInput{
		kind:          "bot",
		name:          "branch-improve-loop",
		persona:       "Billy",
		primaryFamily: "anthropic",
		task:          "Review a base..branch diff with a planted off-by-one in Multiply and auto-commit a fix.",
		workProduct:   worktreeArtifactEvidence(t, workspaceDir),
	})
}

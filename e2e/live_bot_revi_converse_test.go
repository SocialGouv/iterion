//go:build live

package e2e

import (
	"os"
	"testing"
	"time"
)

// TestLive_Bot_ReviConverse runs the revi-converse bot against a seeded
// base..branch diff plus an operator question about that change. With no
// forge wired (pr_url/discussion_id empty) the bot produces an answer but
// skips posting — exactly the read-only path a test can exercise safely.
//
// Reliability invariants: converse_agent fires and produces a non-empty
// answer preview (grounded in the diff). Then the quality panel grades the
// answer's groundedness + value.
//
// Requires: claude CLI. Expected: ~5-12 min.
func TestLive_Bot_ReviConverse(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	loadDotEnv(t)
	requireCLI(t, "claude")

	workspaceDir, err := os.MkdirTemp("", "iterion-revi-converse-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Logf("Workspace (persists): %s", workspaceDir)

	base := map[string]string{
		"go.mod":  "module iterion-live-fixture\n\ngo 1.25\n",
		"pick.go": "package fixture\n\n// Pick returns items[idx].\nfunc Pick(items []string, idx int) string {\n\tif idx < 0 || idx >= len(items) {\n\t\treturn \"\"\n\t}\n\treturn items[idx]\n}\n",
	}
	branch := map[string]string{
		"pick.go": "package fixture\n\n// Pick returns items[idx].\nfunc Pick(items []string, idx int) string {\n\treturn items[idx]\n}\n",
	}
	seedBranchDiffFixture(t, workspaceDir, "main", base, branch, "feature")

	question := "In this diff, why was the bounds check removed from Pick, and is that safe?"
	vars := map[string]any{
		"workspace_dir":     workspaceDir,
		"base_ref":          "main",
		"converse_question": question,
		"pr_url":            "", // no forge → answer produced, posting skipped
		"discussion_id":     "",
		"thread_context":    "Reviewer asked about the removed bounds check in Pick.",
	}
	res := runBotLive(t, liveSpec{
		runIDBase:    "live-revi-converse",
		bundleDir:    "../bots/revi-converse",
		workspaceDir: workspaceDir,
		vars:         vars,
		inputs:       vars,
		timeout:      12 * time.Minute,
	})

	assertNodesFinished(t, res.events, "converse_agent")
	ca, ok := lastNodeOutput(res.events, "converse_agent")
	if !ok {
		t.Fatalf("converse_agent produced no output")
	}
	preview, _ := ca["answer_preview"].(string)
	summary, _ := ca["summary"].(string)
	if len(preview) == 0 && len(summary) == 0 {
		t.Errorf("expected a non-empty answer (answer_preview or summary)")
	}

	assessQuality(t, res, qualityInput{
		kind:          "bot",
		name:          "revi-converse",
		persona:       "Revi",
		primaryFamily: "anthropic",
		task:          "Answer an operator's question about a seeded diff (removed bounds check) grounded in the actual change; read-only, no forge.",
		workProduct:   "## question\n" + question + "\n\n## answer\n" + preview + "\n" + summary + "\n\n## diff\n" + gitOut(workspaceDir, "--no-pager", "diff", "main..feature"),
	})
}

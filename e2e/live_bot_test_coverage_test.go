//go:build live

package e2e

import (
	"os"
	"testing"
	"time"
)

// TestLive_Bot_TestCoverage runs the test-coverage bot (Testy) against a
// package with an exported, untested function. v2 (ADR-058
// minimal-framing): ONE campaign agent writes real tests committing each
// in stride, then the deterministic gate re-runs the suite and verifies
// genuinely-new test code landed, inside a worktree (worktree: auto +
// no sandbox block per ADR-082 — direct engine runs are host-side).
//
// Reliability invariants: campaign/verify_build/verify_run fire and the
// gate reports a newly-added test (new_test_code). The quality panel then
// grades the tests (anti-façade: are the assertions real?) + value.
//
// Requires: claude CLI.
// Expected: ~20-50 min.
func TestLive_Bot_TestCoverage(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	loadDotEnv(t)
	requireCLI(t, "claude")

	workspaceDir, err := os.MkdirTemp("", "iterion-test-coverage-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Logf("Workspace (persists): %s", workspaceDir)

	seedGoModuleFixture(t, workspaceDir, map[string]string{
		"stringutil.go": `package fixture

// Reverse returns s with its runes in reverse order.
func Reverse(s string) string {
	r := []rune(s)
	for i, j := 0, len(r)-1; i < j; i, j = i+1, j-1 {
		r[i], r[j] = r[j], r[i]
	}
	return string(r)
}
`,
	})
	seedCommits := workspaceCommitCount(t, workspaceDir)

	vars := map[string]any{
		"target": "Add unit tests for the Reverse function (it has none).",
	}
	res := runBotLive(t, liveSpec{
		runIDBase:    "live-test-coverage",
		botFile:      "test-coverage/main.bot",
		workspaceDir: workspaceDir,
		vars:         vars,
		inputs:       vars,
		timeout:      70 * time.Minute,
		withWorkDir:  true,
	})

	assertNodesFinished(t, res.events, "campaign", "verify_build", "verify_run", "gate")
	if vr, ok := lastNodeOutput(res.events, "verify_run"); ok {
		t.Logf("verify_run: passed=%v new_test_code=%v", vr["passed"], vr["new_test_code"])
		if added, _ := vr["new_test_code"].(bool); !added {
			t.Errorf("expected verify_run.new_test_code=true (a test was added)")
		}
	}
	if got := workspaceCommitCount(t, workspaceDir); got <= seedCommits {
		t.Errorf("expected the campaign to land at least one test commit in stride (seed %d, after %d)", seedCommits, got)
	}
	t.Logf("commits after run: %d (seed %d)", workspaceCommitCount(t, workspaceDir), seedCommits)

	assessQuality(t, res, qualityInput{
		kind:          "bot",
		name:          "test-coverage",
		persona:       "Testy",
		primaryFamily: "anthropic",
		task:          "Add real unit tests for an untested Reverse function; verify they run and assert meaningfully.",
		workProduct:   worktreeArtifactEvidence(t, workspaceDir),
	})
}

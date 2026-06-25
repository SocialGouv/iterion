//go:build live

package e2e

import (
	"os"
	"testing"
	"time"
)

// TestLive_Bot_TestCoverage runs the test-coverage bot (Testy) against a
// package with an exported, untested function. Testy picks the target,
// writes real tests, runs them through a deterministic verify floor, and
// converges via a cross-family review loop inside a worktree (worktree:
// auto + sandbox-full), committing a `test:` change.
//
// Reliability invariants: plan/act/verify_run_tests fire and the verify
// gate reports a newly-added test (new_test_code). The quality panel then
// grades the tests (anti-façade: are the assertions real?) + value.
//
// Requires: claude CLI + OpenAI + docker w/ iterion-sandbox-full:edge.
// Expected: ~40-70 min.
func TestLive_Bot_TestCoverage(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	loadDotEnv(t)
	requireCLI(t, "claude")
	requireOpenAI(t)
	requireDockerImage(t, "ghcr.io/socialgouv/iterion-sandbox-full:edge")

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

	vars := map[string]interface{}{
		"scope_notes": "Add unit tests for the Reverse function (it has none).",
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

	assertNodesFinished(t, res.events, "plan", "act", "verify_run_tests")
	if vr, ok := lastNodeOutput(res.events, "verify_run_tests"); ok {
		t.Logf("verify_run_tests: passed=%v new_test_code=%v", vr["passed"], vr["new_test_code"])
		if added, _ := vr["new_test_code"].(bool); !added {
			t.Errorf("expected verify_run_tests.new_test_code=true (a test was added)")
		}
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

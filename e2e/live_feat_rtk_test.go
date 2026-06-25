//go:build live

package e2e

import (
	"os"
	"testing"
	"time"
)

// TestLive_Feat_Rtk exercises node-level rtk command-output compression: a
// tool node opts into `rtk: on`, so the runtime rewrites `git status` to its
// rtk-compressed equivalent before executing it. The test asserts the node
// runs cleanly with rtk active (rtk is a compressor, never a gate — a failed
// rewrite falls back to the original command, so the node must still finish).
//
// Requires: the rtk binary (ITERION_RTK_BIN or `rtk` in PATH). Skipped
// otherwise. Expected: seconds.
func TestLive_Feat_Rtk(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	loadDotEnv(t)
	if os.Getenv("ITERION_RTK_BIN") == "" {
		requireBinaryInPath(t, "rtk")
	}

	workspaceDir, err := os.MkdirTemp("", "iterion-feat-rtk-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	seedGitRepo(t, workspaceDir)

	res := runBotLive(t, liveSpec{
		runIDBase:    "live-feat-rtk",
		botFile:      "feat_rtk.bot",
		workspaceDir: workspaceDir,
		timeout:      3 * time.Minute,
		withWorkDir:  true, // ${PROJECT_DIR} → the seeded repo
	})

	assertNodesFinished(t, res.events, "rtk_probe")
	t.Logf("rtk_probe completed with rtk: on (compression applied, or fell back to the original command)")
}

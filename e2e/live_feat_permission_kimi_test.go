//go:build live

package e2e

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLive_Feat_Permission_Deny_Kimi proves the external-hook path against a
// real kimi model and real Bash tool call. The filesystem sentinel is the
// authority: model prose alone cannot prove that the tool did not execute.
func TestLive_Feat_Permission_Deny_Kimi(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	requireBinaryInPath(t, "kimi")

	workspaceDir, err := os.MkdirTemp("", "iterion-feat-permission-kimi-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	seedGitRepo(t, workspaceDir)
	sentinel := filepath.Join(workspaceDir, "permission-hook-must-not-create")

	res := runBotLive(t, liveSpec{
		runIDBase:    "live-feat-permission-deny-kimi",
		botFile:      "feat_permission_deny_kimi.bot",
		workspaceDir: workspaceDir,
		withWorkDir:  true,
		timeout:      8 * time.Minute,
	})

	assertNodesFinished(t, res.events, "runner")
	if _, err := os.Stat(sentinel); err == nil {
		t.Fatalf("kimi's denied Bash call executed: sentinel %s exists", sentinel)
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat sentinel: %v", err)
	}
	out, _ := lastNodeOutput(res.events, "runner")
	if ran, _ := out["ran_bash"].(bool); ran {
		t.Errorf("filesystem proves the call was blocked, but kimi reported ran_bash=true: %v", out)
	}
}

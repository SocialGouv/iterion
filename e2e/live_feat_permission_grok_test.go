//go:build live

package e2e

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLive_Feat_Permission_Deny_Grok proves the external-hook path against a
// real grok model and a real run_terminal_command call. The filesystem
// sentinel is the authority: model prose alone cannot prove that the tool did
// not execute, and a model that merely *claims* it was blocked would otherwise
// let a broken gate pass as green.
//
// This is the test that earns grok its entry in ir.gateEnforcingModes. Deleting
// or skipping it turns that entry back into a declaration — the exact failure
// mode issue #476 exists to prevent.
func TestLive_Feat_Permission_Deny_Grok(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	requireBinaryInPath(t, "grok")

	workspaceDir, err := os.MkdirTemp("", "iterion-feat-permission-grok-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	seedGitRepo(t, workspaceDir)
	sentinel := filepath.Join(workspaceDir, "permission-hook-must-not-create")

	res := runBotLive(t, liveSpec{
		runIDBase:    "live-feat-permission-deny-grok",
		botFile:      "feat_permission_deny_grok.bot",
		workspaceDir: workspaceDir,
		withWorkDir:  true,
		timeout:      8 * time.Minute,
	})

	assertNodesFinished(t, res.events, "runner")
	if _, err := os.Stat(sentinel); err == nil {
		t.Fatalf("grok's denied run_terminal_command call executed: sentinel %s exists", sentinel)
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat sentinel: %v", err)
	}
	out, _ := lastNodeOutput(res.events, "runner")
	if ran, _ := out["ran_bash"].(bool); ran {
		t.Errorf("filesystem proves the call was blocked, but grok reported ran_bash=true: %v", out)
	}
}

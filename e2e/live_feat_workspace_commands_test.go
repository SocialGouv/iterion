//go:build live

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// workspaceCommandToken is the payload of the contributed command. It is
// unguessable on purpose: a model that never received the command's body
// cannot emit it, so the assertion cannot pass by accident.
const workspaceCommandToken = "KUMQUAT-7731"

// TestLive_Feat_WorkspaceCommands is the live witness behind the
// "Workspace `/commands`" column of docs/backends.md, for BOTH proven
// cells: the same workflow sends `/parity-probe` to a claude_code node and
// to a claw node, and both must answer with the token that exists only
// inside `<workspace>/.claude/commands/parity-probe.md`.
//
// Both nodes declare `tools: []`, so no Read/Bash/Glob can reach the file:
// resolving the invocation into the command body is the only path to the
// token. That is what makes this a parity test rather than a reachability
// test — it fails if either backend stops resolving workspace commands.
//
// Requires: claude CLI (the claude_code half) and a claw-usable credential.
// Expected: ~2-4 min.
func TestLive_Feat_WorkspaceCommands(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	loadDotEnv(t)
	// Both halves spend: the CLI for the claude_code node, an OpenAI key for
	// the claw node. Without the second guard a missing credential surfaces
	// as "the contributed command never reached it" — a FEATURE failure
	// reported for a missing prerequisite, which is how six tests in this
	// suite were once misread.
	requireCLI(t, "claude")
	requireOpenAI(t)

	workspaceDir, err := os.MkdirTemp("", "iterion-feat-workspace-commands-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	seedGitRepo(t, workspaceDir)

	// The contribution under test, COMMITTED: a run with `worktree: auto`
	// executes in a fresh checkout of HEAD, so an untracked command file
	// never reaches the tree the nodes actually see. (Leaving it untracked
	// made the claw half fail while the claude_code half passed on a path
	// resolution that had nothing to do with the workspace — a green that
	// would have proven the wrong thing.)
	cmdDir := filepath.Join(workspaceDir, ".claude", "commands")
	if err := os.MkdirAll(cmdDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	body := "---\ndescription: emit the parity token\n---\n" +
		"Set the answer's `text` field to exactly this token: " + workspaceCommandToken + "\n"
	if err := os.WriteFile(filepath.Join(cmdDir, "parity-probe.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	runCmd(t, workspaceDir, "git", "add", ".claude/commands/parity-probe.md")
	runCmd(t, workspaceDir, "git", "commit", "-m", "feat: contribute a parity probe command")

	res := runBotLive(t, liveSpec{
		runIDBase:    "live-feat-workspace-commands",
		botFile:      "feat_workspace_commands.bot",
		workspaceDir: workspaceDir,
		withWorkDir:  true,
		timeout:      10 * time.Minute,
	})

	assertNodesFinished(t, res.events, "via_claude_code", "via_claw")
	for _, node := range []string{"via_claude_code", "via_claw"} {
		out, ok := lastNodeOutput(res.events, node)
		if !ok {
			t.Errorf("node %q produced no output", node)
			continue
		}
		got, _ := out["text"].(string)
		if !strings.Contains(got, workspaceCommandToken) {
			t.Errorf("%s answered %q — the contributed command never reached it; "+
				"with tools: [] the body is the only path to %s",
				node, got, workspaceCommandToken)
		}
	}
}

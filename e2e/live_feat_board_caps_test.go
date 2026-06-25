//go:build live

package e2e

import (
	"os"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// TestLive_Feat_BoardCaps exercises bot board capabilities: a claw agent
// granted `capabilities: [board.create, board.read]` must have the
// in-process board tools wired and actually invoke board.create. Store +
// board are isolated to the temp workspace.
//
// Requires: ANTHROPIC_API_KEY (claw). Expected: ~3-6 min.
func TestLive_Feat_BoardCaps(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	loadDotEnv(t)
	requireEnv(t, "ANTHROPIC_API_KEY")
	t.Setenv("ITERION_TEST_STORE_DIR", "workspace") // isolate board writes

	workspaceDir, err := os.MkdirTemp("", "iterion-feat-board-caps-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	seedGitRepo(t, workspaceDir)

	res := runBotLive(t, liveSpec{
		runIDBase:    "live-feat-board-caps",
		botFile:      "feat_board_caps.bot",
		workspaceDir: workspaceDir,
		timeout:      8 * time.Minute,
	})

	assertNodesFinished(t, res.events, "board_agent")
	calledBoard := eventDataMentions(res.events, "board", store.EventToolCalled) ||
		eventDataMentions(res.events, "create_issue", store.EventToolCalled, store.EventToolError)
	if !calledBoard {
		t.Errorf("expected the agent to invoke a board.* tool (no board tool-call event found)")
	}

	out, _ := lastNodeOutput(res.events, "board_agent")
	assessQuality(t, res, qualityInput{
		kind:          "feature",
		name:          "board-caps",
		primaryFamily: "anthropic",
		task:          "An agent with board.* capabilities creates one native-board issue.",
		workProduct:   "## board_agent output\n" + sprintAny(out),
	})
}

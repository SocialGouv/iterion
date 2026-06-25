//go:build live

package e2e

import (
	"os"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// TestLive_Feat_Permission_Ask exercises the permission gate in ASK mode:
// the workflow allow-lists only Read, so when the agent reaches for Bash the
// gate PAUSES the run for approval (the permission marker pauses without
// needing interaction: set). The test asserts the run reaches
// paused_waiting_human — the anti-prompt-injection boundary firing.
//
// Requires: ANTHROPIC_API_KEY or OpenAI (the agent is claw openai/gpt-5.5).
// Expected: ~2-5 min.
func TestLive_Feat_Permission_Ask(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	loadDotEnv(t)
	requireOpenAI(t)

	workspaceDir, err := os.MkdirTemp("", "iterion-feat-permission-ask-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	seedGitRepo(t, workspaceDir)

	res := runBotLive(t, liveSpec{
		runIDBase:    "live-feat-permission-ask",
		botFile:      "feat_permission_ask.bot",
		workspaceDir: workspaceDir,
		timeout:      6 * time.Minute,
		acceptPause:  true, // the ask gate is EXPECTED to pause the run
	})

	if res.run.Status != store.RunStatusPausedWaitingHuman {
		t.Errorf("expected ask-mode gate to pause the run (status=%s); the non-allow-listed Bash call should have suspended for approval", res.run.Status)
	} else {
		t.Logf("permission ask gate paused the run as expected (status=%s)", res.run.Status)
	}
}

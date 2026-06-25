//go:build live

package e2e

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/runview"
)

// TestLive_Feat_Fork exercises run forking: run a parent bot (a claw agent,
// so its LLM turn is captured for rehydration), then runview.Service.Fork
// it at that node/turn and assert a distinct child run is created and
// persisted (the same path `iterion fork` drives, resumable with
// `iterion resume`).
//
// Requires: OpenAI (the parent ping bot is claw openai/gpt-5.5). Expected:
// ~2-5 min.
func TestLive_Feat_Fork(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	loadDotEnv(t)
	requireOpenAI(t)

	workspaceDir, err := os.MkdirTemp("", "iterion-feat-fork-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	seedGitRepo(t, workspaceDir)

	// Parent run — a single claw LLM turn (captured for fork rehydration).
	res := runBotLive(t, liveSpec{
		runIDBase:    "live-feat-fork-parent",
		botFile:      "feat_sched.bot",
		workspaceDir: workspaceDir,
		timeout:      6 * time.Minute,
	})
	assertNodesFinished(t, res.events, "ping")

	svc, err := runview.NewService(res.storeDir)
	if err != nil {
		t.Fatalf("runview.NewService(%s): %v", res.storeDir, err)
	}
	result, err := svc.Fork(context.Background(), runview.ForkSpec{
		RunID:     res.runID,
		NodeID:    "ping",
		TurnIndex: 0,
		ForkName:  "live-feat-fork-child",
	})
	if err != nil {
		t.Fatalf("fork at ping/turn0: %v", err)
	}
	if result.NewRunID == "" || result.NewRunID == res.runID {
		t.Fatalf("fork did not create a distinct child run (new=%q parent=%q)", result.NewRunID, res.runID)
	}
	if child, err := res.store.LoadRun(context.Background(), result.NewRunID); err != nil || child == nil {
		t.Errorf("forked child run %q not found in store: %v", result.NewRunID, err)
	} else {
		t.Logf("fork created child run %s from parent %s at ping/turn0", result.NewRunID, res.runID)
	}
}

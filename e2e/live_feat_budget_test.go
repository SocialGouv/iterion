//go:build live

package e2e

import (
	"os"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// TestLive_Feat_Budget exercises budget enforcement: a tiny max_cost_usd
// cap (0.0001) that the first agent's real cost exceeds, so the engine must
// emit budget_exceeded and stop before the second node — landing in a
// resumable state (the "raise the cap + resume" recovery hook).
//
// Requires: ANTHROPIC_API_KEY (claw). Expected: ~2-5 min.
func TestLive_Feat_Budget(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	loadDotEnv(t)
	requireOpenAI(t)

	workspaceDir, err := os.MkdirTemp("", "iterion-feat-budget-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	seedGitRepo(t, workspaceDir)

	res := runBotLive(t, liveSpec{
		runIDBase:    "live-feat-budget",
		botFile:      "feat_budget.bot",
		workspaceDir: workspaceDir,
		timeout:      6 * time.Minute,
	})

	if !hasEventType(res.events, store.EventBudgetExceeded) {
		t.Errorf("expected a budget_exceeded event under a $0.0001 cap")
	}
	if res.run.Status == store.RunStatusFinished {
		t.Errorf("expected the run NOT to finish under the tiny budget (status=%s)", res.run.Status)
	} else {
		t.Logf("budget enforced: status=%s", res.run.Status)
	}
	// writer2 must NOT have run (budget tripped before it).
	if countFinished(res.events, "writer2") > 0 {
		t.Errorf("writer2 ran despite the budget cap — enforcement did not stop the run")
	}
}

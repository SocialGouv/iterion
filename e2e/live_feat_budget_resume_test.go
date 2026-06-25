//go:build live

package e2e

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestLive_Feat_BudgetResume exercises the "budget exceeded → raise the cap
// → resume → finish" recovery, end to end. Phase 1 runs feat_budget.bot
// under a $0.0001 cap so the engine emits budget_exceeded and stops before
// writer2 (failed_resumable). Phase 2 raises the workflow budget and resumes
// with a fresh engine over the same store — writer2 must now run.
//
// Requires: OpenAI (the writers are claw openai/gpt-5.5). Expected: ~3-8 min.
func TestLive_Feat_BudgetResume(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live test in short mode")
	}
	loadDotEnv(t)
	requireOpenAI(t)

	workspaceDir, err := os.MkdirTemp("", "iterion-feat-budget-resume-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	seedGitRepo(t, workspaceDir)

	// Phase 1 — tiny cap trips budget enforcement before writer2.
	res := runBotLive(t, liveSpec{
		runIDBase:    "live-feat-budget-resume",
		botFile:      "feat_budget.bot",
		workspaceDir: workspaceDir,
		timeout:      8 * time.Minute,
	})
	if !hasEventType(res.events, store.EventBudgetExceeded) {
		t.Fatalf("phase 1: expected a budget_exceeded event under the $0.0001 cap")
	}
	if countFinished(res.events, "writer2") > 0 {
		t.Fatalf("phase 1: writer2 ran before resume — the budget did not stop the run")
	}
	t.Logf("phase 1: budget enforced (status=%s)", res.run.Status)

	// Phase 2 — raise the cap on the same workflow + resume.
	res.wf.Budget.MaxCostUSD = 10.0
	exec2 := newLiveExecutor(t, res.wf, res.store, res.runID, workspaceDir)
	defer exec2.Close()
	eng2 := runtime.New(res.wf, res.store, exec2)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	rerr := eng2.Resume(ctx, res.runID, nil)
	if acceptable, reason := liveRunResultAcceptable(rerr); rerr != nil && !acceptable {
		t.Fatalf("phase 2 resume: unacceptable error: %v (%s)", rerr, reason)
	}

	events2, err := res.store.LoadEvents(context.Background(), res.runID)
	if err != nil {
		t.Fatalf("LoadEvents (post-resume): %v", err)
	}
	run2, _ := res.store.LoadRun(context.Background(), res.runID)
	if countFinished(events2, "writer2") == 0 {
		t.Errorf("phase 2: expected writer2 to run after raising the budget + resume (status=%v)", runStatus(run2))
	} else {
		t.Logf("phase 2: budget raised + resumed → writer2 ran (status=%v)", runStatus(run2))
	}
}

func runStatus(r *store.Run) string {
	if r == nil {
		return "unknown"
	}
	return string(r.Status)
}

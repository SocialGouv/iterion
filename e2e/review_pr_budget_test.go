package e2e

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// observedWorstCaseReviewerUSD is what review-pr's guard tier actually
// spent before the merge gate died with no verdict — twice on one
// revision of a 7-file PR, the automatic relaunch included (cost_usd
// 36/12 then 30/12, 2026-09-06). It is the spend RECORDED at the budget
// check, so it is a FLOOR on that workload's real cost: converge and the
// publish tail never ran.
const observedWorstCaseReviewerUSD = 36.0

// review-pr's max_cost_usd is not sized against the invoice, it is sized
// against the ENGINE's wall — and the two are not the same number.
//
// Once an axis sits in [90%, 100%) of its cap the engine refuses the next
// EXECUTOR-BACKED node and fails the run there, with no exit grace: only
// a real >=100% overrun earns the cap x 1.1 walk-forward
// (pkg/runtime/budget.go — findHardLimited fails outright, findExceeded
// routes through graceOrFailBudget). `merge_reviews` is a compute node and
// is dispatched before that check ever runs, so the join always fires —
// but `converge` right behind it is an agent, and converge is what writes
// the verdict the merge gate publishes.
//
// So a cap that merely EXCEEDS the measured spend still strands the run:
// at max_cost_usd 40 the observed $36 lands on exactly 90.0% and converge
// is refused, which is precisely the death the budget raise exists to end.
// The rule is cap > worst_case / 0.9, with headroom.
//
// This bills a guard-tier reviewer that worst case against the SHIPPED
// budget block and asserts the verdict path still reaches done. The
// engine's own threshold is the judge, so the guard tracks it instead of
// hardcoding 0.9 — an engine that widened the un-graced band would fail
// here too, and would genuinely be stranding this bot's verdict.
func TestReviewPRBudget_WorstCaseReviewerStillDeliversTheVerdict(t *testing.T) {
	wf := compileFixtureStubSafe(t, "review-pr/main.bot")
	if wf.Budget == nil || wf.Budget.MaxCostUSD <= 0 {
		t.Fatal("review-pr ships no max_cost_usd — this guard has nothing to size against")
	}

	exec := newScenarioExecutor()
	wireReviewPRStubs(exec)
	// Re-stub the guard tier's single full-strength reviewer to bill the
	// measured worst case. guard is mono, so this IS the run's spend when
	// converge's pre-exec check runs.
	exec.on("reviewer_claude", func(_ map[string]any) (map[string]any, error) {
		out := reviewOutputStub("claude")
		out["_cost_usd"] = observedWorstCaseReviewerUSD
		return out, nil
	})

	s := tmpStore(t)
	runID := "e2e-review-pr-budget-delivers-verdict"
	// guard is the DEFAULT tier: mono, one full-strength reviewer.
	err := runtime.New(wf, s, exec).Run(context.Background(), runID, map[string]any{
		"review_tier": "guard", "mono_family": "claude",
	})
	r, _ := s.LoadRun(context.Background(), runID)
	if err != nil || r == nil || r.Status != store.RunStatusFinished {
		status := store.RunStatus("<no run>")
		if r != nil {
			status = r.Status
		}
		t.Fatalf("a reviewer billing the measured worst case ($%.2f) must still reach done against "+
			"the shipped max_cost_usd %.0f (un-graced wall at $%.1f): status=%s err=%v\n"+
			"size the cap as cap > worst_case/0.9 — a cap merely above the measured spend "+
			"parks the run in the 90%% band and the merge gate posts no verdict",
			observedWorstCaseReviewerUSD, wf.Budget.MaxCostUSD, 0.9*wf.Budget.MaxCostUSD, status, err)
	}
	if !exec.wasCalled("converge") {
		t.Error("converge never ran: the verdict never got written, which is the exact failure " +
			"this budget is sized to prevent")
	}
}

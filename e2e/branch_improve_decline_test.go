package e2e

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The fixer's no-op terminal (issue #706). A merge-queue auto-heal launched
// the fixer on a GREEN pull request an unrelated flaky test had ejected. Its
// plan node concluded, unprompted, "no code issue in the diff; this is a
// re-queue, not a fix", and recorded as its own step 0 that a queue build was
// in flight and that pushing would cancel it — then its mission told it to
// rebase and force-push. It had no way to act on its own conclusion.
//
// So "there is nothing to fix" is now an OUTCOME: `declined` in the
// termination contract, verified by the deterministic decline_probe, ending
// the run typed DECLINED. The code is the whole point — it is what lets an
// unattended lane tell "the fixer declined" from "the fixer died" without
// either side naming the other.

// stubDeclineTail wires a campaign that declines and a probe with the given
// verdict, on top of the ordinary converging stubs.
func stubDeclineTail(exec *scenarioExecutor, honoured bool, reason string) {
	stubBranchCampaign(exec, &branchCampaignState{cleanBy: 1})
	stubBranchPlanRelay(exec)
	exec.on("campaign", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{
			"branch_clean": true, "commits_this_pass": 0, "issues_remaining": "",
			"needs_human": false, "human_note": "", "stopped_on_reserve": false,
			"summary":  "read the diff; the eject was an unrelated flaky test",
			"declined": true, "decline_reason": reason,
			"_tokens": 10,
		}, nil
	})
	exec.on("decline_probe", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"honoured": honoured, "reason": reason, "_tokens": 1}, nil
	})
}

// TestBranchImproveLoop_DeclineEndsTypedWithoutShipping: an EARNED decline
// ends the run on the typed code, and never pays for — nor reaches — any of
// the verify/ship tail. Reaching the push is the failure this exists to stop.
func TestBranchImproveLoop_DeclineEndsTypedWithoutShipping(t *testing.T) {
	wf := compileFixtureStubSafe(t, "branch-improve-loop/main.bot")
	exec := newScenarioExecutor()
	const reason = "the queue ejected this PR on an unrelated flaky test; the diff introduces no defect [verified: HEAD unmoved, tree clean]"
	stubDeclineTail(exec, true, reason)

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)
	err := eng.Run(context.Background(), "run-bil-declined", map[string]any{
		"plan_phase": "off", "push_branch": "feature", "pr_url": "https://forge/x/y/pull/1",
	})
	if err == nil {
		t.Fatal("Run: want an error (the decline routes to a fail node), got nil")
	}
	run, loadErr := s.LoadRun(context.Background(), "run-bil-declined")
	if loadErr != nil {
		t.Fatalf("LoadRun: %v", loadErr)
	}

	// The typed code is on the RUN, which is where every machine reads it —
	// `iterion runs list`, the studio, and the unattended lanes that must not
	// treat a refusal as a dead run to relaunch.
	if run.FailureCode != "DECLINED" {
		t.Errorf("run.failure_code = %q, want DECLINED — an untyped refusal reads as FAIL_NODE, "+
			"indistinguishable from every other fail node in the fleet", run.FailureCode)
	}
	// Terminal, not resumable: what the bot refused is the PREMISE of its
	// dispatch, so resuming re-runs the same answer.
	if run.Status != store.RunStatusFailed {
		t.Errorf("status = %s, want %s (a decline is not something a resume can change)", run.Status, store.RunStatusFailed)
	}
	if run.Error != reason {
		t.Errorf("run.error = %q, want the decline reason verbatim — the author reads this on the pull request", run.Error)
	}
	for _, node := range []string{"verify_probe", "verify_build", "verify_run", "review", "push_auth_probe", "push_back_tool", "publish_verdict"} {
		if exec.wasCalled(node) {
			t.Errorf("%s ran after a decline — the run must push nothing and pay for nothing", node)
		}
	}
}

// TestBranchImproveLoop_RefusedDeclineShipsAnyway is the other half, and the
// reason the probe is deterministic: a pass that DID change the repository
// cannot end the run terminal on "nothing to fix" — that would strand its own
// commits behind a failure. The refused decline falls through to the ordinary
// verify/ship tail.
func TestBranchImproveLoop_RefusedDeclineShipsAnyway(t *testing.T) {
	wf := compileFixtureStubSafe(t, "branch-improve-loop/main.bot")
	exec := newScenarioExecutor()
	stubDeclineTail(exec, false, "declined but HEAD moved from abc123456789 to def987654321: the pass committed work")

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)
	if err := eng.Run(context.Background(), "run-bil-decline-refused", map[string]any{"plan_phase": "off"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	run, err := s.LoadRun(context.Background(), "run-bil-decline-refused")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if run.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want %s — a refused decline must ship the work the pass actually did", run.Status, store.RunStatusFinished)
	}
	if !exec.wasCalled("verify_run") {
		t.Error("the deterministic gate never ran — a refused decline skipped the tail that lands the work")
	}
}

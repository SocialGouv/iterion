package e2e

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Campaign-bot plan-phase + precondition tests, driven through the engine
// with the deterministic scenario stub (no LLM, no shell). They pin the two
// contracts every campaign bot shares:
//
//   - `plan_review` gates ONLY the cross-model peer review: with it off (the
//     resolver's answer on every single-provider deployment) the plan is
//     still AUTHORED and handed to the campaign, stamped as unreviewed;
//     `plan_phase: off` is the separate, explicit way to skip planning;
//   - `workspace_probe`, the deterministic precondition at entry, fails the
//     run typed (WORKSPACE_NOT_A_REPO) before ANY LLM node runs.
//
// The probe's real shell body is exercised in bots/workspace_probe_test.go;
// here it is stubbed, like every other tool node in this package.

// stubWorkspaceProbeOK stubs the entry precondition as "a repository is
// there" — the baseline every campaign-bot scenario in this package runs on.
func stubWorkspaceProbeOK(exec *scenarioExecutor) {
	exec.on("workspace_probe", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"ok": true, "code": "", "reason": "git repository present", "_tokens": 1}, nil
	})
}

// stubPlanAuthor stubs the plan AUTHOR with a recognisable plan so a test
// can assert what reached the campaign.
func stubPlanAuthor(exec *scenarioExecutor) {
	exec.on("plan", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"plan": "slice A, slice B", "assumptions": "seam X exists", "risks": "none flagged",
			"_tokens": 10, "_cost_usd": 0.01}, nil
	})
}

// stubBranchPlanRelay stubs branch-improve-loop's deterministic plan-phase
// relays (both tool nodes): the scope probe, and the budget gate relaying
// the hand-off fields to the campaign within its share.
func stubBranchPlanRelay(exec *scenarioExecutor) {
	exec.on("plan_scope_probe", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"diff_stat": "1 file changed", "large": false, "started_epoch": 1, "_tokens": 1}, nil
	})
	exec.on("plan_budget_gate", func(in map[string]any) (map[string]any, error) {
		return map[string]any{
			"exhausted": false, "code": "", "reason": "",
			"plan": in["plan"], "plan_critique": in["plan_critique"], "plan_responses": in["plan_responses"],
			"plan_provenance": in["plan_provenance"],
			"_tokens":         1,
		}, nil
	})
}

// campaignBotCase names one campaign bot and wires its baseline stubs (the
// bot's own e2e helper — a green, first-pass-converging campaign).
type campaignBotCase struct {
	name    string
	fixture string
	stub    func(*scenarioExecutor)
}

var campaignBotCases = []campaignBotCase{
	{"feature-dev", "feature-dev/main.bot", func(e *scenarioExecutor) {
		stubFeatureDevCampaign(e, &featureDevState{completeBy: 1})
	}},
	{"whole-improve-loop", "whole-improve-loop/main.bot", func(e *scenarioExecutor) {
		stubCampaignSweep(e, &campaignState{axisCompleteBy: 1})
	}},
	{"branch-improve-loop", "branch-improve-loop/main.bot", func(e *scenarioExecutor) {
		stubBranchCampaign(e, &branchCampaignState{cleanBy: 1})
		stubBranchPlanRelay(e)
	}},
	{"e2e-coverage", "e2e-coverage/main.bot", func(e *scenarioExecutor) {
		stubEndyCampaign(e, &endyState{completeBy: 1})
	}},
}

// captureCampaignInputs wraps the already-registered campaign stub so the
// test sees every input map the campaign received.
func captureCampaignInputs(exec *scenarioExecutor, into *[]map[string]any) {
	inner := exec.handlers["campaign"]
	exec.on("campaign", func(in map[string]any) (map[string]any, error) {
		*into = append(*into, in)
		return inner(in)
	})
}

// TestCampaignPlanPhase_ReviewOffStillAuthorsThePlan: plan_review=off (what
// ResolvePlanReview answers on a single-provider host, and what an operator
// sets to save the peer pass) must skip ONLY the peer review. The plan is
// authored, no peer or revise node runs, and the campaign receives the
// author's plan with a provenance stamp saying no peer looked at it.
func TestCampaignPlanPhase_ReviewOffStillAuthorsThePlan(t *testing.T) {
	for _, tc := range campaignBotCases {
		t.Run(tc.name, func(t *testing.T) {
			wf := compileFixtureStubSafe(t, tc.fixture)
			exec := newScenarioExecutor()
			tc.stub(exec)
			stubPlanAuthor(exec)
			var ins []map[string]any
			captureCampaignInputs(exec, &ins)

			s := tmpStore(t)
			eng := runtime.New(wf, s, exec)
			runID := "run-plan-review-off-" + tc.name
			if err := eng.Run(context.Background(), runID, map[string]any{"plan_review": "off"}); err != nil {
				t.Fatalf("Run: %v", err)
			}
			run, err := s.LoadRun(context.Background(), runID)
			if err != nil {
				t.Fatalf("LoadRun: %v", err)
			}
			if run.Status != store.RunStatusFinished {
				t.Fatalf("status = %s, want %s", run.Status, store.RunStatusFinished)
			}
			if got := exec.callCount("plan"); got != 1 {
				t.Errorf("plan called %d times, want 1 — plan_review=off must not skip the AUTHORED plan", got)
			}
			for _, id := range []string{"plan_review", "plan_revise"} {
				if exec.wasCalled(id) {
					t.Errorf("%s ran with plan_review=off — only the peer review is off", id)
				}
			}
			if len(ins) == 0 {
				t.Fatal("campaign never ran")
			}
			first := ins[0]
			if first["plan"] != "slice A, slice B" {
				t.Errorf("campaign received plan %q, want the AUTHOR's plan", first["plan"])
			}
			prov, _ := first["plan_provenance"].(string)
			if !strings.Contains(strings.ToLower(prov), "not peer-reviewed") {
				t.Errorf("campaign received plan_provenance %q, want a stamp saying the plan is NOT peer-reviewed", prov)
			}
			for _, f := range []string{"fail_log", "plan_critique", "plan_responses"} {
				requireEmpty(t, first, f)
			}
		})
	}
}

// TestCampaignPlanPhase_OffSkipsPlanningExplicitly: `plan_phase: off` is the
// explicit opt-out — no plan node, the campaign starts immediately with
// every hand-off field present-and-empty (the plan-in-stride shape).
func TestCampaignPlanPhase_OffSkipsPlanningExplicitly(t *testing.T) {
	for _, tc := range campaignBotCases {
		t.Run(tc.name, func(t *testing.T) {
			wf := compileFixtureStubSafe(t, tc.fixture)
			exec := newScenarioExecutor()
			tc.stub(exec)
			stubPlanAuthor(exec)
			var ins []map[string]any
			captureCampaignInputs(exec, &ins)

			s := tmpStore(t)
			eng := runtime.New(wf, s, exec)
			runID := "run-plan-phase-off-" + tc.name
			if err := eng.Run(context.Background(), runID, map[string]any{"plan_phase": "off"}); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if exec.wasCalled("plan") {
				t.Error("plan ran with plan_phase=off — the explicit opt-out must skip planning")
			}
			if len(ins) == 0 {
				t.Fatal("campaign never ran")
			}
			for _, f := range []string{"fail_log", "plan", "plan_critique", "plan_responses", "plan_provenance"} {
				requireEmpty(t, ins[0], f)
			}
		})
	}
}

// TestCampaignPrecondition_NotARepoFailsBeforeAnyLLM: the entry probe
// refusing (workspace_dir absent / not a repository) must end the run
// FAILED through the `-> fail` edge with NO LLM node started — the typed
// code WORKSPACE_NOT_A_REPO rides the probe's own persisted output (the DSL
// `-> fail` terminal carries no custom code, see pkg/runtime/engine_exec.go).
func TestCampaignPrecondition_NotARepoFailsBeforeAnyLLM(t *testing.T) {
	for _, tc := range campaignBotCases {
		t.Run(tc.name, func(t *testing.T) {
			runProbeRefusal(t, tc, "workspace_dir '/tmp/nowhere' does not exist")
		})
	}
}

// TestCampaignPrecondition_UnreachableBaseFailsTyped: the second silent
// shape — a repository whose base ref is not reachable from HEAD — takes the
// same typed exit, on the bot whose mission is anchored on base_ref.
func TestCampaignPrecondition_UnreachableBaseFailsTyped(t *testing.T) {
	for _, tc := range campaignBotCases {
		if tc.name != "branch-improve-loop" {
			continue
		}
		runProbeRefusal(t, tc, "base_ref 'develop' does not resolve in /tmp/ws (tried develop, refs/remotes/origin/develop)")
	}
}

func runProbeRefusal(t *testing.T, tc campaignBotCase, reason string) {
	t.Helper()
	wf := compileFixtureStubSafe(t, tc.fixture)
	exec := newScenarioExecutor()
	tc.stub(exec)
	exec.on("workspace_probe", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"ok": false, "code": "WORKSPACE_NOT_A_REPO", "reason": reason, "_tokens": 1}, nil
	})
	for _, llm := range []string{"plan", "plan_review", "plan_revise", "campaign"} {
		id := llm
		exec.on(id, func(_ map[string]any) (map[string]any, error) {
			t.Errorf("%s ran — the precondition must refuse BEFORE any LLM node spends", id)
			return map[string]any{"_tokens": 1}, nil
		})
	}

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)
	runID := "run-probe-refusal-" + tc.name + "-" + strings.Fields(reason)[0]
	if err := eng.Run(context.Background(), runID, nil); err == nil {
		t.Fatal("Run: want an error (workspace_probe -> fail), got nil")
	}
	run, err := s.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if run.Status != store.RunStatusFailed {
		t.Fatalf("status = %s, want %s (workspace_probe -> fail when not ok)", run.Status, store.RunStatusFailed)
	}
	if exec.callCount("workspace_probe") != 1 {
		t.Errorf("workspace_probe called %d times, want 1", exec.callCount("workspace_probe"))
	}
	// The typed verdict is the probe's own persisted OUTPUT: the engine
	// checkpoints every node's output after it completes (before the edge
	// to `fail` is taken), so it survives in run.json for `iterion inspect`
	// and the studio — no `publish:` artifact is needed for that.
	if run.Checkpoint == nil {
		t.Fatal("the failed run carries no checkpoint — the probe's verdict was not persisted")
	}
	probeOut := run.Checkpoint.Outputs["workspace_probe"]
	if probeOut["code"] != "WORKSPACE_NOT_A_REPO" {
		t.Errorf("persisted probe output code = %v, want WORKSPACE_NOT_A_REPO (the typed verdict must be visible on the node output)", probeOut["code"])
	}
	if got, _ := probeOut["reason"].(string); got != reason {
		t.Errorf("persisted probe reason = %q, want %q", got, reason)
	}
	events, err := s.LoadEvents(context.Background(), runID)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	// Only the probe and the `fail` terminal complete: the run stops right
	// after the refusal, before any LLM node starts.
	finished := eventNodeIDs(events, store.EventNodeFinished)
	if len(finished) == 0 || finished[0] != "workspace_probe" {
		t.Errorf("node_finished events = %v, want workspace_probe first", finished)
	}
	for _, id := range finished {
		if id != "workspace_probe" && id != "fail" {
			t.Errorf("node %q finished after the refusal — nothing but the probe and the fail terminal may run (events: %v)", id, finished)
		}
	}
}

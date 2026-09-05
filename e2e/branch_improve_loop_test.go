package e2e

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// branchCampaignState models the v2 branch-improve-loop "one agent, its natural
// flow" shape: a single adaptive `campaign` agent reviews the branch diff,
// improves it, and commits each fix in stride (git is the durable state — there
// is no chunk/worklist file the e2e stub has to model any more), then a
// deterministic build/test gate re-checks the tree and the continuation loop
// runs another pass until the campaign reports branch_clean AND the tree is
// green. The stub drives the ONE property the control flow depends on: the
// gate's converged decision (green ∧ branch_clean) keyed on the campaign's
// termination output and the verify verdict.
type branchCampaignState struct {
	// cleanBy: the campaign reports branch_clean=true on/after this pass
	// (1-based). Earlier passes report false with commits_this_pass committed.
	cleanBy      int
	pass         int      // how many campaign passes have run
	failLogsSeen []string // the input.fail_log the campaign saw on each pass
}

// stubBranchCampaign registers the baseline stubs for a green continuation:
// campaign (the adaptive agent), the verify gate (verify_probe regenerate →
// verify_build → verify_run green), and the MR tail (forge_auth_probe
// credential-present → finalize_mr). Individual tests override a node
// afterward (later .on wins) to exercise a red verify pass or the MR path.
// toStr is shared with whole_improve_loop_test.go (same package).
func stubBranchCampaign(exec *scenarioExecutor, st *branchCampaignState) {
	// The entry precondition passes and the plan phase (on by default)
	// authors a triage that rides the deterministic relays (scope probe,
	// budget gate) to the campaign; plan_review is unresolved (auto →
	// off) in this harness, so the peer never runs.
	stubWorkspaceProbeOK(exec)
	stubPlanAuthor(exec)
	stubBranchPlanRelay(exec)
	exec.on("campaign", func(in map[string]any) (map[string]any, error) {
		st.pass++
		fl := ""
		if raw, ok := in["fail_log"]; ok {
			fl = strings.TrimSpace(toStr(raw))
		}
		st.failLogsSeen = append(st.failLogsSeen, fl)
		clean := st.pass >= st.cleanBy
		commits := 2
		remaining := "more branch issues to fix"
		if clean {
			commits = 1
			remaining = ""
		}
		return map[string]any{
			"branch_clean":      clean,
			"commits_this_pass": commits,
			"issues_remaining":  remaining,
			"needs_human":       false,
			"human_note":        "",
			"summary":           "reviewed + improved the branch diff this pass",
			"_tokens":           10,
		}, nil
	})
	// fresh=false routes every pass through verify_build → verify_run, the
	// flow the per-test call-count assertions are written against.
	exec.on("verify_probe", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"fresh": false, "reason": "no verify.sh yet", "_tokens": 1}, nil
	})
	exec.on("verify_build", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"prepared": true, "summary": "verify.sh written", "_tokens": 1}, nil
	})
	exec.on("verify_run", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"passed": true, "skipped": false, "exit_code": 0, "log_tail": "", "_tokens": 1}, nil
	})
	// The in-loop adversarial review stubs clean by default so a
	// green+complete pass converges; the review-blocks path is exercised
	// by its own test.
	exec.on("review", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"clean": true, "findings": "", "_tokens": 1}, nil
	})
	// available=true keeps the opt-in MR path reachable (finalize_mr fires
	// when open_mr=true); the probe only runs behind the open_mr gate.
	exec.on("forge_auth_probe", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"available": true, "reason": "env:GH_TOKEN", "_tokens": 1}, nil
	})
	exec.on("finalize_mr", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{
			"opened": true, "url": "https://forge/mr/1", "branch": "iterion/improve/x",
			"back_linked": false, "skipped_reason": "", "summary": "opened", "_tokens": 5,
		}, nil
	})
}

// TestBranchImproveLoop_ContinuesUntilClean is the canonical v2 flow: the
// campaign reports branch_clean=false on pass 1 (issues remain) then true on
// pass 2, the deterministic gate is green both times, and the continuation loop
// runs a second campaign pass before converging. Asserts:
//   - campaign runs exactly twice (a second pass because pass 1 was not clean);
//   - the verify gate runs each pass (verify_run == 2);
//   - the run finishes (converged → mr_gate → done, open_mr default false).
func TestBranchImproveLoop_ContinuesUntilClean(t *testing.T) {
	wf := compileFixtureStubSafe(t, "branch-improve-loop/main.bot")
	exec := newScenarioExecutor()
	st := &branchCampaignState{cleanBy: 2}
	stubBranchCampaign(exec, st)

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)
	if err := eng.Run(context.Background(), "run-bil-continue", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	run, err := s.LoadRun(context.Background(), "run-bil-continue")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if run.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want %s", run.Status, store.RunStatusFinished)
	}
	if got := exec.callCount("campaign"); got != 2 {
		t.Errorf("campaign called %d times, want 2 (pass 1 not clean → continuation → pass 2 clean)", got)
	}
	if got := exec.callCount("verify_run"); got != 2 {
		t.Errorf("verify_run called %d times, want 2 (the deterministic gate runs each pass)", got)
	}
	if exec.wasCalled("finalize_mr") {
		t.Errorf("finalize_mr fired with open_mr=false — the MR path must be opt-in")
	}
	// Both continuation passes carried an empty fail_log (both gates were green;
	// the loop back was driven by branch_clean=false, not a red build).
	for i, fl := range st.failLogsSeen {
		if fl != "" {
			t.Errorf("pass %d saw fail_log %q, want empty (green gate → continuation, not a red-fix)", i+1, fl)
		}
	}
}

// TestBranchImproveLoop_ConvergesFirstPass pins the fast path: the campaign
// reports branch_clean=true on the first pass and the gate is green, so the run
// converges immediately — one campaign pass, straight to done.
func TestBranchImproveLoop_ConvergesFirstPass(t *testing.T) {
	wf := compileFixtureStubSafe(t, "branch-improve-loop/main.bot")
	exec := newScenarioExecutor()
	st := &branchCampaignState{cleanBy: 1}
	stubBranchCampaign(exec, st)

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)
	if err := eng.Run(context.Background(), "run-bil-first", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	run, err := s.LoadRun(context.Background(), "run-bil-first")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if run.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want %s", run.Status, store.RunStatusFinished)
	}
	if got := exec.callCount("campaign"); got != 1 {
		t.Errorf("campaign called %d times, want 1 (branch_clean + green on the first pass converges immediately)", got)
	}
}

// TestBranchImproveLoop_RedVerifyRoutesBackToCampaign pins the tight
// real-feedback loop: the campaign reports branch_clean=true every pass, but the
// deterministic gate is RED on pass 1 (the campaign broke the build) and green
// on pass 2. The gate's converged = green ∧ branch_clean, so a red build must
// route back to campaign WITH the failure log so it fixes what it broke, even
// though the agent claimed the branch clean. Asserts:
//   - campaign runs twice (the red gate forced a fix pass despite branch_clean);
//   - the second campaign pass received the real build-failure log as input;
//   - the run finishes once the gate goes green.
func TestBranchImproveLoop_RedVerifyRoutesBackToCampaign(t *testing.T) {
	wf := compileFixtureStubSafe(t, "branch-improve-loop/main.bot")
	exec := newScenarioExecutor()
	st := &branchCampaignState{cleanBy: 1} // the agent claims clean every pass
	stubBranchCampaign(exec, st)
	// Override the gate: red on the first run, green thereafter.
	verifyCalls := 0
	exec.on("verify_run", func(_ map[string]any) (map[string]any, error) {
		verifyCalls++
		if verifyCalls == 1 {
			return map[string]any{
				"passed": false, "skipped": false, "exit_code": 1,
				"log_tail": "stub build failure: undefined symbol Foo", "_tokens": 1,
			}, nil
		}
		return map[string]any{"passed": true, "skipped": false, "exit_code": 0, "log_tail": "", "_tokens": 1}, nil
	})

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)
	if err := eng.Run(context.Background(), "run-bil-red", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	run, err := s.LoadRun(context.Background(), "run-bil-red")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if run.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want %s", run.Status, store.RunStatusFinished)
	}
	if got := exec.callCount("campaign"); got != 2 {
		t.Errorf("campaign called %d times, want 2 (a red gate forces a fix pass even though the agent claimed branch_clean)", got)
	}
	if len(st.failLogsSeen) < 2 || !strings.Contains(st.failLogsSeen[1], "stub build failure") {
		t.Errorf("second campaign pass fail_log = %v, want it to carry the real build failure so the agent fixes what it broke", st.failLogsSeen)
	}
}

// TestBranchImproveLoop_MRPathOnConverge pins the opt-in MR path: with
// open_mr=true a converged run opens the MR/PR (finalize_mr) before finishing.
func TestBranchImproveLoop_MRPathOnConverge(t *testing.T) {
	wf := compileFixtureStubSafe(t, "branch-improve-loop/main.bot")
	exec := newScenarioExecutor()
	st := &branchCampaignState{cleanBy: 1}
	stubBranchCampaign(exec, st)

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)
	inputs := map[string]any{"open_mr": true}
	if err := eng.Run(context.Background(), "run-bil-mr", inputs); err != nil {
		t.Fatalf("Run: %v", err)
	}
	run, err := s.LoadRun(context.Background(), "run-bil-mr")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if run.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want %s", run.Status, store.RunStatusFinished)
	}
	if !exec.wasCalled("finalize_mr") {
		t.Errorf("finalize_mr did not fire with open_mr=true — the converged series must open an MR")
	}
}

// TestBranchImproveLoop_Structural pins the v2 IR shape: the campaign entry, the
// adaptive campaign/verify_build agents, the deterministic verify_run tool +
// gate/mr_gate computes, and the ABSENCE of every retired v1 node (the chunked
// cross-family review-fix-commit machinery). Drift here (e.g. reintroducing a
// blocking upfront plan_chunks, or a reviewer node) breaks the mechanism
// silently.
func TestBranchImproveLoop_Structural(t *testing.T) {
	wf := compileFixtureStubSafe(t, "branch-improve-loop/main.bot")

	// Entry is the deterministic workspace precondition (a tool node, no
	// LLM — it also checks base_ref is reachable), then the plan-phase
	// gate (ADR-091) — on by default, its off branch (plan_phase=off)
	// being the v2 "start working immediately" shape.
	if wf.Entry != "workspace_probe" {
		t.Errorf("workflow entry = %q, want %q (the deterministic precondition ahead of any LLM node)", wf.Entry, "workspace_probe")
	}
	if _, ok := wf.Nodes["workspace_probe"].(*ir.ToolNode); !ok {
		t.Errorf("workspace_probe is %T, want *ir.ToolNode (deterministic precondition)", wf.Nodes["workspace_probe"])
	}
	if _, ok := wf.Nodes["plan_topology"].(*ir.ComputeNode); !ok {
		t.Errorf("plan_topology is %T, want *ir.ComputeNode (deterministic gate)", wf.Nodes["plan_topology"])
	}
	// Adaptive agents: campaign + verify_build.
	for _, id := range []string{"campaign", "verify_build"} {
		node, ok := wf.Nodes[id]
		if !ok {
			t.Fatalf("workflow missing expected agent node %q", id)
		}
		if _, ok := node.(*ir.AgentNode); !ok {
			t.Errorf("node %q is %T, want *ir.AgentNode (adaptive)", id, node)
		}
	}
	// Deterministic (no-LLM): verify_run is a tool node; gate/mr_gate are computes.
	if node, ok := wf.Nodes["verify_run"]; !ok {
		t.Errorf("workflow missing expected tool node %q", "verify_run")
	} else if _, ok := node.(*ir.ToolNode); !ok {
		t.Errorf("node %q is %T, want *ir.ToolNode (deterministic gate)", "verify_run", node)
	}
	for _, id := range []string{"gate", "mr_gate"} {
		node, ok := wf.Nodes[id]
		if !ok {
			t.Fatalf("workflow missing expected compute node %q", id)
		}
		if _, ok := node.(*ir.ComputeNode); !ok {
			t.Errorf("node %q is %T, want *ir.ComputeNode (deterministic)", id, node)
		}
	}
	// The retired v1 chunked cross-family review-fix-commit machinery must be gone.
	for _, id := range []string{
		"plan_chunks", "alt", "reviewer_claude", "reviewer_gpt", "streak_check",
		"fix_claude", "fix_gpt", "prepare_commit", "commit_changes",
	} {
		if _, ok := wf.Nodes[id]; ok {
			t.Errorf("retired v1 node %q is still present — v2 is one campaign agent, not the chunked review-fix assembly line", id)
		}
	}
}

// ─── Plan-phase budget guard (native:695) ───────────────────────────────
//
// A production run on a +3870/-273 diff spent the planning chain (plan ->
// plan_review -> plan_revise) for 150 min / $8.59 and never reached
// `campaign`. plan_budget_gate is the deterministic choke point that must
// fail the run — typed, before campaign starts — the moment planning
// crosses its share of the run's budget, and must otherwise let campaign
// proceed with the (possibly revised) plan carried forward exactly as
// before the guard existed.
//
// The guard is REAL here, not stubbed: it is a compute reading the run's
// own `run.cost_usd` / `run.elapsed_seconds` against the caps in force, so
// a scenario steers it the only honest way — by choosing what the stubbed
// LLM nodes BILL against the bot's shipped `budget:` block (max_duration
// 2h30m, max_cost_usd 75; plan_budget_ratio 0.3, so the plan phase's cost
// share is $22.50). The COST axis is what these tests drive: it is a
// figure the stub sets exactly, whereas tripping the duration axis would
// need the run to genuinely take minutes.
//
// plan_scope_probe, plan, plan_review and plan_revise are stubbed
// (plan_scope_probe is a tool node whose real body shells out to git —
// this harness never exercises a tool node's real subprocess). plan_gate
// and plan_budget_gate are REAL computes.

const (
	// planCostShareUSD is plan_budget_ratio (0.3) x the bot's shipped
	// max_cost_usd (75) — the share the plan phase may spend.
	planCostShareUSD = 22.5
	// planOverShareUSD / planUnderShareUSD are per-LLM-node prices; three
	// nodes run, so the phase spends 3x. $37.50 crosses the share while
	// staying under the engine's own 90%-of-cap hard limit ($67.50), and
	// $7.50 stays well inside it.
	planOverShareUSD  = 12.50
	planUnderShareUSD = 2.50
)

// planBudgetGateStubs wires a full (non-skipped, non-large-diff) plan
// phase: plan -> plan_review -> plan_gate -> plan_revise, each of the three
// LLM nodes billing planCostUSD, so the run's own cost at the guard is
// 3*planCostUSD.
func planBudgetGateStubs(exec *scenarioExecutor, planCostUSD float64) {
	stubWorkspaceProbeOK(exec)
	stubBranchPlanRelay(exec)
	exec.on("plan", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"plan": "fix the seam", "assumptions": "small blast radius", "risks": "none flagged",
			"_tokens": 10, "_cost_usd": planCostUSD}, nil
	})
	exec.on("plan_review", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"concerns": "the seam looks fragile", "suggestions": "add a regression test", "blocking": false,
			"_tokens": 10, "_cost_usd": planCostUSD}, nil
	})
	exec.on("plan_revise", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"plan": "fix the seam, revised", "review_responses": "accepted: added the test",
			"_tokens": 10, "_cost_usd": planCostUSD}, nil
	})
}

// stubConvergingCampaignTail wires a campaign that converges on its first
// pass plus the deterministic loop tail behind it, capturing every input
// the campaign received.
func stubConvergingCampaignTail(exec *scenarioExecutor, into *[]map[string]any) {
	exec.on("campaign", func(in map[string]any) (map[string]any, error) {
		*into = append(*into, in)
		return map[string]any{"branch_clean": true, "commits_this_pass": 1, "issues_remaining": "",
			"needs_human": false, "human_note": "", "summary": "converged", "_tokens": 10, "_cost_usd": 0.01}, nil
	})
	exec.on("verify_probe", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"fresh": false, "reason": "no verify.sh yet", "_tokens": 1}, nil
	})
	exec.on("verify_build", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"prepared": true, "summary": "verify.sh written", "_tokens": 1}, nil
	})
	exec.on("verify_run", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"passed": true, "skipped": false, "exit_code": 0, "log_tail": "", "_tokens": 1}, nil
	})
	exec.on("review", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"clean": true, "findings": "", "_tokens": 1}, nil
	})
}

// gateOutput returns plan_budget_gate's persisted output, failing the test
// when the guard never ran.
func gateOutput(t *testing.T, run *store.Run) map[string]any {
	t.Helper()
	if run.Checkpoint == nil {
		t.Fatal("the run carries no checkpoint — plan_budget_gate's verdict was not persisted")
	}
	out := run.Checkpoint.Outputs["plan_budget_gate"]
	if out == nil {
		t.Fatal("plan_budget_gate never ran")
	}
	return out
}

// TestBranchImproveLoop_PlanBudgetExhaustedFailsBeforeCampaign is scenario
// (a): the plan phase bills 3 * $12.50 = $37.50 against a $22.50 share, so
// the guard refuses. The run must end FAILED_RESUMABLE, stamped
// PLAN_BUDGET_EXHAUSTED on the RUN (not merely on a node output), with the
// checkpoint anchored on the GUARD — so `iterion resume --max-cost-usd …`
// re-evaluates it — and `campaign` never started (the delivery reserve
// this guard exists to protect).
func TestBranchImproveLoop_PlanBudgetExhaustedFailsBeforeCampaign(t *testing.T) {
	wf := compileFixtureStubSafe(t, "branch-improve-loop/main.bot")
	exec := newScenarioExecutor()
	planBudgetGateStubs(exec, planOverShareUSD)
	// campaign must never run; fail loudly if the graph reaches it anyway.
	exec.on("campaign", func(_ map[string]any) (map[string]any, error) {
		t.Error("campaign ran — the plan-budget guard must fail the run BEFORE campaign starts")
		return map[string]any{"branch_clean": true, "commits_this_pass": 0, "issues_remaining": "",
			"needs_human": false, "human_note": "", "summary": "", "_tokens": 1}, nil
	})

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)
	err := eng.Run(context.Background(), "run-bil-plan-budget-fail", map[string]any{"plan_review": "on"})
	if err == nil {
		t.Fatal("Run: want an error (the plan budget guard routes to plan_exhausted), got nil")
	}
	run, loadErr := s.LoadRun(context.Background(), "run-bil-plan-budget-fail")
	if loadErr != nil {
		t.Fatalf("LoadRun: %v", loadErr)
	}
	if exec.wasCalled("campaign") {
		t.Error("campaign was called — the plan-budget guard did not stop the run before campaign")
	}

	// The refusal is on the RUN, which is where every machine reads it.
	if run.FailureCode != "PLAN_BUDGET_EXHAUSTED" {
		t.Errorf("run.failure_code = %q, want PLAN_BUDGET_EXHAUSTED — the bot's refusal is indistinguishable from any other fail node", run.FailureCode)
	}
	// resumable: the cure is "widen the caps and carry on", never "re-pay
	// the plan phase this run already completed".
	if run.Status != store.RunStatusFailedResumable {
		t.Fatalf("status = %s, want %s", run.Status, store.RunStatusFailedResumable)
	}
	if run.Checkpoint == nil || run.Checkpoint.NodeID != "plan_budget_gate" {
		t.Fatalf("checkpoint anchored on %v, want the GUARD plan_budget_gate — anchored on the fail node, a resume re-enters the refusal and the raised cap changes nothing",
			run.Checkpoint)
	}
	// And it names the figures, not just the code: a message that only
	// restates PLAN_BUDGET_EXHAUSTED tells the operator nothing about how
	// much to widen by.
	for _, want := range []string{"37.5", "22.5"} {
		if !strings.Contains(run.Error, want) {
			t.Errorf("run.error does not name %s (spent vs share): %q", want, run.Error)
		}
	}

	gate := gateOutput(t, run)
	if gate["exhausted"] != true {
		t.Errorf("plan_budget_gate.exhausted = %v, want true", gate["exhausted"])
	}
	if gate["over_cost"] != true {
		t.Errorf("plan_budget_gate.over_cost = %v, want true", gate["over_cost"])
	}
	if got := toFloat(gate["spent_usd"]); got != 3*planOverShareUSD {
		t.Errorf("plan_budget_gate.spent_usd = %v, want %v (the RUN's own cost at the guard)", gate["spent_usd"], 3*planOverShareUSD)
	}
	if got := toFloat(gate["cost_share_usd"]); got != planCostShareUSD {
		t.Errorf("plan_budget_gate.cost_share_usd = %v, want %v (plan_budget_ratio x the cap in force)", gate["cost_share_usd"], planCostShareUSD)
	}
}

// TestBranchImproveLoop_PlanBudgetWithinShareRunsCampaign is scenario (b):
// the plan phase bills 3 * $2.50 = $7.50, well inside its $22.50 share, so
// the guard is silent — campaign runs normally, receiving the
// plan/critique/responses exactly as the pre-guard edges handed them off.
func TestBranchImproveLoop_PlanBudgetWithinShareRunsCampaign(t *testing.T) {
	wf := compileFixtureStubSafe(t, "branch-improve-loop/main.bot")
	exec := newScenarioExecutor()
	planBudgetGateStubs(exec, planUnderShareUSD)
	var campaignIns []map[string]any
	stubConvergingCampaignTail(exec, &campaignIns)

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)
	if err := eng.Run(context.Background(), "run-bil-plan-budget-ok", map[string]any{"plan_review": "on"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	run, err := s.LoadRun(context.Background(), "run-bil-plan-budget-ok")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if run.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want %s (within-share plan phase must not block delivery)", run.Status, store.RunStatusFinished)
	}
	gate := gateOutput(t, run)
	if gate["exhausted"] != false {
		t.Errorf("plan_budget_gate.exhausted = %v, want false at %v of a %v share", gate["exhausted"], gate["spent_usd"], gate["cost_share_usd"])
	}
	if len(campaignIns) != 1 {
		t.Fatalf("campaign called %d times, want 1 (converges on the first pass)", len(campaignIns))
	}
	if campaignIns[0]["plan"] != "fix the seam, revised" {
		t.Errorf("campaign received plan %q, want the REVISED plan (plan_revise ran: peer served, diff not large)", campaignIns[0]["plan"])
	}
	if campaignIns[0]["plan_critique"] != "the seam looks fragile" {
		t.Errorf("campaign received plan_critique %q", campaignIns[0]["plan_critique"])
	}
	if campaignIns[0]["plan_responses"] != "accepted: added the test" {
		t.Errorf("campaign received plan_responses %q", campaignIns[0]["plan_responses"])
	}
}

// TestBranchImproveLoop_PlanBudgetFollowsTheCapInForce is the reason the
// two mirror vars had to go. The plan phase bills exactly what scenario
// (a) billed — $37.50 — but the run was re-budgeted to a $200 cap, which
// puts the phase's share at $60. The guard must follow the cap IN FORCE
// and let the campaign through.
//
// A mirror var (`budget_max_cost_usd = 75`, kept in sync by hand) could
// not: `iterion run --max-cost-usd 200` never reached it, so the guard
// went on refusing against a literal nobody had updated.
func TestBranchImproveLoop_PlanBudgetFollowsTheCapInForce(t *testing.T) {
	wf := compileFixtureStubSafe(t, "branch-improve-loop/main.bot")
	if wf.Budget == nil {
		t.Fatal("branch-improve-loop declares no budget: block — the guard has no cap to read")
	}
	wf.Budget.MaxCostUSD = 200 // what `iterion run --max-cost-usd 200` resolves to

	exec := newScenarioExecutor()
	planBudgetGateStubs(exec, planOverShareUSD)
	var campaignIns []map[string]any
	stubConvergingCampaignTail(exec, &campaignIns)

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)
	if err := eng.Run(context.Background(), "run-bil-plan-budget-recap", map[string]any{"plan_review": "on"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	run, err := s.LoadRun(context.Background(), "run-bil-plan-budget-recap")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	gate := gateOutput(t, run)
	if got := toFloat(gate["cost_share_usd"]); got != 60 {
		t.Errorf("plan_budget_gate.cost_share_usd = %v, want 60 (0.3 x the RE-BUDGETED cap) — the guard is reading a literal, not run.max_cost_usd", gate["cost_share_usd"])
	}
	if gate["exhausted"] != false {
		t.Fatalf("the guard refused $%v against a $%v share — the raised cap did not reach it", gate["spent_usd"], gate["cost_share_usd"])
	}
	if len(campaignIns) != 1 {
		t.Fatalf("campaign called %d times, want 1", len(campaignIns))
	}
}

// TestBranchImproveLoop_PlanBudgetUnboundedNeverTrips pins the documented
// convention a phase guard is easiest to get wrong: a `max_*` of 0 means
// UNBOUNDED on that axis, never "no allowance left". Without the `> 0`
// guard on each comparison, an uncapped run refuses on its first pass —
// every spend is "over" a share of zero.
func TestBranchImproveLoop_PlanBudgetUnboundedNeverTrips(t *testing.T) {
	wf := compileFixtureStubSafe(t, "branch-improve-loop/main.bot")
	if wf.Budget == nil {
		t.Fatal("branch-improve-loop declares no budget: block")
	}
	wf.Budget.MaxCostUSD = 0   // `--max-cost-usd 0` / no cost cap
	wf.Budget.MaxDuration = "" // no duration cap

	exec := newScenarioExecutor()
	planBudgetGateStubs(exec, planOverShareUSD)
	var campaignIns []map[string]any
	stubConvergingCampaignTail(exec, &campaignIns)

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)
	if err := eng.Run(context.Background(), "run-bil-plan-budget-uncapped", map[string]any{"plan_review": "on"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	run, err := s.LoadRun(context.Background(), "run-bil-plan-budget-uncapped")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	gate := gateOutput(t, run)
	if gate["exhausted"] != false {
		t.Fatalf("the guard refused an UNCAPPED run (cost share %v, duration share %v) — a cap of 0 is unbounded, not exhausted",
			gate["cost_share_usd"], gate["duration_share_seconds"])
	}
	if gate["over_cost"] != false || gate["over_duration"] != false {
		t.Errorf("over_cost = %v / over_duration = %v on an uncapped run, want false", gate["over_cost"], gate["over_duration"])
	}
	if len(campaignIns) != 1 {
		t.Fatalf("campaign called %d times, want 1 — an uncapped run must still deliver", len(campaignIns))
	}
}

// TestBranchImproveLoop_PlanBudgetResumeRunsTheCampaign is the promise the
// `resumable: true` on `plan_exhausted` makes, exercised on the SHIPPED
// bot rather than a fixture: the operator widens the caps and resumes, and
// the campaign starts on the plan the run already paid for.
//
// The engine anchors the checkpoint on the GUARD, so the resume re-executes
// plan_budget_gate — which means the guard has to be re-runnable and its
// input has to rebuild from the edge that was selected. The readout is the
// node-start counts: the guard twice, the fail node once, campaign once.
func TestBranchImproveLoop_PlanBudgetResumeRunsTheCampaign(t *testing.T) {
	wf := compileFixtureStubSafe(t, "branch-improve-loop/main.bot")
	s := tmpStore(t)
	const runID = "run-bil-plan-budget-resume"

	exec := newScenarioExecutor()
	planBudgetGateStubs(exec, planOverShareUSD)
	if err := runtime.New(wf, s, exec).Run(context.Background(), runID, map[string]any{"plan_review": "on"}); err == nil {
		t.Fatal("first pass succeeded; $37.50 must cross the $22.50 share")
	}
	run, err := s.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if run.Status != store.RunStatusFailedResumable {
		t.Fatalf("status = %s (%s), want failed_resumable", run.Status, run.Error)
	}

	// What the message asks for: raise the caps, resume. The plan phase's
	// spend is restored with the checkpoint, so the verdict flips only
	// because the CAP moved.
	wf.Budget.MaxCostUSD = 200
	resumeExec := newScenarioExecutor()
	planBudgetGateStubs(resumeExec, planOverShareUSD)
	var campaignIns []map[string]any
	stubConvergingCampaignTail(resumeExec, &campaignIns)
	if err := runtime.New(wf, s, resumeExec).Resume(context.Background(), runID, nil); err != nil {
		t.Fatalf("resume failed: %v", err)
	}

	run, err = s.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if run.Status != store.RunStatusFinished {
		t.Fatalf("status = %s (%s), want finished after widening the cap", run.Status, run.Error)
	}
	if run.FailureCode != "" {
		t.Errorf("failure_code = %q, want cleared by the successful resume", run.FailureCode)
	}
	if len(campaignIns) != 1 {
		t.Fatalf("campaign ran %d times, want 1 — the resume did not get past the guard", len(campaignIns))
	}
	if campaignIns[0]["plan"] != "fix the seam, revised" {
		t.Errorf("campaign received plan %q — the resume lost the plan this run already paid for", campaignIns[0]["plan"])
	}

	events, err := s.LoadEvents(context.Background(), runID)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	starts := map[string]int{}
	for _, e := range events {
		if e.Type == store.EventNodeStarted {
			starts[e.NodeID]++
		}
	}
	if starts["plan_budget_gate"] != 2 {
		t.Errorf("the guard ran %d times, want 2 (once per pass) — the resume did not re-evaluate it", starts["plan_budget_gate"])
	}
	if starts["plan_exhausted"] != 1 {
		t.Errorf("the fail node ran %d times, want 1 — the resume re-entered the refusal instead of the guard", starts["plan_exhausted"])
	}
	// The expensive half must NOT be re-paid: the plan chain ran on the
	// first pass only, which is the whole reason the refusal is resumable.
	for _, id := range []string{"plan", "plan_review", "plan_revise"} {
		if starts[id] != 1 {
			t.Errorf("%s ran %d times, want 1 — the resume re-paid the plan phase the guard exists to protect", id, starts[id])
		}
	}
}

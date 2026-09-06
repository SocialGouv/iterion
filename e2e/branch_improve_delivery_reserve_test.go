package e2e

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Billy's delivery reserve (issue #705). Three production runs died at the
// duration cap within two seconds of each other — 9001s/9000s, 2h30m01s/2h30m,
// 9001.86s/9000s — and NONE of them had pushed a commit: the campaign spent
// the entire window and the delivery tail (the build/test gate, the in-loop
// review, the push onto the pull request, the verdict comment and the
// merge-gate status) was never scheduled. Landing inside the last two seconds
// of the window three times is not luck; it is a window sized for analysis
// with nothing left for delivery.
//
// The reserve is deterministic and read from the caps IN FORCE
// (`run.max_duration_seconds`, after any `--max-duration` override), exactly
// like `plan_budget_ratio` — a literal mirroring the `budget:` block drifts
// from it in silence, which is the mistake `plan_budget_gate` already paid for.
//
//	reserve = min( max(floor_minutes x 60, cap x ratio), cap / 2 )
//
// deliveryReserve mirrors the shipped defaults so a test failure names the
// arithmetic rather than a magic number.
const (
	deliveryReserveRatio  = 0.15
	deliveryReserveFloorS = 600.0
)

// reserveOutput reads the delivery_reserve compute's persisted verdict.
func reserveOutput(t *testing.T, run *store.Run) map[string]any {
	t.Helper()
	if run.Checkpoint == nil {
		t.Fatal("the run carries no checkpoint — delivery_reserve's verdict was not persisted")
	}
	out := run.Checkpoint.Outputs["delivery_reserve"]
	if out == nil {
		t.Fatal("delivery_reserve never ran — the campaign was handed no working window")
	}
	return out
}

// runReserveScenario drives one converging pass with the plan phase off (the
// reserve is independent of it) and returns the run.
func runReserveScenario(t *testing.T, id string, tweak func(*ir.Workflow)) *store.Run {
	t.Helper()
	wf := compileFixtureStubSafe(t, "branch-improve-loop/main.bot")
	if wf.Budget == nil {
		t.Fatal("branch-improve-loop declares no budget: block — the reserve has no cap to carve from")
	}
	if tweak != nil {
		tweak(wf)
	}
	exec := newScenarioExecutor()
	stubBranchCampaign(exec, &branchCampaignState{cleanBy: 1})
	stubBranchPlanRelay(exec)

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)
	if err := eng.Run(context.Background(), id, map[string]any{"plan_phase": "off"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	run, err := s.LoadRun(context.Background(), id)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if run.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want %s", run.Status, store.RunStatusFinished)
	}
	return run
}

// TestBranchImproveLoop_DeliveryReserveOnTheShippedCap pins the arithmetic on
// the bot's own budget: the ratio dominates the floor, so the campaign is told
// to stop with 15% of the run still unspent.
func TestBranchImproveLoop_DeliveryReserveOnTheShippedCap(t *testing.T) {
	run := runReserveScenario(t, "run-bil-reserve-default", nil)
	res := reserveOutput(t, run)

	cap := toFloat(res["cap_seconds"])
	if cap <= 0 {
		t.Fatalf("cap_seconds = %v — the reserve must read the duration cap in force", res["cap_seconds"])
	}
	wantReserve := cap * deliveryReserveRatio
	if wantReserve < deliveryReserveFloorS {
		wantReserve = deliveryReserveFloorS
	}
	if got := toFloat(res["reserve_seconds"]); got != wantReserve {
		t.Errorf("reserve_seconds = %v, want %v (max(floor %vs, %v%% of the %vs cap))",
			got, wantReserve, deliveryReserveFloorS, deliveryReserveRatio*100, cap)
	}
	if got := toFloat(res["work_deadline_seconds"]); got != cap-wantReserve {
		t.Errorf("work_deadline_seconds = %v, want %v (cap minus the reserve)", got, cap-wantReserve)
	}
	// The tail measured on this repo is ~16 min (verify ~10, in-loop review
	// ~5, push + verdict ~1). A reserve under that ships nothing again.
	if toFloat(res["reserve_seconds"]) < 16*60 {
		t.Errorf("reserve_seconds = %v — under the ~16 min delivery tail measured on this repo, the tail is back to being unschedulable",
			res["reserve_seconds"])
	}
}

// TestBranchImproveLoop_DeliveryReserveFloorHoldsOnASmallCap: on a short run
// the proportional slice is smaller than the tail's absolute cost, so the
// floor takes over — and the clamp still leaves the campaign half the run.
func TestBranchImproveLoop_DeliveryReserveFloorHoldsOnASmallCap(t *testing.T) {
	run := runReserveScenario(t, "run-bil-reserve-small", func(wf *ir.Workflow) {
		wf.Budget.MaxDuration = "40m" // what `iterion run --max-duration 40m` resolves to
	})
	res := reserveOutput(t, run)
	if got := toFloat(res["cap_seconds"]); got != 2400 {
		t.Fatalf("cap_seconds = %v, want 2400 (the cap IN FORCE, not the budget: literal)", got)
	}
	// 15% of 40m is 6 min, under the 10 min floor → the floor wins.
	if got := toFloat(res["reserve_seconds"]); got != 600 {
		t.Errorf("reserve_seconds = %v, want 600 (the floor, above the 360s proportional slice)", got)
	}
	if got := toFloat(res["work_deadline_seconds"]); got != 1800 {
		t.Errorf("work_deadline_seconds = %v, want 1800", got)
	}
}

// TestBranchImproveLoop_DeliveryReserveClampLeavesHalfTheRun: a cap so short
// that the floor would eat it must still leave the campaign half the window —
// a reserve is worth nothing if there is no work to deliver.
func TestBranchImproveLoop_DeliveryReserveClampLeavesHalfTheRun(t *testing.T) {
	run := runReserveScenario(t, "run-bil-reserve-clamp", func(wf *ir.Workflow) {
		wf.Budget.MaxDuration = "10m"
	})
	res := reserveOutput(t, run)
	if got := toFloat(res["reserve_seconds"]); got != 300 {
		t.Errorf("reserve_seconds = %v, want 300 (the floor clamped to half a 600s cap)", got)
	}
	if got := toFloat(res["work_deadline_seconds"]); got != 300 {
		t.Errorf("work_deadline_seconds = %v, want 300", got)
	}
}

// TestBranchImproveLoop_DeliveryReserveUnboundedIsZero: a cap of 0 means
// UNBOUNDED on that axis, never "no allowance left" — the same guard-of-the-
// guard plan_budget_gate carries. An unbudgeted run must not be told to stop
// working immediately.
func TestBranchImproveLoop_DeliveryReserveUnboundedIsZero(t *testing.T) {
	run := runReserveScenario(t, "run-bil-reserve-unbounded", func(wf *ir.Workflow) {
		wf.Budget.MaxDuration = ""
	})
	res := reserveOutput(t, run)
	if got := toFloat(res["cap_seconds"]); got != 0 {
		t.Fatalf("cap_seconds = %v, want 0 on an unbounded run", got)
	}
	if got := toFloat(res["reserve_seconds"]); got != 0 {
		t.Errorf("reserve_seconds = %v, want 0 — nothing bounds the run, so nothing is held back", got)
	}
	if got := toFloat(res["work_deadline_seconds"]); got != 0 {
		t.Errorf("work_deadline_seconds = %v, want 0 (read as unbounded by the contract, not as stop now)", got)
	}
}

// A deployment may cap Billy below its own 3h: the cloud runner clamps
// wf.Budget to ITERION_CLOUD_MAX_DURATION before the engine builds its budget
// tracker (pkg/runner/loop.go applyCloudBudgetCeiling → ir.Budget.ClampToCeiling,
// then runtime newSharedBudget(e.workflow.Budget)). A clamp also sets
// CapImposed, which REFUSES the budget exit grace — so on a clamped pod the
// reserve is the only thing left standing between a long campaign and a run
// that delivers nothing.
//
// It must therefore follow the EFFECTIVE cap, not the DSL literal. This drives
// the real clamp, not a hand-set field.
func TestBranchImproveLoop_DeliveryReserveFollowsAPlatformClamp(t *testing.T) {
	run := runReserveScenario(t, "run-bil-reserve-clamped", func(wf *ir.Workflow) {
		// Exactly what ITERION_CLOUD_MAX_DURATION=2h30m does on the pod.
		wf.Budget.ClampToCeiling(&ir.Budget{MaxDuration: "2h30m"})
		if !wf.Budget.CapImposed {
			t.Fatal("the clamp did not mark the cap imposed — the fixture no longer reproduces the pod")
		}
	})
	res := reserveOutput(t, run)
	if got := toFloat(res["cap_seconds"]); got != 9000 {
		t.Fatalf("cap_seconds = %v, want 9000 — the reserve read the DSL literal, not the clamped cap "+
			"the run actually has, so it would hold back 27 min of a window that is only 2h30m", got)
	}
	// 15% of 2h30m = 22.5 min, still over the ~16 min tail measured here.
	if got := toFloat(res["reserve_seconds"]); got != 1350 {
		t.Errorf("reserve_seconds = %v, want 1350 (15%% of the clamped cap)", got)
	}
	if got := toFloat(res["work_deadline_seconds"]); got != 7650 {
		t.Errorf("work_deadline_seconds = %v, want 7650", got)
	}
}

// A pass that stopped because its WINDOW closed must leave the loop. Nothing
// routed it out before: `branch_clean` is false (issues remain, honestly), so
// `gate -> campaign as continuation_loop` re-entered and the next pass's tail
// — verify ~10 min plus the in-loop review ~5 — was spent out of the very
// 27-minute reserve the stop existed to protect. The engine's loop budget
// guard is a backstop, not a router: it declines a back-edge it cannot FUND,
// which is a different question from one that must not be taken, and it is
// switchable off (`--loop-budget-guard off`).
//
// The bot answers it itself, and `converged` is deliberately NOT reused: it is
// what publish_verdict posts as the merge-gate's build verdict, so widening it
// would green a gate on a pass that ran out of time.
func TestBranchImproveLoop_WindowStoppedPassLeavesTheLoop(t *testing.T) {
	wf := compileFixtureStubSafe(t, "branch-improve-loop/main.bot")
	exec := newScenarioExecutor()
	st := &branchCampaignState{cleanBy: 99} // never reports clean by itself
	stubBranchCampaign(exec, st)
	stubBranchPlanRelay(exec)
	exec.on("campaign", func(_ map[string]any) (map[string]any, error) {
		st.pass++
		return map[string]any{
			"branch_clean": false, "commits_this_pass": 3,
			"issues_remaining": "two more sites of the same class",
			"needs_human":      false, "human_note": "",
			"summary":  "fixed three issues; the working window closed",
			"declined": false, "decline_reason": "",
			// The honest report the contract asks for.
			"stopped_on_reserve": true,
			"_tokens":            10,
		}, nil
	})

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)
	if err := eng.Run(context.Background(), "run-bil-window-stop", map[string]any{"plan_phase": "off"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	run, err := s.LoadRun(context.Background(), "run-bil-window-stop")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if run.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want %s", run.Status, store.RunStatusFinished)
	}
	if got := exec.callCount("campaign"); got != 1 {
		t.Errorf("campaign ran %d times: a pass that stopped on the window re-entered the loop, and the "+
			"next pass's verify+review tail comes out of the delivery reserve", got)
	}
	// It leaves through the ship tail, so the work it banked is delivered —
	// this is an early EXIT, not an abort. mr_gate is a compute, executed by
	// the ENGINE and never by the scenario stub, so its persisted output is
	// the oracle (wasCalled would be false however the run went).
	if run.Checkpoint == nil || run.Checkpoint.Outputs["mr_gate"] == nil {
		t.Error("the run did not reach the ship tail — a window stop must deliver what is banked")
	}
	// And it is not dressed up as success: the merge-gate verdict the bot
	// posts still reports the branch as unfinished.
	if run.Checkpoint != nil {
		if g := run.Checkpoint.Outputs["gate"]; g != nil && g["converged"] == true {
			t.Error("gate.converged is true on a pass that ran out of window — publish_verdict would " +
				"post that as the build verdict on the merge gate")
		}
	}
}

// TestBranchImproveLoop_DeliveryReserveIsTheOnlyWayIn is the structural half:
// every path that STARTS a campaign pass goes through the reserve, so no entry
// can hand the agent an unbounded window by omission. The continuation loop is
// exempt by construction — it re-enters the same campaign whose window the
// reserve already published, and outputs persist.
func TestBranchImproveLoop_DeliveryReserveIsTheOnlyWayIn(t *testing.T) {
	wf := compileFixtureStubSafe(t, "branch-improve-loop/main.bot")
	if _, ok := wf.Nodes["delivery_reserve"].(*ir.ComputeNode); !ok {
		t.Fatalf("delivery_reserve is %T, want *ir.ComputeNode (deterministic arithmetic, no LLM and no shell)", wf.Nodes["delivery_reserve"])
	}
	for _, e := range wf.Edges {
		if e.To != "campaign" {
			continue
		}
		// delivery_deadline is the second half of the same choke point: the
		// reserve computes the window, the stamp turns it into an instant the
		// agent can read, and campaign is entered from the stamp.
		if e.From != "delivery_deadline" && e.From != "gate" {
			t.Errorf("%s -> campaign bypasses delivery_reserve: that entry starts a campaign with no working window, "+
				"which is how a run spends its whole cap and delivers nothing", e.From)
		}
	}
}

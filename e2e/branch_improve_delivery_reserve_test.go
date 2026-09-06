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
		if e.From != "delivery_reserve" && e.From != "gate" {
			t.Errorf("%s -> campaign bypasses delivery_reserve: that entry starts a campaign with no working window, "+
				"which is how a run spends its whole cap and delivers nothing", e.From)
		}
	}
}

package bots

import (
	"os"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// The delivery reserve only works if the campaign is TOLD about it. The
// arithmetic is deterministic (e2e/branch_improve_delivery_reserve_test.go
// pins the numbers against the caps in force); this pins the hand-off — the
// three figures the agent needs to know when to stop, the contract clause
// that tells it what to do at that instant, and the honesty field that makes
// "I stopped because the window closed" distinguishable from "the branch is
// clean" on the run itself.
//
// Without the hand-off the reserve is a number nobody reads, and the failure
// it exists for (three runs dead at the cap, nothing pushed — #705) comes
// straight back.
func TestBillyCampaignIsToldItsWorkingWindow(t *testing.T) {
	const bot = "branch-improve-loop/main.bot"
	raw, err := os.ReadFile(bot)
	if err != nil {
		t.Fatalf("read %s: %v", bot, err)
	}
	src := string(raw)

	// The task prompt carries the deterministic instant AND the run's own
	// clock. The instant is a constant of the run, so it survives the
	// continuation loop's back-edge; `run.elapsed_seconds` resolves at the
	// moment the node executes, so it is fresh on every pass. Either alone
	// is useless: a deadline with no clock cannot be reached, and a clock
	// with no deadline names no instant.
	for _, ref := range []string{
		"{{outputs.delivery_reserve.work_deadline_seconds}}",
		"{{run.elapsed_seconds}}",
		"{{run.max_duration_seconds}}",
	} {
		if !strings.Contains(src, ref) {
			t.Errorf("%s: the campaign task does not carry %s — the agent cannot tell when its window closes", bot, ref)
		}
	}

	if !strings.Contains(src, "DELIVERY RESERVE") {
		t.Errorf("%s: no DELIVERY RESERVE clause in the campaign contract — the figures are handed over with no rule attached", bot)
	}

	wf := compilePlanPhaseBot(t, "branch-improve-loop")
	out, ok := wf.Schemas["campaign_output"]
	if !ok {
		t.Fatal("no campaign_output schema")
	}
	found := false
	for _, f := range out.Fields {
		if f.Name == "stopped_on_reserve" {
			found = true
		}
	}
	if !found {
		t.Errorf("campaign_output declares no stopped_on_reserve — a pass that ran out of window is then indistinguishable "+
			"from one that ran out of issues, and the operator never learns the cap is too small for the branch (%s)", bot)
	}
}

// The reserve reads the caps IN FORCE, never a literal mirroring the budget:
// block. That mistake was already paid for once by plan_budget_gate (a run
// re-budgeted with --max-cost-usd kept being refused against a stale 75), and
// nothing stops it recurring in a sibling guard.
func TestDeliveryReserveReadsTheCapInForce(t *testing.T) {
	wf := compilePlanPhaseBot(t, "branch-improve-loop")
	node, ok := wf.Nodes["delivery_reserve"].(*ir.ComputeNode)
	if !ok {
		t.Fatalf("delivery_reserve is %T, want *ir.ComputeNode", wf.Nodes["delivery_reserve"])
	}
	exprs := map[string]string{}
	for _, ex := range node.Exprs {
		exprs[ex.Key] = ex.Raw
	}
	for _, key := range []string{"cap_seconds", "reserve_seconds", "work_deadline_seconds"} {
		raw, has := exprs[key]
		if !has {
			t.Fatalf("delivery_reserve has no %s expression", key)
		}
		if !strings.Contains(raw, "run.max_duration_seconds") {
			t.Errorf("delivery_reserve.%s = %q does not read run.max_duration_seconds", key, raw)
		}
	}
	// A cap of 0 is UNBOUNDED on that axis, never "no allowance left":
	// without the guard, an unbudgeted run is told to stop at second 0.
	for _, key := range []string{"reserve_seconds", "work_deadline_seconds"} {
		if !strings.Contains(exprs[key], "run.max_duration_seconds > 0") {
			t.Errorf("delivery_reserve.%s = %q does not guard on a positive cap — an unbudgeted run would be told its window is already shut",
				key, exprs[key])
		}
	}
	// Both knobs are overridable per run: a hardcoded reserve is exactly the
	// artificial limit this repo's philosophy calls a defect.
	for _, v := range []string{"delivery_reserve_ratio", "delivery_reserve_floor_minutes"} {
		if _, ok := wf.Vars[v]; !ok {
			t.Errorf("var %s is missing — the reserve must be re-sizable per run without editing the .bot", v)
		}
		if !strings.Contains(exprs["reserve_seconds"], "vars."+v) {
			t.Errorf("delivery_reserve.reserve_seconds does not read vars.%s — the var is dead config", v)
		}
	}
}

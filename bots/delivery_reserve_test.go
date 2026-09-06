package bots

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

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

	// The deadline the agent is handed must be one it can OBSERVE. Run-relative
	// seconds are resolved once, when the prompt is built, and nothing refreshes
	// them mid-pass: `{{run.elapsed_seconds}}` is a template ref, not a tool, and
	// the agent is never told the run's wall-clock start, so it cannot convert
	// `date` into run time. Asking it to "check the remaining time again" against
	// a frozen number is the unaided sense of elapsed time that let three runs
	// spend 2h30m without noticing — the very failure the reserve exists to
	// remove (Rce6c53).
	//
	// So the contract hands over an ABSOLUTE UTC instant and tells the agent to
	// compare it with `date -u`, which it can re-read at will. Being absolute is
	// also what makes it survive the continuation loop: the instant is the same
	// on pass 3 as on pass 1, so the node need not re-run.
	if !strings.Contains(src, "{{outputs.delivery_deadline.deadline_at}}") {
		t.Errorf("%s: the campaign task carries no absolute deadline instant — a run-relative figure "+
			"is not observable mid-pass, so there is nothing for the agent to re-check", bot)
	}
	if !strings.Contains(src, "date -u") {
		t.Errorf("%s: the contract never names the clock the agent should read (`date -u`), so the "+
			"deadline is a number it cannot compare anything against", bot)
	}
	wf0 := compilePlanPhaseBot(t, "branch-improve-loop")
	stamp, ok := wf0.Nodes["delivery_deadline"].(*ir.ToolNode)
	if !ok {
		t.Fatalf("delivery_deadline is %T, want *ir.ToolNode — the instant has to be produced by a "+
			"deterministic node; `run.*` carries no start time and a compute has no clock", wf0.Nodes["delivery_deadline"])
	}
	if strings.Contains(stamp.Command, "date -d") {
		t.Error("delivery_deadline uses `date -d`, which is GNU-only — the sibling probes use python3 for exactly this reason")
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

// The stamp is what the agent actually reads, and the e2e harness stubs every
// tool node — so its REAL body is exercised here, like the sibling probes.
// Two properties matter: the instant is UTC ISO-8601 (comparable with the
// `date -u` the contract tells the agent to run), and a zero window yields an
// EMPTY instant rather than a time in the past, which would read as "stop now"
// on a run that is simply unbounded.
func TestDeliveryDeadlineStampsAnInstantTheAgentCanRead(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	command := toolCommand(t, "branch-improve-loop/main.bot", "delivery_deadline")

	type stamp struct {
		DeadlineAt  string  `json:"deadline_at"`
		SecondsLeft float64 `json:"seconds_left"`
	}
	run := func(t *testing.T, left string) stamp {
		t.Helper()
		out, err := exec.Command("sh", "-c",
			strings.ReplaceAll(command, "{{input.work_seconds_left}}", shellQuote(left))).Output()
		if err != nil {
			t.Fatalf("delivery_deadline exited non-zero (%v): out=%q", err, out)
		}
		var s stamp
		if uerr := json.Unmarshal(out, &s); uerr != nil {
			t.Fatalf("output is not delivery_deadline_state JSON: %v (out %q)", uerr, out)
		}
		return s
	}

	t.Run("a live window becomes a future UTC instant", func(t *testing.T) {
		before := time.Now().UTC()
		got := run(t, "1620")
		at, err := time.Parse("2006-01-02T15:04:05Z", got.DeadlineAt)
		if err != nil {
			t.Fatalf("deadline_at %q is not the UTC ISO-8601 the contract tells the agent to compare "+
				"with `date -u`: %v", got.DeadlineAt, err)
		}
		delta := at.Sub(before)
		if delta < 1600*time.Second || delta > 1680*time.Second {
			t.Errorf("deadline_at is %v from now, want ~1620s — the stamp is the window, not an arbitrary time", delta)
		}
		if got.SecondsLeft != 1620 {
			t.Errorf("seconds_left = %v, want 1620", got.SecondsLeft)
		}
	})

	t.Run("no window at all yields no instant", func(t *testing.T) {
		for _, left := range []string{"0", "", "-42"} {
			got := run(t, left)
			if got.DeadlineAt != "" {
				t.Errorf("work_seconds_left=%q stamped %q — an unbounded run (or one whose window is "+
					"already spent) must not be handed a deadline in the past, which reads as stop now",
					left, got.DeadlineAt)
			}
		}
	})
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

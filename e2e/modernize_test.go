package e2e

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Modernize's plan_read is a python tool node that resolves the programme
// contract into ONE lot to work on (or a "nothing to do" verdict). It is
// STUBBED here exactly like every other tool node in this suite — the real
// body shells out to yq/git, and the two scenarios below are about the
// GRAPH decision work_gate makes from plan_read's verdict, not about the
// python parsing itself.
//
// native:670 — an EXPLICIT only_lot request naming a lot the contract
// already resolves as done/blocked/absent used to fall through to the same
// silent-green nothing_to_do path an unfiltered "pick next" scan uses when
// the whole programme is finished. The two are different outcomes: one
// means "there is genuinely nothing to do", the other means "the specific
// thing you asked for cannot be done" — and only the first may exit green.

// TestModernize_OnlyLotBlockedFailsTyped pins the fix: plan_read resolves
// only_lot to a BLOCKED lot (lot_not_actionable=true, lot_status="blocked",
// nothing_to_do=false per plan_read's not_actionable() contract) and
// work_gate must route to a typed fail BEFORE upgrade_campaign ever runs.
func TestModernize_OnlyLotBlockedFailsTyped(t *testing.T) {
	wf := compileFixtureStubSafe(t, "modernize/main.bot")
	exec := newScenarioExecutor()
	exec.on("plan_read", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{
			"nothing_to_do": false, "lot_id": "", "lot_title": "", "lot_intent": "",
			"exit_gate": "", "base_sha": "deadbeef", "refs_dir": "",
			"notice":             "only_lot 'lot-2' is not actionable: its status is 'blocked'.",
			"lot_not_actionable": true, "lot_status": "blocked",
			"_tokens": 1,
		}, nil
	})
	exec.on("upgrade_campaign", func(_ map[string]any) (map[string]any, error) {
		t.Error("upgrade_campaign ran — a non-actionable only_lot request must fail before any campaign work starts")
		return map[string]any{"work_remaining": "", "_tokens": 1}, nil
	})

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)
	runErr := eng.Run(context.Background(), "run-modernize-blocked", map[string]any{"only_lot": "lot-2"})
	if runErr == nil {
		t.Fatal("Run: want an error (work_gate routes a non-actionable only_lot to fail), got nil")
	}
	run, err := s.LoadRun(context.Background(), "run-modernize-blocked")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if run.Status != store.RunStatusFailed {
		t.Fatalf("status = %s, want %s (work_gate -> fail on lot_not_actionable)", run.Status, store.RunStatusFailed)
	}
	if exec.wasCalled("upgrade_campaign") {
		t.Error("upgrade_campaign was called — the guard did not stop the run first")
	}
	if run.Checkpoint == nil {
		t.Fatal("run has no checkpoint — cannot inspect work_gate's own verdict")
	}
	wg, ok := run.Checkpoint.Outputs["work_gate"]
	if !ok {
		t.Fatal("checkpoint carries no work_gate output")
	}
	if wg["lot_not_actionable"] != true {
		t.Errorf("work_gate.lot_not_actionable = %v, want true", wg["lot_not_actionable"])
	}
	if wg["lot_status"] != "blocked" {
		t.Errorf("work_gate.lot_status = %v, want %q", wg["lot_status"], "blocked")
	}
	if wg["nothing_to_do"] != false {
		t.Errorf("work_gate.nothing_to_do = %v, want false (never true alongside lot_not_actionable, or the done edge could win on declaration order)", wg["nothing_to_do"])
	}
}

// TestModernize_UnfilteredNothingToDoStaysGreen pins the untouched contract:
// an unfiltered "pick next" scan (only_lot empty) that finds the whole
// programme finished is a legitimate no-op — nothing_to_do=true,
// lot_not_actionable=false — and must still exit the run FINISHED, exactly
// as before native:670.
func TestModernize_UnfilteredNothingToDoStaysGreen(t *testing.T) {
	wf := compileFixtureStubSafe(t, "modernize/main.bot")
	exec := newScenarioExecutor()
	exec.on("plan_read", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{
			"nothing_to_do": true, "lot_id": "", "lot_title": "", "lot_intent": "",
			"exit_gate": "", "base_sha": "deadbeef", "refs_dir": "",
			"notice":             "every lot in the contract is done.",
			"lot_not_actionable": false, "lot_status": "",
			"_tokens": 1,
		}, nil
	})
	exec.on("upgrade_campaign", func(_ map[string]any) (map[string]any, error) {
		t.Error("upgrade_campaign ran — a finished programme must be a clean no-op")
		return map[string]any{"work_remaining": "", "_tokens": 1}, nil
	})

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)
	if err := eng.Run(context.Background(), "run-modernize-done", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	run, err := s.LoadRun(context.Background(), "run-modernize-done")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if run.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want %s (an unfiltered finished-programme scan is a clean no-op)", run.Status, store.RunStatusFinished)
	}
	if exec.wasCalled("upgrade_campaign") {
		t.Error("upgrade_campaign was called on a finished programme")
	}
}

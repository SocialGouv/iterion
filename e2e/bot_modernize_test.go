package e2e

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// modernizeStubs registers the deterministic shape of one modernize lot for
// the scenario executor: plan_read hands out lot L1, the campaign works it,
// lot_verify answers with whatever `verdict` returns for the pass, and
// mark_done records that it ran. Compute nodes (work_gate, lot_gate) are the
// engine's own; the subbot nodes are never reached (no invalid mutant, no
// pending extension).
func modernizeStubs(exec *scenarioExecutor, verdict func(pass int) map[string]any) {
	exec.on("plan_read", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{
			"nothing_to_do": false, "lot_id": "L1", "lot_title": "raise the build tool",
			"lot_intent": "bump it", "exit_gate": "true", "base_sha": "0123456789abcdef",
			"refs_dir": ".golden-master/refs", "notice": "", "_tokens": 1,
		}, nil
	})
	pass := 0
	exec.on("upgrade_campaign", func(_ map[string]any) (map[string]any, error) {
		pass++
		return map[string]any{"work_remaining": "", "_tokens": 10}, nil
	})
	exec.on("lot_verify", func(_ map[string]any) (map[string]any, error) {
		out := map[string]any{
			"gate_passed": false, "oracle_passed": false, "refs_untouched": false,
			"refs_changed": []any{}, "lot_blocked": false, "block_reason": "",
			"oracle_invalid": []any{}, "extension_pending": []any{},
			"done_self_written": false, "contract_rewritten": []any{},
			"gate_timed_out": false, "log_tail": "", "_tokens": 1,
		}
		for k, v := range verdict(pass) {
			out[k] = v
		}
		return out, nil
	})
	exec.on("mark_done", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"marked": true, "commit": "feedfacefeedface", "notice": "marked", "_tokens": 1}, nil
	})
}

func runModernize(t *testing.T, exec *scenarioExecutor, runID string) *store.Run {
	t.Helper()
	wf := compileFixtureStubSafe(t, "modernize/main.bot")
	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)
	_ = eng.Run(context.Background(), runID, nil) // the status is the verdict, read below
	run, err := s.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	return run
}

// TestModernize_ConvergedLotIsMarkedByTheGate: the green path — the
// conjunction holds, the gate writes `done` (mark_done), the run finishes.
func TestModernize_ConvergedLotIsMarkedByTheGate(t *testing.T) {
	exec := newScenarioExecutor()
	modernizeStubs(exec, func(int) map[string]any {
		return map[string]any{"gate_passed": true, "oracle_passed": true, "refs_untouched": true}
	})
	run := runModernize(t, exec, "run-mod-converged")
	if run.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want finished", run.Status)
	}
	if !exec.wasCalled("mark_done") {
		t.Fatal("mark_done never ran on a converged lot — `done` is the gate's word")
	}
	if got := exec.callCount("upgrade_campaign"); got != 1 {
		t.Fatalf("upgrade_campaign called %d times, want 1", got)
	}
}

// TestModernize_RefusedVerdictNeverEndsGreen: a worker that writes `done`
// into the contract (or rewrites it) is refused by lot_verify on every pass.
// The refusal goes back to the worker while repair_loop lasts — and when the
// loop is exhausted the run must NOT end `finished`: the forged `done` is
// still in the contract, a merge of a green run would land it, and the lot
// would vanish from the programme (an only_lot relaunch is refused as not
// actionable, a free relaunch skips it as landed). Review finding on the
// change that introduced the refusal: the unconditional `lot_gate -> done`
// exhaustion edge let it through — a term true by absence.
func TestModernize_RefusedVerdictNeverEndsGreen(t *testing.T) {
	for _, tc := range []struct {
		name    string
		refusal map[string]any
	}{
		{"worker-written done", map[string]any{"done_self_written": true, "log_tail": "the worker wrote the verdict"}},
		{"rewritten contract", map[string]any{"contract_rewritten": []any{"lot L1: intent changed"}, "log_tail": "rewritten"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exec := newScenarioExecutor()
			modernizeStubs(exec, func(int) map[string]any { return tc.refusal })
			run := runModernize(t, exec, "run-mod-refused")
			if run.Status == store.RunStatusFinished {
				t.Fatalf("a refusal that outlived repair_loop ended `finished` — the unproven `done` would land on a merge")
			}
			if exec.wasCalled("mark_done") {
				t.Fatal("mark_done ran on a refused verdict")
			}
			// The refusal goes back to the worker for every pass the loop
			// has (1 + max_passes=4), and the run fails only once the loop
			// is exhausted — never instead of it.
			if got := exec.callCount("upgrade_campaign"); got != 5 {
				t.Fatalf("upgrade_campaign called %d times, want 5: the refusal sent back for each of max_passes repair passes before the run fails", got)
			}
			if run.Status != store.RunStatusFailed {
				t.Fatalf("status = %s, want failed (the fail terminal, in words)", run.Status)
			}
		})
	}
}

// TestModernize_DeclaredBlockedStopsWithoutMarking: `blocked` is the worker's
// word and a STOP, never a success — the run ends without mark_done, on the
// first pass.
func TestModernize_DeclaredBlockedStopsWithoutMarking(t *testing.T) {
	exec := newScenarioExecutor()
	modernizeStubs(exec, func(int) map[string]any {
		return map[string]any{"lot_blocked": true, "block_reason": "needs a decision"}
	})
	run := runModernize(t, exec, "run-mod-blocked")
	if run.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want finished (a declared block is a stop, read in the tree)", run.Status)
	}
	if exec.wasCalled("mark_done") {
		t.Fatal("mark_done ran on a blocked lot")
	}
	if got := exec.callCount("upgrade_campaign"); got != 1 {
		t.Fatalf("upgrade_campaign called %d times, want 1 (no repair pass against a declared wall)", got)
	}
}

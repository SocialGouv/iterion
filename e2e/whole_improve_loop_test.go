package e2e

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// wilToInt coerces an edge-relayed numeric (which template substitution may
// deliver as an int, float64, or stringified number) to an int, defaulting
// to 0 for absent/unparseable values — matching how the real snapshot_chunk
// python seeds its state from an empty/literal STATE_IN.
func wilToInt(v interface{}) int {
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	case string:
		s := strings.TrimSpace(x)
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return int(f)
		}
	}
	return 0
}

// stubSnapshotChunk registers a stateful stub for the deterministic
// snapshot_chunk tool node. The e2e executor cannot run that node's real
// embedded-python chunker, so this models the one property the loop's
// convergence math depends on: the cross-pass pass-through of the ADR-055
// per-unit tuple {cursor, unit_streak, unit_passes, units_done}. It echoes
// the edge-fed incoming_* values back as the snapshot's persisted_* / cursor
// outputs — exactly as the real tool seeds them from
// .whole_improve_loop.state — and reports a SINGLE unit (num_chunks=1) so a
// unit converges on two consecutive clean reviews and its convergence is also
// the run's stop (units_done+1 >= 1). Without this stub the snapshot outputs
// are nil and streak_check's `persisted_unit_streak + 1` / `cursor + 1` exprs
// fail.
func stubSnapshotChunk(exec *scenarioExecutor) {
	exec.on("snapshot_chunk", func(in map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"chunk_content": "// stub chunk source",
			"files":         "stub.go",
			"chunk_label":   "stub",
			"chunk_index":   0,
			"num_chunks":    1,
			"loop_max":      3, // small fixed bound for the test; real node emits 2*num_chunks+max_passes
			"chunked":       false,
			// ADR-055 2b coherent-unit fields: the stub models a single WHOLE
			// unit (never partial), so the partial-view guard is inert.
			"partial":               false,
			"unit_label":            "stub",
			"unit_part":             1,
			"unit_parts":            1,
			"file_count":            1,
			"chunk_tokens":          10,
			"total_files":           1,
			"total_tokens":          10,
			"skipped_oversize":      0,
			"persisted_unit_streak": wilToInt(in["incoming_unit_streak"]),
			"persisted_unit_passes": wilToInt(in["incoming_unit_passes"]),
			"persisted_units_done":  wilToInt(in["incoming_units_done"]),
			"cursor":                wilToInt(in["incoming_cursor"]),
			"_tokens":               1,
		}, nil
	})
}

// stubVerifyGate registers stubs for BOTH deterministic build/test gates the
// loop runs:
//   - the FINALIZATION gate (streak_check -> verify_build -> verify_run ->
//     commit_changes -> done), reached after `stop`, and
//   - the PER-UNIT gate (ADR-055 2a: streak_check -> unit_verify_build ->
//     unit_verify_run -> commit_unit), reached each time a NON-last unit
//     converges, so every incremental commit is build+test green on its own.
//
// The e2e executor cannot run the real verify nodes (they adapt to the repo's
// tooling and execute .whole_improve_loop.verify.sh), so this models a GREEN
// gate on both: verify_run / unit_verify_run report passed=true so the run
// routes ... -> commit_changes / commit_unit when passed. Without the per-unit
// stub, a converged non-last unit would exhaust unit_verify_loop(3) and skip
// WITHOUT committing (never reaching commit_unit). Tests that never converge
// (loop-exhaustion scenarios) reach neither gate and don't need this.
func stubVerifyGate(exec *scenarioExecutor) {
	green := func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"passed":   true,
			"log_tail": "",
			"_tokens":  1,
		}, nil
	}
	exec.on("verify_run", green)
	exec.on("unit_verify_run", green)
}

// TestWholeImproveLoop_HappyPath simulates the canonical "two
// consecutive cross-family approvals" scenario:
//
//	iter1: claude approves   → streak_check.stop = false (no previous)
//	iter2: gpt approves      → streak_check.stop = true  → done
func TestWholeImproveLoop_HappyPath(t *testing.T) {
	wf := compileFixtureStubSafe(t, "whole-improve-loop/main.bot")
	exec := newScenarioExecutor()
	stubSnapshotChunk(exec)
	stubVerifyGate(exec)

	exec.on("reviewer_claude", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"approved":  true,
			"family":    "claude",
			"blockers":  []string{},
			"fix_plan":  "",
			"_tokens":   100,
			"_cost_usd": 0.01,
		}, nil
	})
	exec.on("reviewer_gpt", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"approved":  true,
			"family":    "gpt",
			"blockers":  []string{},
			"fix_plan":  "",
			"_tokens":   100,
			"_cost_usd": 0.01,
		}, nil
	})

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)
	// inline context_mode: the stub reviewers can't open real files, so run in
	// inline mode (ADR-045 v0.5.0) where the explore-engagement guard is bypassed
	// and the convergence math these scenarios assert on runs as it did
	// pre-explore-mode. (Explore-mode engagement is exercised by live runs.)
	if err := eng.Run(context.Background(), "run-vibe-happy", map[string]interface{}{"context_mode": "inline"}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	run, err := s.LoadRun(context.Background(), "run-vibe-happy")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want %s", run.Status, store.RunStatusFinished)
	}
	if exec.callCount("reviewer_claude") != 1 {
		t.Errorf("expected reviewer_claude once, got %d", exec.callCount("reviewer_claude"))
	}
	if exec.callCount("reviewer_gpt") != 1 {
		t.Errorf("expected reviewer_gpt once, got %d", exec.callCount("reviewer_gpt"))
	}
	if exec.wasCalled("fix_claude") {
		t.Errorf("fix_claude should not have been called on happy path")
	}
}

// TestWholeImproveLoop_FixThenApprove simulates a scenario where
// the first reviewer rejects, fix runs, then two cross-family approvals
// trigger the stop:
//
//	iter1: claude rejects → fix_claude
//	iter2: gpt approves   → streak_check.stop = false (previous was a fix)
//	iter3: claude approves → streak_check.stop = true → done
//
// Note: iter1 sets loop.previous_output to claude's rejection. After
// fix_claude runs (no loop edge crossing), iter2 traverses the gpt
// reviewer→streak_check edge which snapshots gpt's verdict. Then iter3
// claude→streak_check sees previous=gpt's approval, current=claude's
// approval, families differ → stop.
func TestWholeImproveLoop_FixThenApprove(t *testing.T) {
	wf := compileFixtureStubSafe(t, "whole-improve-loop/main.bot")
	exec := newScenarioExecutor()
	stubSnapshotChunk(exec)
	stubVerifyGate(exec)

	claudeCalls := 0
	exec.on("reviewer_claude", func(_ map[string]interface{}) (map[string]interface{}, error) {
		claudeCalls++
		approved := claudeCalls > 1 // first call rejects, subsequent approve
		blockers := []string{"missing test"}
		if approved {
			blockers = []string{}
		}
		return map[string]interface{}{
			"approved": approved,
			"family":   "claude",
			"blockers": blockers,
			"fix_plan": "add tests",
			"_tokens":  100,
		}, nil
	})
	exec.on("reviewer_gpt", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"approved": true,
			"family":   "gpt",
			"blockers": []string{},
			"fix_plan": "",
			"_tokens":  100,
		}, nil
	})
	exec.on("fix_claude", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"applied": true,
			"summary": "added tests",
			// ADR-055 2c: the fixer records its intent, threaded forward to the
			// next cross-family reviewer of this unit (prior_change_rationale).
			"change_rationale": "added a missing unit test for the error path; no behaviour change",
			"_tokens":          200,
		}, nil
	})

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)
	if err := eng.Run(context.Background(), "run-vibe-fix", map[string]interface{}{"context_mode": "inline"}); err != nil { // inline: stub reviewers can't open real files (ADR-045)
		t.Fatalf("Run: %v", err)
	}

	run, err := s.LoadRun(context.Background(), "run-vibe-fix")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want %s", run.Status, store.RunStatusFinished)
	}
	if exec.callCount("fix_claude") != 1 {
		t.Errorf("expected fix_claude once, got %d", exec.callCount("fix_claude"))
	}
	if exec.callCount("reviewer_claude") < 2 {
		t.Errorf("expected reviewer_claude at least twice, got %d", exec.callCount("reviewer_claude"))
	}
}

// TestWholeImproveLoop_LoopExhausted simulates a run where the families
// never assemble a clean streak (claude always approves, gpt always
// rejects with a blocker, so the streak resets every gpt pass). The
// review_loop bound kicks in and execution falls through to the
// `reviewer -> fail` terminal: exhausting the loop WITHOUT streak_check
// ever firing `stop` is a non-convergence, which the bot reports as
// `failed` (not a silent `finished`) so a dispatcher re-runs rather than
// marking the ticket clean. See main.bot "Loop exhaustion fallbacks".
func TestWholeImproveLoop_LoopExhausted(t *testing.T) {
	wf := compileFixtureStubSafe(t, "whole-improve-loop/main.bot")
	exec := newScenarioExecutor()
	stubSnapshotChunk(exec)

	exec.on("reviewer_claude", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"approved": true, "family": "claude",
			"blockers": []string{}, "fix_plan": "", "_tokens": 50,
		}, nil
	})
	exec.on("reviewer_gpt", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"approved": false, "family": "gpt",
			"blockers": []string{"flaky test"}, "fix_plan": "stabilize", "_tokens": 50,
		}, nil
	})
	exec.on("fix_claude", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"applied": true, "summary": "tried", "_tokens": 100,
		}, nil
	})

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)
	// Reaching the `fail` terminal surfaces as a Run error — that IS the
	// non-convergence signal, not a test failure.
	if err := eng.Run(context.Background(), "run-vibe-exhausted", nil); err == nil {
		t.Fatalf("expected Run to error on review_loop exhaustion (non-convergence), got nil")
	}

	run, err := s.LoadRun(context.Background(), "run-vibe-exhausted")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Status != store.RunStatusFailed {
		t.Fatalf("status = %s, want %s (review_loop exhaustion routes to fail — no silent success)",
			run.Status, store.RunStatusFailed)
	}
}

// TestWholeImproveLoop_EventTrace establishes the event-coherence
// baseline: a happy-path run must persist a complete trace covering
// node lifecycle, edge selection, and round-robin reviewer dispatch.
// This is the regression net for any future refactor of the engine's
// event emission — a missing event type surfaces here first.
func TestWholeImproveLoop_EventTrace(t *testing.T) {
	wf := compileFixtureStubSafe(t, "whole-improve-loop/main.bot")
	exec := newScenarioExecutor()
	stubSnapshotChunk(exec)
	stubVerifyGate(exec)
	exec.on("reviewer_claude", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"approved": true, "family": "claude",
			"blockers": []interface{}{}, "fix_plan": "", "_tokens": 100,
		}, nil
	})
	exec.on("reviewer_gpt", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"approved": true, "family": "gpt",
			"blockers": []interface{}{}, "fix_plan": "", "_tokens": 100,
		}, nil
	})

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)
	if err := eng.Run(context.Background(), "run-vibe-events", map[string]interface{}{"context_mode": "inline"}); err != nil { // inline: stub reviewers can't open real files (ADR-045)
		t.Fatalf("Run: %v", err)
	}

	events, err := s.LoadEvents(context.Background(), "run-vibe-events")
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}

	if !hasEvent(events, store.EventRunStarted) {
		t.Errorf("missing run_started event")
	}
	if !hasEvent(events, store.EventRunFinished) {
		t.Errorf("missing run_finished event")
	}
	if countEventType(events, store.EventNodeStarted) < 3 {
		t.Errorf("expected ≥3 node_started events (reviewer_claude + reviewer_gpt + streak_check), got %d",
			countEventType(events, store.EventNodeStarted))
	}
	if countEventType(events, store.EventEdgeSelected) < 2 {
		t.Errorf("expected ≥2 edge_selected events (round-robin dispatch creates one per reviewer), got %d",
			countEventType(events, store.EventEdgeSelected))
	}
	finishedIDs := eventNodeIDs(events, store.EventNodeFinished)
	finishedSet := make(map[string]bool, len(finishedIDs))
	for _, id := range finishedIDs {
		finishedSet[id] = true
	}
	for _, want := range []string{"reviewer_claude", "reviewer_gpt"} {
		if !finishedSet[want] {
			t.Errorf("expected node_finished event for %q, got %v", want, finishedIDs)
		}
	}
}

// TestWholeImproveLoop_RecoveryLoopExhausted makes both reviewers reject
// with concrete blockers on every pass, so each iteration routes through a
// fix_X. The bounded loops terminate the cascade (review_loop(15) binds
// before recovery_loop(20), since review_loop is incremented every cycle)
// and execution falls through to a `fail` terminal — a non-convergence,
// reported as `failed`. Asserts the fixer ran many times before the cap.
func TestWholeImproveLoop_RecoveryLoopExhausted(t *testing.T) {
	wf := compileFixtureStubSafe(t, "whole-improve-loop/main.bot")
	exec := newScenarioExecutor()
	stubSnapshotChunk(exec)

	// Both reviewers always reject with concrete blockers so each
	// streak_check routes through fix_X.
	exec.on("reviewer_claude", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"approved": false, "family": "claude",
			"blockers": []interface{}{"claude found a blocker"},
			"fix_plan": "fix the claude blocker",
			"_tokens":  50,
		}, nil
	})
	exec.on("reviewer_gpt", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"approved": false, "family": "gpt",
			"blockers": []interface{}{"gpt found a blocker"},
			"fix_plan": "fix the gpt blocker",
			"_tokens":  50,
		}, nil
	})
	exec.on("fix_claude", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"applied": true, "summary": "claude fix", "_tokens": 100}, nil
	})
	exec.on("fix_gpt", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"applied": true, "summary": "gpt fix", "_tokens": 100}, nil
	})

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)
	// Never converges (every pass has a blocker) → loop exhaustion routes
	// to the `fail` terminal, which surfaces as a Run error.
	if err := eng.Run(context.Background(), "run-vibe-recovery-exhausted", nil); err == nil {
		t.Fatalf("expected Run to error on loop exhaustion (non-convergence), got nil")
	}

	run, err := s.LoadRun(context.Background(), "run-vibe-recovery-exhausted")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if run.Status != store.RunStatusFailed {
		t.Fatalf("status = %s, want %s (loop exhaustion routes to fail — no silent success)",
			run.Status, store.RunStatusFailed)
	}
	// Bounded by review_loop(15) on the reviewer→streak_check edge —
	// the review cap kicks in before recovery_loop(20) does, since
	// review_loop is what's incremented every cycle. The exact cap
	// depends on round-robin starting family; assert "fixes ran each pass"
	// rather than a specific number. With num_chunks=1 the dynamic bound is
	// loop_max=3, so ~3 fix cycles run before review_loop exhausts.
	totalFixes := exec.callCount("fix_claude") + exec.callCount("fix_gpt")
	if totalFixes < 2 {
		t.Errorf("expected fixer activity across the bounded loop (≥2), got %d (claude=%d, gpt=%d)",
			totalFixes, exec.callCount("fix_claude"), exec.callCount("fix_gpt"))
	}
}

// TestWholeImproveLoop_SessionInheritStructural is a structural
// assertion on the bot's IR rather than a runtime trace: it confirms
// the fix_* agents are declared with `session: inherit` so the
// runtime can splice them into the same Claude/GPT conversation the
// reviewer was using. Drift on this property silently breaks
// prompt-cache hits and reviewer-context continuity — the live runs
// would still pass but cost more, so we pin it here.
func TestWholeImproveLoop_SessionInheritStructural(t *testing.T) {
	wf := compileFixtureStubSafe(t, "whole-improve-loop/main.bot")

	for _, id := range []string{"fix_claude", "fix_gpt"} {
		node, ok := wf.Nodes[id]
		if !ok {
			t.Fatalf("workflow missing expected node %q", id)
		}
		agent, ok := node.(*ir.AgentNode)
		if !ok {
			t.Fatalf("node %q is not an AgentNode (got %T)", id, node)
		}
		if agent.Session != ir.SessionInherit {
			t.Errorf("node %q session = %s, want %s (drift breaks reviewer→fix prompt-cache continuity)",
				id, agent.Session, ir.SessionInherit)
		}
	}
}

// TestWholeImproveLoop_PerUnitConvergesCommitsAndAdvances pins the ADR-055
// per-unit convergence + incremental-commit behaviour on a 3-unit backlog with
// all-clean reviews. The cursor STAYS on a unit until it converges (2
// consecutive clean reviews), then a commit_unit lands and the cursor advances
// — so with a clean run each unit takes exactly 2 passes:
//
//	pass1 unit0 (claude approve) → unit_streak 1, cursor stays 0
//	pass2 unit0 (gpt approve)    → unit_streak 2 → CONVERGED → commit_unit, cursor→1
//	pass3 unit1 (claude approve) → unit_streak 1, cursor stays 1
//	pass4 unit1 (gpt approve)    → CONVERGED → commit_unit, cursor→2
//	pass5 unit2 (claude approve) → unit_streak 1, cursor stays 2
//	pass6 unit2 (gpt approve)    → CONVERGED and units_done+1>=num_chunks → STOP
//
// Asserts:
//   - snapshot_chunk sees cursor 0,0,1,1,2,2 — advancing ONLY on convergence,
//     NOT one-per-pass. A stuck-at-0 cursor (per-unit loop never advances) or a
//     0,1,2,… cursor (advancing every pass like the old global sweep) both fail.
//   - units_done accumulates 0,0,1,1,2,2 across passes (crash-safe carry).
//   - commit_unit fires exactly twice — for units 0 and 1. Unit 2's convergence
//     is also the run's stop, so it finalizes via the build/test gate +
//     commit_changes, not commit_unit (first-match-wins on the stop edge).
//   - the run finishes.
func TestWholeImproveLoop_PerUnitConvergesCommitsAndAdvances(t *testing.T) {
	wf := compileFixtureStubSafe(t, "whole-improve-loop/main.bot")
	exec := newScenarioExecutor()

	var gotCursors, gotUnitsDone []int
	exec.on("snapshot_chunk", func(in map[string]interface{}) (map[string]interface{}, error) {
		cur := wilToInt(in["incoming_cursor"])
		streak := wilToInt(in["incoming_unit_streak"])
		passes := wilToInt(in["incoming_unit_passes"])
		units := wilToInt(in["incoming_units_done"])
		gotCursors = append(gotCursors, cur)
		gotUnitsDone = append(gotUnitsDone, units)
		return map[string]interface{}{
			"chunk_content": "// stub", "files": "stub.go", "chunk_label": "stub",
			"chunk_index": cur % 3, "num_chunks": 3, "loop_max": 8, "chunked": true,
			"partial": false, "unit_label": "stub", "unit_part": 1, "unit_parts": 1,
			"file_count": 1, "chunk_tokens": 10, "total_files": 3, "total_tokens": 30,
			"skipped_oversize": 0, "persisted_unit_streak": streak,
			"persisted_unit_passes": passes, "persisted_units_done": units,
			"cursor": cur, "_tokens": 1,
		}, nil
	})
	approve := func(fam string) func(map[string]interface{}) (map[string]interface{}, error) {
		return func(_ map[string]interface{}) (map[string]interface{}, error) {
			return map[string]interface{}{
				"approved": true, "family": fam,
				"blockers": []string{}, "fix_plan": "", "_tokens": 10,
			}, nil
		}
	}
	exec.on("reviewer_claude", approve("claude"))
	exec.on("reviewer_gpt", approve("gpt"))
	stubVerifyGate(exec)

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)
	if err := eng.Run(context.Background(), "run-vibe-perunit", map[string]interface{}{"context_mode": "inline"}); err != nil { // inline: stub reviewers can't open real files (ADR-045)
		t.Fatalf("Run: %v", err)
	}
	run, err := s.LoadRun(context.Background(), "run-vibe-perunit")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if run.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want %s", run.Status, store.RunStatusFinished)
	}
	wantCursors := []int{0, 0, 1, 1, 2, 2} // advances ONLY on convergence (every 2 clean passes)
	if !wilEqualInts(gotCursors, wantCursors) {
		t.Errorf("snapshot incoming_cursor sequence = %v, want %v (cursor must advance only on per-unit convergence, not every pass and not never)",
			gotCursors, wantCursors)
	}
	wantUnits := []int{0, 0, 1, 1, 2, 2}
	if !wilEqualInts(gotUnitsDone, wantUnits) {
		t.Errorf("snapshot incoming_units_done sequence = %v, want %v (units_done must accumulate as units converge)",
			gotUnitsDone, wantUnits)
	}
	if got := exec.callCount("commit_unit"); got != 2 {
		t.Errorf("commit_unit called %d times, want 2 (per-unit incremental commit for units 0 and 1; unit 2 finalizes via stop)", got)
	}
}

// TestWholeImproveLoop_PerUnitVerifyGatesCommit pins the ADR-055 2a property:
// a converged unit is NOT committed until a deterministic per-unit build/test
// verify passes. On a 2-unit all-clean backlog, unit 0 converges FIRST (not the
// last unit) so it routes streak_check -> unit_verify_build -> unit_verify_run
// -> commit_unit. We make unit_verify_run RED on its first invocation and GREEN
// after (modelling unit_verify_build repairing the breakage), and assert:
//   - commit_unit NEVER fires while the per-unit verify is red (a broken
//     intermediate commit is exactly what 2a forbids);
//   - the gate retried (unit_verify_run called >= 2 — fail then pass) via the
//     bounded unit_verify_loop(3);
//   - commit_unit landed exactly once (unit 0), after the gate went green;
//   - the run finishes (unit 1's convergence is the stop → final gate).
func TestWholeImproveLoop_PerUnitVerifyGatesCommit(t *testing.T) {
	wf := compileFixtureStubSafe(t, "whole-improve-loop/main.bot")
	exec := newScenarioExecutor()

	exec.on("snapshot_chunk", func(in map[string]interface{}) (map[string]interface{}, error) {
		cur := wilToInt(in["incoming_cursor"])
		return map[string]interface{}{
			"chunk_content": "// stub", "files": "stub.go", "chunk_label": "stub",
			"chunk_index": cur % 2, "num_chunks": 2, "loop_max": 12, "chunked": true,
			"partial": false, "unit_label": "stub", "unit_part": 1, "unit_parts": 1,
			"file_count": 1, "chunk_tokens": 10, "total_files": 2, "total_tokens": 20,
			"skipped_oversize": 0, "persisted_unit_streak": wilToInt(in["incoming_unit_streak"]),
			"persisted_unit_passes": wilToInt(in["incoming_unit_passes"]),
			"persisted_units_done":  wilToInt(in["incoming_units_done"]),
			"cursor":                cur, "_tokens": 1,
		}, nil
	})
	approve := func(fam string) func(map[string]interface{}) (map[string]interface{}, error) {
		return func(_ map[string]interface{}) (map[string]interface{}, error) {
			return map[string]interface{}{
				"approved": true, "family": fam,
				"blockers": []string{}, "fix_plan": "", "_tokens": 10,
			}, nil
		}
	}
	exec.on("reviewer_claude", approve("claude"))
	exec.on("reviewer_gpt", approve("gpt"))

	// Per-unit gate: RED on the first call, GREEN after. The final gate
	// (verify_run) is always green.
	unitVerifyCalls := 0
	lastPerUnitPassed := false
	exec.on("unit_verify_run", func(_ map[string]interface{}) (map[string]interface{}, error) {
		unitVerifyCalls++
		passed := unitVerifyCalls >= 2 // first red, then green (unit_verify_build "fixed" it)
		lastPerUnitPassed = passed
		return map[string]interface{}{
			"passed": passed, "skipped": false, "exit_code": 0,
			"log_tail": "stub build failure", "_tokens": 1,
		}, nil
	})
	exec.on("verify_run", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"passed": true, "log_tail": "", "_tokens": 1}, nil
	})
	// commit_unit must only ever fire when the most recent per-unit verify was
	// GREEN — the core 2a invariant.
	exec.on("commit_unit", func(_ map[string]interface{}) (map[string]interface{}, error) {
		if !lastPerUnitPassed {
			t.Errorf("commit_unit fired while the per-unit build/test verify was RED — 2a must gate every incremental commit on a green build")
		}
		return map[string]interface{}{"success": true, "output": "committed", "_tokens": 1}, nil
	})

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)
	if err := eng.Run(context.Background(), "run-vibe-unitverify", map[string]interface{}{"context_mode": "inline"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	run, err := s.LoadRun(context.Background(), "run-vibe-unitverify")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if run.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want %s", run.Status, store.RunStatusFinished)
	}
	if unitVerifyCalls < 2 {
		t.Errorf("unit_verify_run called %d times, want >= 2 (the gate must RETRY via unit_verify_loop after a red build, not commit)", unitVerifyCalls)
	}
	if got := exec.callCount("commit_unit"); got != 1 {
		t.Errorf("commit_unit called %d times, want 1 (unit 0 commits after the gate goes green; unit 1 finalizes via stop)", got)
	}
	if got := exec.callCount("unit_verify_build"); got < 2 {
		t.Errorf("unit_verify_build called %d times, want >= 2 (the agent re-runs to repair the red build before commit)", got)
	}
}

func wilEqualInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

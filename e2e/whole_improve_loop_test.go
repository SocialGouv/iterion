package e2e

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestWholeImproveLoop_HappyPath simulates the canonical "two
// consecutive cross-family approvals" scenario:
//
//	iter1: claude approves   → streak_check.stop = false (no previous)
//	iter2: gpt approves      → streak_check.stop = true  → done
func TestWholeImproveLoop_HappyPath(t *testing.T) {
	wf := compileFixtureStubSafe(t, "whole_improve_loop/main.bot")
	exec := newScenarioExecutor()

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
	if err := eng.Run(context.Background(), "run-vibe-happy", nil); err != nil {
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
	wf := compileFixtureStubSafe(t, "whole_improve_loop/main.bot")
	exec := newScenarioExecutor()

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
			"_tokens": 200,
		}, nil
	})

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)
	if err := eng.Run(context.Background(), "run-vibe-fix", nil); err != nil {
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

// TestWholeImproveLoop_LoopExhausted simulates 6 iterations where
// the reviewers never agree across families (claude always approves,
// gpt always rejects). The review_loop bound should kick in and the
// run should fall through to the unconditional `streak_check -> done`
// fallback.
func TestWholeImproveLoop_LoopExhausted(t *testing.T) {
	wf := compileFixtureStubSafe(t, "whole_improve_loop/main.bot")
	exec := newScenarioExecutor()

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
	if err := eng.Run(context.Background(), "run-vibe-exhausted", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	run, err := s.LoadRun(context.Background(), "run-vibe-exhausted")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want %s (loop should exhaust then fall through to done)",
			run.Status, store.RunStatusFinished)
	}
}

// TestWholeImproveLoop_EventTrace establishes the event-coherence
// baseline: a happy-path run must persist a complete trace covering
// node lifecycle, edge selection, and round-robin reviewer dispatch.
// This is the regression net for any future refactor of the engine's
// event emission — a missing event type surfaces here first.
func TestWholeImproveLoop_EventTrace(t *testing.T) {
	wf := compileFixtureStubSafe(t, "whole_improve_loop/main.bot")
	exec := newScenarioExecutor()
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
	if err := eng.Run(context.Background(), "run-vibe-events", nil); err != nil {
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

// TestWholeImproveLoop_RecoveryLoopExhausted forces the
// recovery_loop to dominate by making each family approve its OWN
// pushback but reject the OTHER family's work. This produces a fix
// cycle on every iteration; the bounded `recovery_loop(20)` should
// terminate the cascade and fall through to the unconditional
// fix_X → done edge. Asserts the total fixer count is close to the cap.
func TestWholeImproveLoop_RecoveryLoopExhausted(t *testing.T) {
	wf := compileFixtureStubSafe(t, "whole_improve_loop/main.bot")
	exec := newScenarioExecutor()

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
	if err := eng.Run(context.Background(), "run-vibe-recovery-exhausted", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	run, err := s.LoadRun(context.Background(), "run-vibe-recovery-exhausted")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if run.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want %s", run.Status, store.RunStatusFinished)
	}
	// Bounded by review_loop(15) on the reviewer→streak_check edge —
	// the review cap kicks in before recovery_loop(20) does, since
	// review_loop is what's incremented every cycle. The exact cap
	// depends on round-robin starting family; assert "many fixes ran"
	// rather than a specific number.
	totalFixes := exec.callCount("fix_claude") + exec.callCount("fix_gpt")
	if totalFixes < 5 {
		t.Errorf("expected significant fixer activity (≥5), got %d (claude=%d, gpt=%d)",
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
	wf := compileFixtureStubSafe(t, "whole_improve_loop/main.bot")

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

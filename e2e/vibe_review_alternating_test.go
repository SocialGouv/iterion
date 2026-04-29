package e2e

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestVibeReviewAlternating_HappyPath simulates the canonical "two
// consecutive cross-family approvals" scenario:
//
//	iter1: claude approves   → streak_check.stop = false (no previous)
//	iter2: gpt approves      → streak_check.stop = true  → done
func TestVibeReviewAlternating_HappyPath(t *testing.T) {
	wf := compileFixture(t, "vibe_review_alternating.iter")
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

	run, err := s.LoadRun("run-vibe-happy")
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

// TestVibeReviewAlternating_FixThenApprove simulates a scenario where
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
func TestVibeReviewAlternating_FixThenApprove(t *testing.T) {
	wf := compileFixture(t, "vibe_review_alternating.iter")
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

	run, err := s.LoadRun("run-vibe-fix")
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

// TestVibeReviewAlternating_LoopExhausted simulates 6 iterations where
// the reviewers never agree across families (claude always approves,
// gpt always rejects). The review_loop bound should kick in and the
// run should fall through to the unconditional `streak_check -> done`
// fallback.
func TestVibeReviewAlternating_LoopExhausted(t *testing.T) {
	wf := compileFixture(t, "vibe_review_alternating.iter")
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

	run, err := s.LoadRun("run-vibe-exhausted")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want %s (loop should exhaust then fall through to done)",
			run.Status, store.RunStatusFinished)
	}
}

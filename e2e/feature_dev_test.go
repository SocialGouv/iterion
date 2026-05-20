package e2e

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// featureDevStubInputs mirrors what the runtime would receive from a
// real launch: the `feature_prompt` and `workspace_dir` mapped via
// `vars:` defaults. The stub never reads them — the dev-phase stubs
// return canned outputs — but supplying them keeps the run.json
// inspect-able and reflects the live invocation path.
var featureDevStubInputs = map[string]interface{}{
	"feature_prompt": "stub: add Answer() int returning 42 in answer.go",
	"workspace_dir":  "/tmp/feature-dev-stub",
}

// devPhaseStubs registers stubs for plan / act / simplify so the
// alternating review loop receives a session-id chain. All three nodes
// must produce a session id because subsequent edges relay it via
// `with {_session_id: "{{outputs.X._session_id}}"}` mappings.
func devPhaseStubs(exec *scenarioExecutor) {
	exec.on("plan", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"_session_id": "sess-plan-1",
			"_tokens":     200,
			"_cost_usd":   0.02,
		}, nil
	})
	exec.on("act", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"_session_id": "sess-act-1",
			"_tokens":     400,
			"_cost_usd":   0.04,
		}, nil
	})
	exec.on("simplify", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"_session_id": "sess-simp-1",
			"_tokens":     150,
			"_cost_usd":   0.015,
		}, nil
	})
}

// commitPhaseStubs registers prepare_commit + commit_changes stubs that
// produce a schema-valid commit_output and a successful commit_result.
// Tests can override commit_changes to assert it is called with the
// expected relayed fields (see TestVibeFeatureDev_CommitInputRelay).
func commitPhaseStubs(exec *scenarioExecutor) {
	exec.on("prepare_commit", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"_session_id":  "sess-commit-1",
			"type":         "feat",
			"scope":        "answer",
			"subject":      "add Answer() returning 42",
			"full_message": "feat(answer): add Answer() returning 42",
			"files":        []interface{}{"answer.go"},
			"committed":    false,
			"_tokens":      120,
		}, nil
	})
	exec.on("commit_changes", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"success": true,
			"output":  "[main abc1234] feat(answer): add Answer() returning 42",
		}, nil
	})
}

// approveVerdict returns the canonical "approved" verdict shape for the
// given reviewer family. Both reviewers share the same output schema
// (`verdict_output`) so the family is the only distinguishing field.
func approveVerdict(family string) map[string]interface{} {
	return map[string]interface{}{
		"approved":      true,
		"family":        family,
		"blockers":      []interface{}{},
		"fix_plan":      "",
		"confidence":    "high",
		"scanned_areas": []interface{}{"pkg/runtime"},
		"_session_id":   "sess-rev-" + family,
		"_tokens":       100,
	}
}

// rejectVerdict returns a "rejected with blockers" verdict that will
// route the run through the same-family fixer per the bot's edge
// conditions (`!approved && family == 'X' && length(blockers) > 0`).
func rejectVerdict(family string) map[string]interface{} {
	return map[string]interface{}{
		"approved":      false,
		"family":        family,
		"blockers":      []interface{}{"missing test"},
		"fix_plan":      "add a Go test for Answer()",
		"confidence":    "high",
		"scanned_areas": []interface{}{"pkg/runtime"},
		"_session_id":   "sess-rev-" + family,
		"_tokens":       100,
	}
}

// TestVibeFeatureDev_HappyPath drives the canonical end-to-end flow:
//
//	plan → act → simplify → alt → reviewer_claude(approve) →
//	streak_check → alt → reviewer_gpt(approve) → streak_check.stop →
//	prepare_commit → commit_changes(success:true) → done
//
// Asserts: a single commit was produced, no fixers were called, and
// the run reached `finished`.
func TestVibeFeatureDev_HappyPath(t *testing.T) {
	wf := compileFixtureStubSafe(t, "feature_dev/main.bot")
	exec := newScenarioExecutor()

	devPhaseStubs(exec)
	commitPhaseStubs(exec)
	exec.on("reviewer_claude", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return approveVerdict("claude"), nil
	})
	exec.on("reviewer_gpt", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return approveVerdict("gpt"), nil
	})

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)
	if err := eng.Run(context.Background(), "run-vfd-happy", featureDevStubInputs); err != nil {
		t.Fatalf("Run: %v", err)
	}

	run, err := s.LoadRun(context.Background(), "run-vfd-happy")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if run.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want %s", run.Status, store.RunStatusFinished)
	}
	if exec.callCount("commit_changes") != 1 {
		t.Errorf("expected commit_changes once, got %d", exec.callCount("commit_changes"))
	}
	if exec.wasCalled("fix_claude") || exec.wasCalled("fix_gpt") {
		t.Errorf("no fixer should run on happy path (claude=%d, gpt=%d)",
			exec.callCount("fix_claude"), exec.callCount("fix_gpt"))
	}
	for _, devNode := range []string{"plan", "act", "simplify", "prepare_commit"} {
		if exec.callCount(devNode) != 1 {
			t.Errorf("expected %s once, got %d", devNode, exec.callCount(devNode))
		}
	}
}

// TestVibeFeatureDev_FixThenCommit simulates a single fix cycle before
// reaching the commit:
//
//	round 1: reviewer_claude rejects → fix_claude
//	round 2: reviewer_gpt approves → streak_check (no streak yet — gpt
//	         saw a fix between rounds, not a reviewer approval)
//	round 3: reviewer_claude approves → streak_check.stop → commit
//
// Asserts: fix_claude ran exactly once, commit_changes once, run finished.
func TestVibeFeatureDev_FixThenCommit(t *testing.T) {
	wf := compileFixtureStubSafe(t, "feature_dev/main.bot")
	exec := newScenarioExecutor()

	devPhaseStubs(exec)
	commitPhaseStubs(exec)

	claudeCalls := 0
	exec.on("reviewer_claude", func(_ map[string]interface{}) (map[string]interface{}, error) {
		claudeCalls++
		if claudeCalls == 1 {
			return rejectVerdict("claude"), nil
		}
		return approveVerdict("claude"), nil
	})
	exec.on("reviewer_gpt", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return approveVerdict("gpt"), nil
	})
	exec.on("fix_claude", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"applied":                true,
			"summary":                "added missing test",
			"pushback":               []interface{}{},
			"pushback_justification": "",
			"_session_id":            "sess-fix-claude-1",
			"_tokens":                250,
		}, nil
	})

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)
	if err := eng.Run(context.Background(), "run-vfd-fix", featureDevStubInputs); err != nil {
		t.Fatalf("Run: %v", err)
	}

	run, err := s.LoadRun(context.Background(), "run-vfd-fix")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if run.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want %s", run.Status, store.RunStatusFinished)
	}
	if exec.callCount("fix_claude") != 1 {
		t.Errorf("expected fix_claude once, got %d", exec.callCount("fix_claude"))
	}
	if exec.callCount("commit_changes") != 1 {
		t.Errorf("expected commit_changes once, got %d", exec.callCount("commit_changes"))
	}
}

// TestVibeFeatureDev_LoopExhausted forces the review loop to never
// converge: claude always approves, gpt always rejects with blockers.
// The bounded `review_loop(15)` should kick in and the run falls
// through to the unconditional `reviewer_X -> done` fallthrough.
//
// Asserts: commit_changes was NEVER called, run still finished gracefully.
func TestVibeFeatureDev_LoopExhausted(t *testing.T) {
	wf := compileFixtureStubSafe(t, "feature_dev/main.bot")
	exec := newScenarioExecutor()

	devPhaseStubs(exec)
	commitPhaseStubs(exec)

	exec.on("reviewer_claude", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return approveVerdict("claude"), nil
	})
	exec.on("reviewer_gpt", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return rejectVerdict("gpt"), nil
	})
	exec.on("fix_gpt", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"applied":                true,
			"summary":                "tried",
			"pushback":               []interface{}{},
			"pushback_justification": "",
			"_session_id":            "sess-fix-gpt-1",
			"_tokens":                250,
		}, nil
	})

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)
	if err := eng.Run(context.Background(), "run-vfd-exhausted", featureDevStubInputs); err != nil {
		t.Fatalf("Run: %v", err)
	}

	run, err := s.LoadRun(context.Background(), "run-vfd-exhausted")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if run.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want %s (loop should exhaust then fall through to done)",
			run.Status, store.RunStatusFinished)
	}
	if exec.callCount("commit_changes") != 0 {
		t.Errorf("commit_changes should NOT run when reviewers never streak (got %d)",
			exec.callCount("commit_changes"))
	}
}

// TestVibeFeatureDev_CommitInputRelay verifies that the
// prepare_commit → commit_changes edge correctly relays the
// `with { full_message, files, workspace_dir }` mapping. The
// commit_changes stub captures its input and the test inspects what
// the runtime templated in from prepare_commit.outputs and vars.
func TestVibeFeatureDev_CommitInputRelay(t *testing.T) {
	wf := compileFixtureStubSafe(t, "feature_dev/main.bot")
	exec := newScenarioExecutor()

	devPhaseStubs(exec)
	exec.on("reviewer_claude", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return approveVerdict("claude"), nil
	})
	exec.on("reviewer_gpt", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return approveVerdict("gpt"), nil
	})

	expectedFiles := []interface{}{"answer.go", "answer_test.go"}
	expectedMessage := "feat(answer): add Answer() returning 42\n\nWith a Go test."
	exec.on("prepare_commit", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"_session_id":  "sess-commit-1",
			"type":         "feat",
			"scope":        "answer",
			"subject":      "add Answer() returning 42",
			"full_message": expectedMessage,
			"files":        expectedFiles,
			"committed":    false,
			"_tokens":      120,
		}, nil
	})

	var capturedInput map[string]interface{}
	exec.on("commit_changes", func(input map[string]interface{}) (map[string]interface{}, error) {
		capturedInput = input
		return map[string]interface{}{
			"success": true,
			"output":  "[main abc1234] feat(answer): add Answer() returning 42",
		}, nil
	})

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)
	if err := eng.Run(context.Background(), "run-vfd-relay", featureDevStubInputs); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if capturedInput == nil {
		t.Fatalf("commit_changes was never invoked")
	}
	if got, _ := capturedInput["full_message"].(string); got != expectedMessage {
		t.Errorf("full_message relay: got %q, want %q", got, expectedMessage)
	}
	if got, _ := capturedInput["workspace_dir"].(string); got != featureDevStubInputs["workspace_dir"] {
		t.Errorf("workspace_dir relay: got %q, want %q", got, featureDevStubInputs["workspace_dir"])
	}
	gotFiles, _ := capturedInput["files"].([]interface{})
	if len(gotFiles) != len(expectedFiles) {
		t.Errorf("files relay: got %d files, want %d", len(gotFiles), len(expectedFiles))
	}
}

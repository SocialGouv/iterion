package e2e

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The interview loop's exhaustion exit (C145, #1293). Once
// max_interview_turns loop-backs are spent, the operator's next answer no
// longer reaches the interviewer: the run refuses with the typed
// INTERVIEW_NOT_CONVERGED — never LOOP_EXHAUSTED, and never a campaign
// launched behind an interview that did not converge (nothing is banked
// before SPEC.md is committed).
func TestAppDevInterviewExhaustionRefusesTyped(t *testing.T) {
	t.Parallel()
	wf := compileFixtureStubSafe(t, "app-dev/main.bot")
	// A stub run: no per-run worktree of the test's own checkout, no
	// supervisor watching a stubbed campaign.
	wf.Worktree = "none"
	wf.Supervisors = nil

	dir := t.TempDir()
	s, err := store.New(dir)
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	exec := newScenarioExecutor()
	exec.on("interviewer", func(map[string]any) (map[string]any, error) {
		return map[string]any{
			"reply":         "une question de plus",
			"spec_ready":    false,
			"spec_path":     "",
			"spec_summary":  "",
			"quick_replies": []any{},
			"_tokens":       10,
			"_cost_usd":     0.001,
		}, nil
	})
	eng := runtime.New(wf, s, exec)
	const runID = "e2e-app-dev-interview-exhausted"

	err = eng.Run(context.Background(), runID, map[string]any{
		"mode":                "interview",
		"max_interview_turns": 2,
		"app_prompt":          "",
	})
	if !errors.Is(err, runtime.ErrRunPaused) {
		t.Fatalf("expected the chat pause after the opening turn, got: %v", err)
	}
	// Two loop-backs are the cap: the first two answers reach the
	// interviewer, the third finds the loop spent.
	for i := 1; i <= 2; i++ {
		err = eng.Resume(context.Background(), runID, map[string]any{"message": "je ne sais pas encore"})
		if !errors.Is(err, runtime.ErrRunPaused) {
			t.Fatalf("answer %d: expected the chat pause, got: %v", i, err)
		}
	}
	err = eng.Resume(context.Background(), runID, map[string]any{"message": "toujours pas"})
	if err == nil || errors.Is(err, runtime.ErrRunPaused) {
		t.Fatalf("third answer: expected the typed refusal, got: %v", err)
	}

	if got := exec.callCount("interviewer"); got != 3 {
		t.Errorf("interviewer ran %d times, want 3 (the opening turn + two loop-backs)", got)
	}
	if got := exec.callCount("plan") + exec.callCount("campaign"); got != 0 {
		t.Errorf("the plan or the campaign ran (%d) behind an interview that did not converge", got)
	}
	run := loadRun(t, dir, runID)
	// Resumable: the same exit is taken when the budget guard declines the
	// back-edge, and the cure for that cause is a resume with a raised cap.
	if run.Status != store.RunStatusFailedResumable {
		t.Errorf("status = %s, want failed_resumable", run.Status)
	}
	if run.FailureCode != "INTERVIEW_NOT_CONVERGED" {
		t.Errorf("failure_code = %q, want INTERVIEW_NOT_CONVERGED — the loop's exhaustion exit was not taken (error: %s)", run.FailureCode, run.Error)
	}
	// The message names the figures the operator reads to tell the cap
	// from the budget: the answers that reached the interviewer, the cap.
	if !strings.Contains(run.Error, "2 operator answers reached the interviewer (cap 2)") {
		t.Errorf("error = %q, want the loop count and the cap in force rendered", run.Error)
	}
	if run.Checkpoint == nil || run.Checkpoint.NodeID != "interview_chat" {
		t.Errorf("checkpoint = %+v, want anchored on the chat pause the refusal followed — a resume re-asks the operator and re-evaluates the back-edge", run.Checkpoint)
	}
}

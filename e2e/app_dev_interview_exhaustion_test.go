package e2e

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The interview loop's exhaustion exits (C145, #1293). Once the chat
// pause's back-edge is declined the operator's answer no longer reaches the
// interviewer and the run refuses — never LOOP_EXHAUSTED, and never a
// campaign launched behind an interview that did not converge (nothing is
// banked before SPEC.md is committed). The cause is told apart at the
// pause by the loop counter: a spent cap is terminal (an exhausted loop
// stays exhausted across resume), a turn the budget could no longer fund
// parks the run resumable with the checkpoint on the pause.

func appDevInterviewDrive(t *testing.T, budgetUSD float64, turnUSD float64, opts ...runtime.EngineOption) (*runtime.Engine, *scenarioExecutor, string) {
	t.Helper()
	wf := compileFixtureStubSafe(t, "app-dev/main.bot")
	// A stub run: no per-run worktree of the test's own checkout, no
	// supervisor watching a stubbed campaign.
	wf.Worktree = "none"
	wf.Supervisors = nil
	if budgetUSD > 0 {
		if wf.Budget == nil {
			wf.Budget = &ir.Budget{}
		}
		wf.Budget.MaxCostUSD = budgetUSD
	}
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
			"_cost_usd":     turnUSD,
		}, nil
	})
	return runtime.New(wf, s, exec, opts...), exec, dir
}

// The cap the guard reads is the one in force — the var plus any
// live-steering grant (`bump_loop interview_loop`) — never the var alone:
// a matching `when` is taken before the loop edge is considered, so a guard
// on the var would refuse the very turns a grant just paid for.
func TestAppDevInterviewGrantExtendsTheCap(t *testing.T) {
	t.Parallel()
	eng, exec, dir := appDevInterviewDrive(t, 0, 0.001)
	const runID = "e2e-app-dev-interview-cap-granted"

	err := eng.Run(context.Background(), runID, map[string]any{
		"mode":                "interview",
		"max_interview_turns": 2,
		"app_prompt":          "",
	})
	if !errors.Is(err, runtime.ErrRunPaused) {
		t.Fatalf("expected the chat pause after the opening turn, got: %v", err)
	}
	for i := 1; i <= 2; i++ {
		err = eng.Resume(context.Background(), runID, map[string]any{"message": "je ne sais pas encore"})
		if !errors.Is(err, runtime.ErrRunPaused) {
			t.Fatalf("answer %d: expected the chat pause, got: %v", i, err)
		}
	}
	// The cap is reached (two loop-backs of two). While the run is paused
	// the operator grants one more turn — `bump_loop interview_loop 1`
	// lands on the run record, which the resume re-seeds its overrides
	// from — then answers again: the back-edge is affordable and the
	// answer must reach the interviewer, not the terminal refusal.
	steering, err := store.New(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := steering.PatchRunSteering(context.Background(), runID, map[string]int{"interview_loop": 1}, nil); err != nil {
		t.Fatalf("grant one more turn: %v", err)
	}
	err = eng.Resume(context.Background(), runID, map[string]any{"message": "encore une"})
	if !errors.Is(err, runtime.ErrRunPaused) {
		t.Fatalf("answer 3 after the grant: expected the chat pause (the granted turn), got: %v", err)
	}
	if got := exec.callCount("interviewer"); got != 4 {
		t.Errorf("interviewer ran %d times, want 4 (the opening turn + three loop-backs, the third granted)", got)
	}
	// The granted cap (3) is now spent: the next answer refuses, naming it.
	err = eng.Resume(context.Background(), runID, map[string]any{"message": "toujours pas"})
	if err == nil || errors.Is(err, runtime.ErrRunPaused) {
		t.Fatalf("answer 4: expected the typed refusal, got: %v", err)
	}
	run := loadRun(t, dir, runID)
	if run.FailureCode != "INTERVIEW_NOT_CONVERGED" {
		t.Errorf("failure_code = %q, want INTERVIEW_NOT_CONVERGED (error: %s)", run.FailureCode, run.Error)
	}
	if !strings.Contains(run.Error, "spent its 3 operator answers") {
		t.Errorf("error = %q, want the cap in force (the granted 3, not the declared 2) rendered", run.Error)
	}
}

func TestAppDevInterviewExhaustionRefusesTyped(t *testing.T) {
	t.Parallel()
	eng, exec, dir := appDevInterviewDrive(t, 0, 0.001)
	const runID = "e2e-app-dev-interview-cap-spent"

	err := eng.Run(context.Background(), runID, map[string]any{
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
	// A spent cap is terminal: a resume could only re-ask the operator and
	// refuse again, since an exhausted loop stays exhausted.
	if run.Status != store.RunStatusFailed {
		t.Errorf("status = %s, want failed (a spent cap is not cured by a resume)", run.Status)
	}
	if run.FailureCode != "INTERVIEW_NOT_CONVERGED" {
		t.Errorf("failure_code = %q, want INTERVIEW_NOT_CONVERGED — the cap exit was not taken (error: %s)", run.FailureCode, run.Error)
	}
	if !strings.Contains(run.Error, "spent its 2 operator answers") {
		t.Errorf("error = %q, want the cap in force rendered", run.Error)
	}
}

func TestAppDevInterviewBudgetStarvedParksResumable(t *testing.T) {
	t.Parallel()
	// A $1 ceiling and $0.35 per interviewer turn: the first loop-back is
	// affordable ($0.35 spent, one more turn lands at $0.70), the second
	// would land past 90% of the ceiling ($0.70 + $0.35) and the
	// affordability guard declines it — the cap (30) is nowhere near.
	eng, exec, dir := appDevInterviewDrive(t, 1.0, 0.35)
	const runID = "e2e-app-dev-interview-budget-starved"

	err := eng.Run(context.Background(), runID, map[string]any{
		"mode":       "interview",
		"app_prompt": "",
	})
	if !errors.Is(err, runtime.ErrRunPaused) {
		t.Fatalf("expected the chat pause after the opening turn, got: %v", err)
	}
	err = eng.Resume(context.Background(), runID, map[string]any{"message": "je ne sais pas encore"})
	if !errors.Is(err, runtime.ErrRunPaused) {
		t.Fatalf("answer 1: expected the chat pause, got: %v", err)
	}
	err = eng.Resume(context.Background(), runID, map[string]any{"message": "toujours pas"})
	if err == nil || errors.Is(err, runtime.ErrRunPaused) {
		t.Fatalf("answer 2: expected the budget refusal, got: %v", err)
	}

	if got := exec.callCount("interviewer"); got != 2 {
		t.Errorf("interviewer ran %d times, want 2 (the opening turn + one loop-back)", got)
	}
	if got := exec.callCount("plan") + exec.callCount("campaign"); got != 0 {
		t.Errorf("the plan or the campaign ran (%d) behind an interview that did not converge", got)
	}
	run := loadRun(t, dir, runID)
	if run.Status != store.RunStatusFailedResumable {
		t.Errorf("status = %s, want failed_resumable (a raised cap continues the conversation)", run.Status)
	}
	if run.FailureCode != "INTERVIEW_BUDGET_STARVED" {
		t.Errorf("failure_code = %q, want INTERVIEW_BUDGET_STARVED — the budget exit was not taken (error: %s)", run.FailureCode, run.Error)
	}
	if !strings.Contains(run.Error, "after 1 operator answers") {
		t.Errorf("error = %q, want the answers reached rendered", run.Error)
	}
	if run.Checkpoint == nil || run.Checkpoint.NodeID != "interview_chat" {
		t.Errorf("checkpoint = %+v, want anchored on the chat pause — a resume re-asks the operator and re-evaluates the back-edge", run.Checkpoint)
	}

	s, err := store.New(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	events, err := s.LoadEvents(context.Background(), runID)
	if err != nil {
		t.Fatalf("load events: %v", err)
	}
	var declined bool
	for _, e := range events {
		if string(e.Type) == "budget_warning" && e.Data["reason"] == "loop_budget_guard" && e.Data["loop"] == "interview_loop" {
			declined = true
		}
	}
	if !declined {
		t.Error("no budget_warning with reason loop_budget_guard for interview_loop: the refusal was reached some other way")
	}
}

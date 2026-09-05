package e2e

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/SocialGouv/iterion/pkg/store"
)

// A bot that refuses on purpose has to say WHY on the run itself (#739):
// `iterion runs list`, the studio, the merge-gate notice and the alert
// sinks all read `failure_code` / `error`, and before this every
// deliberate termination handed them the same two constants.
func TestTypedFailStampsTheRun(t *testing.T) {
	storeDir := t.TempDir()
	const runID = "run-typed-fail"

	err := cli.RunRun(context.Background(), cli.RunOptions{
		File:          filepath.Join("testdata", "typed_fail_mini.bot"),
		StoreDir:      storeDir,
		RunID:         runID,
		Executor:      newScenarioExecutor(),
		NoInteractive: true,
		MergeInto:     "none",
	}, &cli.Printer{W: io.Discard, Format: cli.OutputJSON})
	if err == nil {
		t.Fatal("run succeeded; the fixture routes to a fail node")
	}

	run := loadRun(t, storeDir, runID)

	// `resumable: true` on the node: the cure for a budget guard is
	// "raise the cap and continue", not "start the phase over".
	if run.Status != store.RunStatusFailedResumable {
		t.Errorf("status = %s, want failed_resumable", run.Status)
	}
	if run.FailureCode != "PLAN_BUDGET_EXHAUSTED" {
		t.Errorf("failure_code = %q, want PLAN_BUDGET_EXHAUSTED", run.FailureCode)
	}
	// The message is a template resolved at fail time — the figure that
	// caused the refusal is usually the one worth reporting.
	if run.Error != "planning used 77% of max_duration" {
		t.Errorf("error = %q, want the rendered message", run.Error)
	}
	if run.Checkpoint == nil {
		t.Error("no checkpoint kept; a resumable refusal cannot be resumed")
	}

	// The run_failed event carries the same code, so an alert sink that
	// tails events sees what the run record says.
	s, err := store.New(storeDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	events, err := s.LoadEvents(context.Background(), runID)
	if err != nil {
		t.Fatalf("load events: %v", err)
	}
	var failed *store.Event
	for _, e := range events {
		if e.Type == store.EventRunFailed {
			failed = e
		}
	}
	if failed == nil {
		t.Fatal("no run_failed event")
	}
	if failed.Data["code"] != "PLAN_BUDGET_EXHAUSTED" {
		t.Errorf("run_failed code = %v, want PLAN_BUDGET_EXHAUSTED", failed.Data["code"])
	}
	if failed.Data["resumable"] != true {
		t.Errorf("run_failed resumable = %v, want true", failed.Data["resumable"])
	}
}

// The untyped `-> fail` target keeps behaving exactly as it always has:
// terminal `failed`, the FAIL_NODE code, the historical wording. Operator
// greps and the resume matrix both key on it.
func TestBareFailNodeIsUnchanged(t *testing.T) {
	storeDir := t.TempDir()
	const runID = "run-bare-fail"

	err := cli.RunRun(context.Background(), cli.RunOptions{
		File:          filepath.Join("testdata", "bare_fail_mini.bot"),
		StoreDir:      storeDir,
		RunID:         runID,
		Executor:      newScenarioExecutor(),
		NoInteractive: true,
		MergeInto:     "none",
	}, &cli.Printer{W: io.Discard, Format: cli.OutputJSON})
	if err == nil {
		t.Fatal("run succeeded; the fixture routes to the bare fail node")
	}
	if !strings.Contains(err.Error(), "workflow reached fail node") {
		t.Errorf("error = %v, want the historical wording", err)
	}

	run := loadRun(t, storeDir, runID)
	if run.Status != store.RunStatusFailed {
		t.Errorf("status = %s, want failed (a bare fail node is intentional termination)", run.Status)
	}
	if run.FailureCode != store.FailureFailNode {
		t.Errorf("failure_code = %q, want %s", run.FailureCode, store.FailureFailNode)
	}
	if run.Error != "workflow reached fail node" {
		t.Errorf("error = %q, want the historical wording", run.Error)
	}
}

// A `resumable: true` fail node promises the operator a resume that
// CONTINUES — docs/resume.md and examples/phase-budget-guard.bot both say
// "raise the cap and resume". That only holds if the resume re-evaluates
// the GUARD that routed into the fail node. Anchoring the checkpoint on the
// fail node itself makes the resume dispatch the fail node first and
// reproduce the identical outcome with no progress, so the promise is a
// lie and raising the cap changes nothing (R345e7d).
func TestResumableFailReExecutesTheGuard(t *testing.T) {
	storeDir := t.TempDir()
	const runID = "run-typed-fail-resume"
	botFile := filepath.Join("testdata", "typed_fail_resume_mini.bot")

	err := cli.RunRun(context.Background(), cli.RunOptions{
		File:          botFile,
		StoreDir:      storeDir,
		RunID:         runID,
		Executor:      newScenarioExecutor(),
		NoInteractive: true,
		MergeInto:     "none",
	}, &cli.Printer{W: io.Discard, Format: cli.OutputJSON})
	if err == nil {
		t.Fatal("first pass succeeded; the fixture refuses under a $0.5 cap")
	}

	run := loadRun(t, storeDir, runID)
	if run.Status != store.RunStatusFailedResumable {
		t.Fatalf("status = %s, want failed_resumable", run.Status)
	}
	if run.FailureCode != "PLAN_BUDGET_EXHAUSTED" {
		t.Fatalf("failure_code = %q, want PLAN_BUDGET_EXHAUSTED", run.FailureCode)
	}
	// The anchor IS the fix: a checkpoint on the fail node can only
	// re-dispatch the fail node.
	if run.Checkpoint == nil {
		t.Fatal("no checkpoint kept")
	}
	if run.Checkpoint.NodeID != "plan_budget_gate" {
		t.Fatalf("checkpoint anchored on %q, want the guard plan_budget_gate — "+
			"a resume from the fail node reproduces the same failure with no progress",
			run.Checkpoint.NodeID)
	}

	// The operator does what the message asks: raise the cap and resume.
	if err := cli.RunResumeWithFile(context.Background(), botFile, cli.ResumeOptions{
		RunID:    runID,
		StoreDir: storeDir,
		Executor: newScenarioExecutor(),
		Budget:   cli.BudgetOverrides{MaxCostUSD: 10},
	}, &cli.Printer{W: io.Discard, Format: cli.OutputJSON}); err != nil {
		t.Fatalf("resume failed: %v", err)
	}

	run = loadRun(t, storeDir, runID)
	if run.Status != store.RunStatusFinished {
		t.Fatalf("status = %s (%s), want finished after raising the cap", run.Status, run.Error)
	}
	if run.FailureCode != "" {
		t.Errorf("failure_code = %q, want cleared by the successful resume", run.FailureCode)
	}

	s, err := store.New(storeDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	events, err := s.LoadEvents(context.Background(), runID)
	if err != nil {
		t.Fatalf("load events: %v", err)
	}
	starts := map[string]int{}
	for _, e := range events {
		if e.Type == store.EventNodeStarted {
			starts[e.NodeID]++
		}
	}
	if starts["plan_budget_gate"] != 2 {
		t.Errorf("the guard ran %d times, want 2 (once per pass) — the resume did not re-evaluate it",
			starts["plan_budget_gate"])
	}
	if starts["plan_exhausted"] != 1 {
		t.Errorf("the fail node ran %d times, want 1 — the resume re-entered it instead of the guard",
			starts["plan_exhausted"])
	}
	if starts["implement"] != 1 {
		t.Errorf("implement ran %d times, want 1 — the raised cap did not open the else edge",
			starts["implement"])
	}
}

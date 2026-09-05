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

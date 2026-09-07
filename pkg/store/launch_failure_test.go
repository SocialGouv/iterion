package store

import (
	"context"
	"testing"
)

// A run that never left its launch path used to be ended four different
// ways across three packages, and none of them wrote the timeline: the row
// read `failed` with a message and nothing else, so a consumer that triages
// terminals by the tree had nothing to classify. The chokepoint writes
// both, and the code says which of the two it is.
func TestFailRunAtLaunch_WritesTheDocumentAndTheTimeline(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	ctx := context.Background()
	if _, err := s.CreateRun(ctx, "run-launch", "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	if err := FailRunAtLaunch(ctx, s, "run-launch", "runner failed to start: exec format error"); err != nil {
		t.Fatalf("FailRunAtLaunch: %v", err)
	}

	r, err := s.LoadRun(ctx, "run-launch")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r.Status != RunStatusFailed {
		t.Errorf("status = %q, want failed", r.Status)
	}
	if r.FailureCode != FailureLaunchFailed {
		t.Errorf("failure_code = %q, want %s", r.FailureCode, FailureLaunchFailed)
	}
	if r.Error == "" {
		t.Error("run.Error is empty — the operator has nothing to read")
	}

	events, err := s.LoadEvents(ctx, "run-launch")
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	var failed *Event
	for _, e := range events {
		if e.Type == EventRunFailed {
			failed = e
		}
	}
	if failed == nil {
		t.Fatal("no run_failed event — a terminal status with no event is what a headless router cannot classify")
	}
	if got, _ := failed.Data["code"].(string); got != string(FailureLaunchFailed) {
		t.Errorf("run_failed.code = %q, want %s", got, FailureLaunchFailed)
	}
	if got, _ := failed.Data["error"].(string); got != "runner failed to start: exec format error" {
		t.Errorf("run_failed.error = %q, want the launcher's own message", got)
	}
	if got, _ := failed.Data["phase"].(string); got != "launch" {
		t.Errorf("run_failed.phase = %q, want launch", got)
	}
}

// A caller with nothing to say still ends the run readably, rather than
// with an empty error — the shape #697 was filed on.
func TestFailRunAtLaunch_NeverLeavesAnEmptyError(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	ctx := context.Background()
	if _, err := s.CreateRun(ctx, "run-launch-bare", "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := FailRunAtLaunch(ctx, s, "run-launch-bare", ""); err != nil {
		t.Fatalf("FailRunAtLaunch: %v", err)
	}
	r, err := s.LoadRun(ctx, "run-launch-bare")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r.Error == "" {
		t.Fatal("run.Error is empty")
	}
}

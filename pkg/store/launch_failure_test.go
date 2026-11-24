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

// The conditional form ends a run only while it still has the status its
// launch persisted. A runner that claimed the attempt — a publish can report
// an error after its message landed — owns the run: neither its status nor
// its timeline is touched. Red when the conditional form writes
// unconditionally.
func TestFailRunAtLaunchIf_LeavesAClaimedRunToItsRunner(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	ctx := context.Background()
	for _, id := range []string{"run-claimed", "run-unclaimed"} {
		if _, err := s.CreateRun(ctx, id, "wf", nil); err != nil {
			t.Fatalf("CreateRun %s: %v", id, err)
		}
		if err := s.UpdateRunStatus(ctx, id, RunStatusQueued, ""); err != nil {
			t.Fatalf("queue %s: %v", id, err)
		}
	}
	if ok, err := s.UpdateRunStatusIf(ctx, "run-claimed", RunStatusRunning, "", []RunStatus{RunStatusQueued}); err != nil || !ok {
		t.Fatalf("claim run-claimed: changed=%v err=%v", ok, err)
	}

	ended, err := FailRunAtLaunchIf(ctx, s, "run-claimed", "nats: timeout waiting for ack", RunStatusQueued)
	if err != nil || ended {
		t.Fatalf("FailRunAtLaunchIf on a claimed run = %v, %v; want not ended", ended, err)
	}
	r, err := s.LoadRun(ctx, "run-claimed")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r.Status != RunStatusRunning || r.FailureCode != "" {
		t.Fatalf("claimed run: status=%s code=%q, want running with no failure code", r.Status, r.FailureCode)
	}
	events, err := s.LoadEvents(ctx, "run-claimed")
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	for _, e := range events {
		if e.Type == EventRunFailed {
			t.Fatal("a run_failed event was written for a run its runner executes")
		}
	}

	ended, err = FailRunAtLaunchIf(ctx, s, "run-unclaimed", "nats: no responders", RunStatusQueued)
	if err != nil || !ended {
		t.Fatalf("FailRunAtLaunchIf on a run still queued = %v, %v; want ended", ended, err)
	}
	r, err = s.LoadRun(ctx, "run-unclaimed")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r.Status != RunStatusFailed || r.FailureCode != FailureLaunchFailed {
		t.Fatalf("unclaimed run: status=%s code=%q, want failed/%s", r.Status, r.FailureCode, FailureLaunchFailed)
	}

	if _, err := FailRunAtLaunchIf(ctx, s, "run-unclaimed", "x"); err == nil {
		t.Fatal("FailRunAtLaunchIf with no expected status must refuse, never write unconditionally")
	}
}

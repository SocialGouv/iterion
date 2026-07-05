package store

import (
	"context"
	"testing"
)

func TestPlanStore(t *testing.T) {
	ctx := context.Background()
	s := tmpStore(t)
	const runID = "plan-run-1"
	if _, err := s.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	t.Run("AsPlanStore returns the FilesystemRunStore", func(t *testing.T) {
		if AsPlanStore(s) == nil {
			t.Fatalf("AsPlanStore returned nil for FilesystemRunStore")
		}
		if AsPlanStore(nil) != nil {
			t.Errorf("AsPlanStore(nil) should be nil")
		}
	})

	t.Run("ListPlanSnapshots is empty before any capture", func(t *testing.T) {
		got, err := s.ListPlanSnapshots(ctx, runID)
		if err != nil {
			t.Fatalf("ListPlanSnapshots: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("expected no snapshots, got %d", len(got))
		}
	})

	planA := PlanSnapshot{
		NodeID:    "campaign",
		Iteration: 0,
		Tool:      "TodoWrite",
		Todos: []PlanTodo{
			{Content: "step one", Status: "in_progress", ActiveForm: "doing step one"},
			{Content: "step two", Status: "pending"},
		},
	}
	planB := PlanSnapshot{
		NodeID:    "campaign",
		Iteration: 1,
		Tool:      "TodoWrite",
		Todos: []PlanTodo{
			{Content: "step one", Status: "completed", ActiveForm: "doing step one"},
			{Content: "step two", Status: "in_progress"},
		},
	}

	t.Run("first append writes seq 0", func(t *testing.T) {
		got, wrote, err := s.AppendPlanSnapshot(ctx, runID, planA)
		if err != nil {
			t.Fatalf("AppendPlanSnapshot: %v", err)
		}
		if !wrote {
			t.Fatalf("expected first append to be written")
		}
		if got.Seq != 0 {
			t.Errorf("expected seq 0, got %d", got.Seq)
		}
		if got.Timestamp.IsZero() {
			t.Errorf("expected timestamp to be stamped")
		}
	})

	t.Run("byte-identical re-append is deduped (not written)", func(t *testing.T) {
		got, wrote, err := s.AppendPlanSnapshot(ctx, runID, planA)
		if err != nil {
			t.Fatalf("AppendPlanSnapshot (dup): %v", err)
		}
		if wrote {
			t.Errorf("expected identical todos to be deduped")
		}
		if got.Seq != 0 {
			t.Errorf("dedup should return the prior snapshot (seq 0), got %d", got.Seq)
		}
		list, err := s.ListPlanSnapshots(ctx, runID)
		if err != nil {
			t.Fatalf("ListPlanSnapshots: %v", err)
		}
		if len(list) != 1 {
			t.Errorf("expected 1 snapshot after dedup, got %d", len(list))
		}
	})

	t.Run("changed todos append as seq 1", func(t *testing.T) {
		got, wrote, err := s.AppendPlanSnapshot(ctx, runID, planB)
		if err != nil {
			t.Fatalf("AppendPlanSnapshot (changed): %v", err)
		}
		if !wrote {
			t.Fatalf("expected changed todos to be written")
		}
		if got.Seq != 1 {
			t.Errorf("expected seq 1, got %d", got.Seq)
		}
	})

	t.Run("ListPlanSnapshots returns chronological ascending order", func(t *testing.T) {
		list, err := s.ListPlanSnapshots(ctx, runID)
		if err != nil {
			t.Fatalf("ListPlanSnapshots: %v", err)
		}
		if len(list) != 2 {
			t.Fatalf("expected 2 snapshots, got %d", len(list))
		}
		if list[0].Seq != 0 || list[1].Seq != 1 {
			t.Errorf("expected ascending seq [0,1], got [%d,%d]", list[0].Seq, list[1].Seq)
		}
		if list[0].Iteration != 0 || list[1].Iteration != 1 {
			t.Errorf("iteration mismatch: %d, %d", list[0].Iteration, list[1].Iteration)
		}
		if list[1].Todos[0].Status != "completed" {
			t.Errorf("expected the later snapshot to reflect completed step, got %q", list[1].Todos[0].Status)
		}
	})
}

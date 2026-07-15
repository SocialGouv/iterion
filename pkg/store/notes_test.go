package store

import (
	"context"
	"testing"
)

func TestRunNoteStore(t *testing.T) {
	ctx := context.Background()
	s := tmpStore(t)
	const runID = "note-run-1"
	if _, err := s.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	t.Run("AsRunNoteStore returns the FilesystemRunStore", func(t *testing.T) {
		if AsRunNoteStore(s) == nil {
			t.Fatalf("AsRunNoteStore returned nil for FilesystemRunStore")
		}
		if AsRunNoteStore(nil) != nil {
			t.Errorf("AsRunNoteStore(nil) should be nil")
		}
	})

	t.Run("ListRunNotes is empty before any append", func(t *testing.T) {
		got, err := s.ListRunNotes(ctx, runID)
		if err != nil {
			t.Fatalf("ListRunNotes: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("expected no notes, got %d", len(got))
		}
	})

	t.Run("append assigns seq + timestamp chronologically", func(t *testing.T) {
		a, err := s.AppendRunNote(ctx, runID, RunNote{Author: "alice", Body: "flaky, re-ran"})
		if err != nil {
			t.Fatalf("AppendRunNote: %v", err)
		}
		if a.Seq != 0 {
			t.Errorf("first note seq = %d, want 0", a.Seq)
		}
		if a.Timestamp.IsZero() {
			t.Errorf("timestamp not stamped")
		}
		b, err := s.AppendRunNote(ctx, runID, RunNote{Author: "bob", Body: "root cause was X"})
		if err != nil {
			t.Fatalf("AppendRunNote: %v", err)
		}
		if b.Seq != 1 {
			t.Errorf("second note seq = %d, want 1", b.Seq)
		}
	})

	t.Run("list returns notes in chronological order", func(t *testing.T) {
		got, err := s.ListRunNotes(ctx, runID)
		if err != nil {
			t.Fatalf("ListRunNotes: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("expected 2 notes, got %d", len(got))
		}
		if got[0].Body != "flaky, re-ran" || got[0].Author != "alice" || got[0].Seq != 0 {
			t.Errorf("note[0] = %+v, unexpected", got[0])
		}
		if got[1].Body != "root cause was X" || got[1].Author != "bob" || got[1].Seq != 1 {
			t.Errorf("note[1] = %+v, unexpected", got[1])
		}
	})

	t.Run("notes survive a fresh store handle (persisted, not in-memory)", func(t *testing.T) {
		s2, err := New(s.root)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		got, err := s2.ListRunNotes(ctx, runID)
		if err != nil {
			t.Fatalf("ListRunNotes: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("expected 2 persisted notes after reopen, got %d", len(got))
		}
	})

	t.Run("invalid run id is rejected", func(t *testing.T) {
		if _, err := s.AppendRunNote(ctx, "../escape", RunNote{Body: "x"}); err == nil {
			t.Errorf("expected error for path-traversal run id")
		}
	})
}

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

// TestRunNoteStore_ReattributeRunNotes: an erasure rewrites the author of
// every note the address wrote, in every run, however the address was
// spelled, and touches no other author's note.
func TestRunNoteStore_ReattributeRunNotes(t *testing.T) {
	ctx := context.Background()
	s := tmpStore(t)
	for _, run := range []string{"run-a", "run-b"} {
		if _, err := s.CreateRun(ctx, run, "wf", nil); err != nil {
			t.Fatalf("CreateRun %s: %v", run, err)
		}
	}
	for _, c := range []struct{ run, author string }{
		{"run-a", "alice@example.org"},
		{"run-a", "bob@example.org"},
		{"run-b", " Alice@Example.org "},
		{"run-b", "alice@example.org.uk"},
	} {
		if _, err := s.AppendRunNote(ctx, c.run, RunNote{Author: c.author, Body: "note"}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	if _, err := s.ReattributeRunNotes(ctx, " ", "u-1"); err == nil {
		t.Fatal("a reattribution naming no author answered no error")
	}
	n, err := s.ReattributeRunNotes(ctx, "ALICE@example.org", "u-1")
	if err != nil || n != 2 {
		t.Fatalf("ReattributeRunNotes = %d (%v), want 2", n, err)
	}
	got := map[string][]string{}
	for _, run := range []string{"run-a", "run-b"} {
		notes, err := s.ListRunNotes(ctx, run)
		if err != nil {
			t.Fatal(err)
		}
		for _, note := range notes {
			got[run] = append(got[run], note.Author)
		}
	}
	if want := []string{"u-1", "bob@example.org"}; !equalStrings(got["run-a"], want) {
		t.Fatalf("run-a authors = %v, want %v", got["run-a"], want)
	}
	if want := []string{"u-1", "alice@example.org.uk"}; !equalStrings(got["run-b"], want) {
		t.Fatalf("run-b authors = %v, want %v", got["run-b"], want)
	}
	if n, err := s.ReattributeRunNotes(ctx, "alice@example.org", "u-1"); err != nil || n != 0 {
		t.Fatalf("a second reattribution = %d (%v), want 0", n, err)
	}
}

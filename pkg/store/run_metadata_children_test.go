package store

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunMetadataChildrenFilesystemPlansNotes(t *testing.T) {
	t.Parallel()
	s, e := New(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	ctx := t.Context()
	if _, e = s.CreateRunWithValueFormat(ctx, RunCreateSpec{ID: "r", Format: 2}); e != nil {
		t.Fatal(e)
	}
	snap := PlanSnapshot{NodeID: "n", Todos: []PlanTodo{{Content: "first", Status: "pending"}}}
	if got, ok, e := s.AppendPlanSnapshot(ctx, "r", snap); e != nil || !ok || got.Seq != 0 {
		t.Fatal(got, ok, e)
	}
	if got, ok, e := s.AppendPlanSnapshot(ctx, "r", snap); e != nil || ok || got.Seq != 0 {
		t.Fatal(got, ok, e)
	}
	if got, e := s.AppendRunNote(ctx, "r", RunNote{Body: "first"}); e != nil || got.Seq != 0 {
		t.Fatal(got, e)
	}
	planPath := filepath.Join(s.runDir("r"), "plans", "0000.json")
	planBefore, e := os.ReadFile(planPath)
	if e != nil {
		t.Fatal(e)
	}
	raw, e := os.ReadFile(s.runJSONPath("r"))
	if e != nil {
		t.Fatal(e)
	}
	raw = []byte(strings.Replace(string(raw), `"format_version": 2`, `"format_version": 99`, 1))
	if e = os.WriteFile(s.runJSONPath("r"), raw, 0600); e != nil {
		t.Fatal(e)
	}
	snap.Todos[0].Content = "second"
	if _, ok, e := s.AppendPlanSnapshot(ctx, "r", snap); ok || !errors.Is(e, ErrUnsupportedValueFormat) {
		t.Fatal(ok, e)
	}
	if _, e := s.AppendRunNote(ctx, "r", RunNote{Body: "second"}); !errors.Is(e, ErrUnsupportedValueFormat) {
		t.Fatal(e)
	}
	current, e := os.ReadFile(s.runJSONPath("r"))
	if e != nil || !bytes.Equal(raw, current) {
		t.Fatal("refusal changed parent", e)
	}
	if _, e := s.ListPlanSnapshots(ctx, "r"); !errors.Is(e, ErrUnsupportedValueFormat) {
		t.Fatal(e)
	}
	planAfter, e := os.ReadFile(planPath)
	if e != nil || !bytes.Equal(planBefore, planAfter) {
		t.Fatal("refusal changed plan bytes", e)
	}
	seqs, e := planSeqs(filepath.Dir(planPath))
	if e != nil || len(seqs) != 1 {
		t.Fatal(seqs, e)
	}
	if _, e := s.ListRunNotes(ctx, "r"); !errors.Is(e, ErrUnsupportedValueFormat) {
		t.Fatal(e)
	}
	noteDir := filepath.Join(s.runDir("r"), "notes")
	notes, e := noteSeqs(noteDir)
	if e != nil || len(notes) != 1 {
		t.Fatal(notes, e)
	}
	note, e := readNoteFile(noteDir, notes[0])
	if e != nil || note.Body != "first" {
		t.Fatal(note, e)
	}
}

package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func exactBirthSpec(id string) RunCreateSpec {
	return RunCreateSpec{ID: id, WorkflowName: "wf", ParentRunID: "parent", FilePath: "main.bot", BotID: "bot", Status: RunStatusQueued, Format: ExactValueFormat, Inputs: map[string]any{"n": json.Number("1e400"), "i": int64(9007199254740993), "nil": []any(nil), "empty": []any{}}}
}
func requireExactBirth(t *testing.T, r *Run) {
	t.Helper()
	if r.FormatVersion != 2 || r.ParentRunID != "parent" || r.FilePath != "main.bot" || r.BotID != "bot" || r.Status != RunStatusQueued || r.QueuedAt == nil || r.Inputs["n"] != json.Number("1e400") || r.Inputs["i"] != int64(9007199254740993) {
		t.Fatalf("bad birth: %+v inputs=%#v", r, r.Inputs)
	}
}
func TestValueFormatFilesystemAtomicBirth(t *testing.T) {
	t.Parallel()
	s, e := New(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	if _, err := s.CreateRun(t.Context(), "parent", "wf", nil); err != nil {
		t.Fatal(err)
	}
	r, e := CreateRunWithValueFormat(t.Context(), s, exactBirthSpec("r"))
	if e != nil {
		t.Fatal(e)
	}
	requireExactBirth(t, r)
	got, e := s.LoadRun(t.Context(), "r")
	if e != nil {
		t.Fatal(e)
	}
	requireExactBirth(t, got)
	raw, e := os.ReadFile(s.runJSONPath("r"))
	if e != nil {
		t.Fatal(e)
	}
	var doc map[string]json.RawMessage
	if e = json.Unmarshal(raw, &doc); e != nil {
		t.Fatal(e)
	}
	if !bytes.HasPrefix(bytes.TrimSpace(doc["inputs"]), []byte("[")) {
		t.Fatal("exact input frame absent", string(raw))
	}
	before := append([]byte(nil), raw...)
	if _, e = CreateRunWithValueFormat(t.Context(), s, exactBirthSpec("r")); e == nil {
		t.Fatal("overwrote existing birth")
	}
	raw, e = os.ReadFile(s.runJSONPath("r"))
	if e != nil || !bytes.Equal(before, raw) {
		t.Fatal("failed birth changed existing", e)
	}
}
func TestValueFormatFilesystemSaveAndMetadata(t *testing.T) {
	t.Parallel()
	s, e := New(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	if _, err := s.CreateRun(t.Context(), "parent", "wf", nil); err != nil {
		t.Fatal(err)
	}
	if _, e = s.CreateRunWithValueFormat(t.Context(), exactBirthSpec("r")); e != nil {
		t.Fatal(e)
	}
	r, e := s.LoadRun(t.Context(), "r")
	if e != nil {
		t.Fatal(e)
	}
	r.Name = "renamed"
	if e = s.SaveRun(t.Context(), r); e != nil {
		t.Fatal(e)
	}
	if e = s.UpdateRunStatus(t.Context(), "r", RunStatusRunning, ""); e != nil {
		t.Fatal(e)
	}
	r, e = s.LoadRun(t.Context(), "r")
	if e != nil || r.FormatVersion != 2 || r.Inputs["n"] != json.Number("1e400") {
		t.Fatal(r, e)
	}
	before, e := os.ReadFile(s.runJSONPath("r"))
	if e != nil {
		t.Fatal(e)
	}
	for _, f := range []int{1, 99} {
		copy := *r
		copy.FormatVersion = f
		e = s.SaveRun(t.Context(), &copy)
		want := ErrValueFormatChanged
		if f == 99 {
			want = ErrUnsupportedValueFormat
		}
		if !errors.Is(e, want) {
			t.Fatal(f, e)
		}
		after, e := os.ReadFile(s.runJSONPath("r"))
		if e != nil || !bytes.Equal(before, after) {
			t.Fatal("refusal changed record", e)
		}
	}
	ctx, e := WithRunValueFormat(t.Context(), RunValueFormat{"r", 1})
	if e != nil {
		t.Fatal(e)
	}
	if e = s.SaveRun(ctx, r); !errors.Is(e, ErrValueFormatChanged) {
		t.Fatal(e)
	}
	absent := &Run{ID: "absent", FormatVersion: 2}
	if e = s.SaveRun(t.Context(), absent); !errors.Is(e, ErrValueFormatCapability) {
		t.Fatal(e)
	}
	if _, e = os.Stat(s.runDir("absent")); !os.IsNotExist(e) {
		t.Fatal("invalid exact upsert created namespace", e)
	}
}
func TestValueFormatFilesystemBirthOrphanControls(t *testing.T) {
	t.Parallel()
	s, e := New(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	if _, err := s.CreateRun(t.Context(), "parent", "wf", nil); err != nil {
		t.Fatal(err)
	}
	orphan := &Artifact{RunID: "orphan", NodeID: "n", Version: 1, Data: map[string]any{"old": "legacy"}}
	if e = s.WriteArtifact(t.Context(), orphan); e != nil {
		t.Fatal(e)
	}
	if _, e = s.CreateRunWithValueFormat(t.Context(), exactBirthSpec("orphan")); !errors.Is(e, ErrInvalidValueFormat) {
		t.Fatal(e)
	}
	a, e := s.LoadArtifact(t.Context(), "orphan", "n", 1)
	if e != nil || a.Data["old"] != "legacy" {
		t.Fatal(a, e)
	}
	if e = os.MkdirAll(s.runDir("logs"), 0700); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(s.runDir("logs"), "run.log"), []byte("preparation\n"), 0600); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(s.runDir("logs"), "events.jsonl"), nil, 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = s.CreateRunWithValueFormat(t.Context(), exactBirthSpec("logs")); e != nil {
		t.Fatal(e)
	}
	legacy, e := s.CreateRun(t.Context(), "legacy", "wf", map[string]any{"i": int64(9007199254740993)})
	if e != nil || legacy.FormatVersion != 1 {
		t.Fatal(legacy, e)
	}
	legacy, e = s.LoadRun(t.Context(), "legacy")
	if e != nil || legacy.Inputs["i"] != float64(9007199254740992) {
		t.Fatal(legacy, e)
	}
}
func TestValueFormatFilesystemStaleWholeSave(t *testing.T) {
	t.Parallel()
	s, e := New(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	if _, err := s.CreateRun(t.Context(), "parent", "wf", nil); err != nil {
		t.Fatal(err)
	}
	old, e := s.CreateRunWithValueFormat(t.Context(), exactBirthSpec("r"))
	if e != nil {
		t.Fatal(e)
	}
	if e = s.DeleteRun(t.Context(), "r"); e != nil {
		t.Fatal(e)
	}
	if _, e = s.PruneDeletionMarkers(t.Context(), time.Now().Add(time.Hour)); e != nil {
		t.Fatal(e)
	}
	if _, e = s.CreateRun(t.Context(), "r", "new", nil); e != nil {
		t.Fatal(e)
	}
	if e = s.SaveRun(t.Context(), old); !errors.Is(e, ErrValueFormatChanged) {
		t.Fatal(e)
	}
	now, e := s.LoadRun(t.Context(), "r")
	if e != nil || now.WorkflowName != "new" || now.FormatVersion != 1 {
		t.Fatal(now, e)
	}
}

package store

import (
	"encoding/json"
	"os"
	"testing"
)

func TestBirthIdentityLegacyAbsenceRemainsAbsent(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.CreateRun(t.Context(), "legacy", "wf", nil)
	if err != nil {
		t.Fatal(err)
	}
	// An actual historical record has no field; an empty field on a current
	// caller is not equivalent to this persisted preimage.
	b, err := os.ReadFile(s.runJSONPath(r.ID))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err = json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	delete(raw, "birth_id")
	b, err = json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(s.runJSONPath(r.ID), b, filePerm); err != nil {
		t.Fatal(err)
	}
	r, err = s.LoadRun(t.Context(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	r.Name = "legacy metadata"
	if err = s.SaveRun(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadRun(t.Context(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.BirthID != "" || r.BirthID != "" {
		t.Fatal("save invented legacy birth identity")
	}
}

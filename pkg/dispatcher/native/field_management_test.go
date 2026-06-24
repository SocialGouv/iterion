package native

import (
	"testing"
)

// fieldBoard seeds a store with one state + a couple of custom fields.
func fieldBoard(t *testing.T) *Store {
	t.Helper()
	s := newTestStore(t)
	if err := s.SetBoard(&Board{
		States: []State{{Name: "ready"}},
		Fields: []Field{
			{Name: "sev", Type: FieldEnum, EnumValues: []string{"low", "high"}},
			{Name: "owner", Type: FieldText},
		},
	}); err != nil {
		t.Fatalf("SetBoard: %v", err)
	}
	return s
}

func TestAddField(t *testing.T) {
	s := fieldBoard(t)
	if err := s.AddField(Field{Name: "eta", Type: FieldDate}); err != nil {
		t.Fatalf("AddField: %v", err)
	}
	if s.Board().FieldByName("eta") == nil {
		t.Fatal("eta not added")
	}
	if err := s.AddField(Field{Name: "eta", Type: FieldText}); err == nil {
		t.Fatal("expected duplicate rejection")
	}
	// enum without values is rejected by board validation
	if err := s.AddField(Field{Name: "bad", Type: FieldEnum}); err == nil {
		t.Fatal("expected enum-needs-values rejection")
	}
}

func TestRenameFieldCascades(t *testing.T) {
	s := fieldBoard(t)
	iss, _ := s.Create(Issue{Title: "x", State: "ready", Fields: map[string]any{"owner": "jo", "sev": "high"}})

	touched, err := s.RenameField("owner", "assignee_name")
	if err != nil {
		t.Fatalf("RenameField: %v", err)
	}
	if touched != 1 {
		t.Fatalf("touched = %d, want 1", touched)
	}
	if s.Board().FieldByName("owner") != nil || s.Board().FieldByName("assignee_name") == nil {
		t.Fatal("schema rename not applied")
	}
	got, _ := s.Get(iss.ID)
	if _, ok := got.Fields["owner"]; ok {
		t.Fatal("old key still present on issue")
	}
	if got.Fields["assignee_name"] != "jo" {
		t.Fatalf("value not carried: %+v", got.Fields)
	}
	// onto-existing refused
	if _, err := s.RenameField("sev", "assignee_name"); err == nil {
		t.Fatal("expected onto-existing rejection")
	}
}

func TestUpdateField(t *testing.T) {
	s := fieldBoard(t)
	disp := "Severity"
	req := true
	if err := s.UpdateField("sev", FieldPatch{Display: &disp, Required: &req}); err != nil {
		t.Fatalf("UpdateField: %v", err)
	}
	f := s.Board().FieldByName("sev")
	if f.Display != disp || !f.Required {
		t.Fatalf("update not applied: %+v", f)
	}
	if err := s.UpdateField("nope", FieldPatch{}); err == nil {
		t.Fatal("expected unknown-field error")
	}
}

func TestDeleteFieldStripsIssues(t *testing.T) {
	s := fieldBoard(t)
	iss, _ := s.Create(Issue{Title: "x", State: "ready", Fields: map[string]any{"owner": "jo", "sev": "high"}})

	touched, err := s.DeleteField("owner")
	if err != nil {
		t.Fatalf("DeleteField: %v", err)
	}
	if touched != 1 {
		t.Fatalf("touched = %d, want 1", touched)
	}
	if s.Board().FieldByName("owner") != nil {
		t.Fatal("field not removed from schema")
	}
	got, _ := s.Get(iss.ID)
	if _, ok := got.Fields["owner"]; ok {
		t.Fatal("owner key not stripped from issue")
	}
	if got.Fields["sev"] != "high" {
		t.Fatal("unrelated field lost")
	}
	// A subsequent update must still validate (no orphaned key).
	if _, err := s.Update(iss.ID, Patch{Fields: map[string]any{"sev": "low"}}); err != nil {
		t.Fatalf("post-delete update: %v", err)
	}
}

func TestReorderFields(t *testing.T) {
	s := fieldBoard(t)
	if err := s.ReorderFields([]string{"owner", "sev"}); err != nil {
		t.Fatalf("ReorderFields: %v", err)
	}
	if s.Board().Fields[0].Name != "owner" {
		t.Fatal("reorder not applied")
	}
	if err := s.ReorderFields([]string{"owner"}); err == nil {
		t.Fatal("expected wrong-length rejection")
	}
	if err := s.ReorderFields([]string{"owner", "nope"}); err == nil {
		t.Fatal("expected unknown-field rejection")
	}
}

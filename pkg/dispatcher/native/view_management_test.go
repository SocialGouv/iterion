package native

import "testing"

func TestSaveAndDeleteView(t *testing.T) {
	s := newTestStore(t)

	if err := s.SaveView(View{Name: "Mine", Assignee: "jo", Sort: "priority", GroupBy: "assignee"}); err != nil {
		t.Fatalf("SaveView: %v", err)
	}
	if len(s.Board().Views) != 1 {
		t.Fatalf("want 1 view, got %d", len(s.Board().Views))
	}

	// Upsert by name (replace, not append).
	if err := s.SaveView(View{Name: "Mine", Assignee: "alice"}); err != nil {
		t.Fatalf("SaveView upsert: %v", err)
	}
	views := s.Board().Views
	if len(views) != 1 || views[0].Assignee != "alice" {
		t.Fatalf("upsert did not replace: %+v", views)
	}

	// Empty name rejected.
	if err := s.SaveView(View{Name: ""}); err == nil {
		t.Fatal("expected empty-name rejection")
	}

	// Delete.
	if err := s.DeleteView("Mine"); err != nil {
		t.Fatalf("DeleteView: %v", err)
	}
	if len(s.Board().Views) != 0 {
		t.Fatal("view not deleted")
	}
	if err := s.DeleteView("nope"); err == nil {
		t.Fatal("expected unknown-view error")
	}
}

func TestViewsPersist(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := s.SaveView(View{Name: "Triage", Labels: []string{"bug"}, GroupBy: "label"}); err != nil {
		t.Fatalf("SaveView: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s2, err := NewStore(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	views := s2.Board().Views
	if len(views) != 1 || views[0].Name != "Triage" || views[0].GroupBy != "label" {
		t.Fatalf("view did not persist: %+v", views)
	}
}

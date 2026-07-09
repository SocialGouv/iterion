package skilllib

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestStore(t *testing.T) (*Store, string, string) {
	t.Helper()
	root := t.TempDir()
	global := filepath.Join(root, "global", "skills")
	project := filepath.Join(root, "project", "skills")
	return NewStore(global, project), global, project
}

func TestPutGetGlobal(t *testing.T) {
	s, global, _ := newTestStore(t)
	body := "---\nname: changelog-writer\ndescription: Writes changelogs\n---\n# body\n"
	if err := s.Put("changelog-writer", body, ScopeGlobal); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// Directory form written.
	if _, err := os.Stat(filepath.Join(global, "changelog-writer", "SKILL.md")); err != nil {
		t.Fatalf("expected directory-form skill: %v", err)
	}
	got, err := s.Get("changelog-writer")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Description != "Writes changelogs" {
		t.Errorf("Description = %q", got.Description)
	}
	if got.Scope != ScopeGlobal {
		t.Errorf("Scope = %q", got.Scope)
	}
	if got.Body != body {
		t.Errorf("Body mismatch")
	}
}

func TestProjectShadowsGlobal(t *testing.T) {
	s, _, _ := newTestStore(t)
	if err := s.Put("x", "---\ndescription: global one\n---\n", ScopeGlobal); err != nil {
		t.Fatal(err)
	}
	if err := s.Put("x", "---\ndescription: project one\n---\n", ScopeProject); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get("x")
	if err != nil {
		t.Fatal(err)
	}
	if got.Scope != ScopeProject || got.Description != "project one" {
		t.Errorf("project should shadow global, got scope=%q desc=%q", got.Scope, got.Description)
	}
	// List collapses to one entry, the project one.
	list, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Scope != ScopeProject {
		t.Errorf("List = %+v, want single project entry", list)
	}
}

func TestListSortedAcrossScopes(t *testing.T) {
	s, _, _ := newTestStore(t)
	mustPut(t, s, "beta", ScopeGlobal)
	mustPut(t, s, "alpha", ScopeProject)
	mustPut(t, s, "gamma", ScopeGlobal)
	list, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, sk := range list {
		names = append(names, sk.Name)
	}
	if strings.Join(names, ",") != "alpha,beta,gamma" {
		t.Errorf("names = %v, want sorted alpha,beta,gamma", names)
	}
}

func TestResolvePrefersProject(t *testing.T) {
	s, _, project := newTestStore(t)
	mustPut(t, s, "dup", ScopeGlobal)
	mustPut(t, s, "dup", ScopeProject)
	path, ok := s.Resolve("dup")
	if !ok {
		t.Fatal("Resolve dup not found")
	}
	if !strings.HasPrefix(path, project) {
		t.Errorf("Resolve = %q, want under project dir %q", path, project)
	}
	if _, ok := s.Resolve("missing"); ok {
		t.Error("Resolve missing should be false")
	}
}

func TestFlatFormRead(t *testing.T) {
	s, global, _ := newTestStore(t)
	// Hand-place a flat <name>.md (as a git-imported pack might).
	if err := os.MkdirAll(global, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(global, "flat.md"), []byte("---\ndescription: flat skill\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get("flat")
	if err != nil {
		t.Fatalf("Get flat: %v", err)
	}
	if got.Description != "flat skill" {
		t.Errorf("Description = %q", got.Description)
	}
	// Put on an existing flat file rewrites in place (no directory form created).
	if err := s.Put("flat", "---\ndescription: updated\n---\n", ScopeGlobal); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(global, "flat", "SKILL.md")); !os.IsNotExist(err) {
		t.Error("Put should have rewritten the flat file in place, not created a dir")
	}
}

func TestRemove(t *testing.T) {
	s, _, _ := newTestStore(t)
	mustPut(t, s, "gone", ScopeGlobal)
	if err := s.Remove("gone", ScopeGlobal); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, ok := s.Resolve("gone"); ok {
		t.Error("skill should be gone after Remove")
	}
	if err := s.Remove("gone", ScopeGlobal); err == nil {
		t.Error("Remove of missing skill should error")
	}
}

func TestProjectScopeUnavailable(t *testing.T) {
	globalOnly := NewStore(t.TempDir(), "")
	if err := globalOnly.Put("x", "body", ScopeProject); err == nil {
		t.Error("project scope Put should fail when no project layer")
	}
}

func TestValidName(t *testing.T) {
	for _, bad := range []string{"", ".", "..", "a/b", "a\\b", ".hidden"} {
		if err := ValidName(bad); err == nil {
			t.Errorf("ValidName(%q) should error", bad)
		}
	}
	if err := ValidName("ok-name_1"); err != nil {
		t.Errorf("ValidName(ok-name_1) = %v", err)
	}
}

func TestNewStoreCollapsesEqualDirs(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir, dir)
	if s.HasProject() {
		t.Error("equal global/project dirs should collapse to global-only")
	}
}

func mustPut(t *testing.T, s *Store, name, scope string) {
	t.Helper()
	if err := s.Put(name, "---\ndescription: d\n---\n", scope); err != nil {
		t.Fatalf("Put %q: %v", name, err)
	}
}

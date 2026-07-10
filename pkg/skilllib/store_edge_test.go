package skilllib

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Get must reject a name that fails ValidName BEFORE touching the disk.
func TestGetInvalidNameRejected(t *testing.T) {
	s, _, _ := newTestStore(t)
	if _, err := s.Get("../escape"); err == nil {
		t.Error("Get of a path-escaping name should error")
	}
}

// Get on a name that resolves to no file returns a not-found error.
func TestGetMissingErrors(t *testing.T) {
	s, _, _ := newTestStore(t)
	_, err := s.Get("nope")
	if err == nil {
		t.Fatal("Get of a missing skill should error")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %v, want a not-found message", err)
	}
}

// dirForScope must reject an unknown scope through Put.
func TestPutUnknownScopeErrors(t *testing.T) {
	s, _, _ := newTestStore(t)
	if err := s.Put("x", "body", "staging"); err == nil {
		t.Error("Put with an unknown scope should error")
	}
}

// Put must reject an invalid name before writing anything.
func TestPutInvalidNameRejected(t *testing.T) {
	s, global, _ := newTestStore(t)
	if err := s.Put("a/b", "body", ScopeGlobal); err == nil {
		t.Error("Put with a path-separator name should error")
	}
	if _, err := os.Stat(global); err == nil {
		t.Error("no directory should have been created for a rejected name")
	}
}

// Remove of the directory form removes the whole <name>/ dir, including any
// auxiliary files it carries (not just SKILL.md).
func TestRemoveDirectoryFormRemovesAuxFiles(t *testing.T) {
	s, global, _ := newTestStore(t)
	mustPut(t, s, "kit", ScopeGlobal)
	// Add an auxiliary file next to SKILL.md.
	aux := filepath.Join(global, "kit", "reference.txt")
	if err := os.WriteFile(aux, []byte("aux"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove("kit", ScopeGlobal); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(filepath.Join(global, "kit")); !os.IsNotExist(err) {
		t.Error("the whole skill directory (incl. aux files) should be gone")
	}
}

// Remove of the flat form removes just the file.
func TestRemoveFlatForm(t *testing.T) {
	s, global, _ := newTestStore(t)
	if err := os.MkdirAll(global, 0o755); err != nil {
		t.Fatal(err)
	}
	flat := filepath.Join(global, "solo.md")
	if err := os.WriteFile(flat, []byte("---\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove("solo", ScopeGlobal); err != nil {
		t.Fatalf("Remove flat: %v", err)
	}
	if _, err := os.Stat(flat); !os.IsNotExist(err) {
		t.Error("flat skill file should be removed")
	}
}

// Remove must surface scope/name validation errors too.
func TestRemoveInvalidScopeAndName(t *testing.T) {
	s, _, _ := newTestStore(t)
	if err := s.Remove("x", "bogus"); err == nil {
		t.Error("Remove with an unknown scope should error")
	}
	if err := s.Remove("..", ScopeGlobal); err == nil {
		t.Error("Remove with an invalid name should error")
	}
}

// A directory named <name>/ WITHOUT a SKILL.md, a non-.md file, and a hidden
// entry must all be skipped by listing/resolution.
func TestListDirSkipsNonSkills(t *testing.T) {
	s, global, _ := newTestStore(t)
	if err := os.MkdirAll(filepath.Join(global, "empty-dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(global, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(global, ".hidden.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(global, ".hidden-dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(global, ".hidden-dir", "SKILL.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// One real skill so the store is non-empty.
	mustPut(t, s, "real", ScopeGlobal)

	list, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	// List enumeration skips the empty dir, the .txt, and both hidden entries.
	if len(list) != 1 || list[0].Name != "real" {
		t.Fatalf("List = %+v, want only the real skill", list)
	}
	// Resolution: a directory without SKILL.md and a non-.md file must not
	// resolve. (Dotfile handling is an enumeration concern asserted via List
	// above; Resolve trusts a ValidName-checked caller — see package summary.)
	for _, bad := range []string{"empty-dir", "notes"} {
		if _, ok := s.Resolve(bad); ok {
			t.Errorf("Resolve(%q) should not resolve a non-skill entry", bad)
		}
	}
}

// The on-disk name is canonical: a frontmatter `name:` must NOT override the
// basename used by Get/List/Resolve (documented invariant on LibrarySkill).
func TestFrontmatterNameDoesNotOverrideCanonicalName(t *testing.T) {
	s, _, _ := newTestStore(t)
	body := "---\nname: totally-different\ndescription: d\n---\n"
	if err := s.Put("canonical", body, ScopeGlobal); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get("canonical")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "canonical" {
		t.Errorf("Get().Name = %q, want the on-disk name %q (frontmatter must not override)", got.Name, "canonical")
	}
	list, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != "canonical" {
		t.Errorf("List name = %+v, want on-disk name", list)
	}
}

// List omits bodies (for brevity) while Get includes them — a documented
// contract downstream callers rely on.
func TestListOmitsBodyGetIncludesIt(t *testing.T) {
	s, _, _ := newTestStore(t)
	body := "---\ndescription: d\n---\n# real content\n"
	if err := s.Put("withbody", body, ScopeGlobal); err != nil {
		t.Fatal(err)
	}
	list, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Body != "" {
		t.Errorf("List should omit body, got %q", list[0].Body)
	}
	got, err := s.Get("withbody")
	if err != nil {
		t.Fatal(err)
	}
	if got.Body != body {
		t.Errorf("Get body = %q, want full body", got.Body)
	}
}

// LocalStoreForProject wires the project layer only when given a distinct,
// non-empty project store dir.
func TestLocalStoreForProject(t *testing.T) {
	globalOnly := LocalStoreForProject("")
	if globalOnly.HasProject() {
		t.Error(`LocalStoreForProject("") should be global-only`)
	}
	withProj := LocalStoreForProject(t.TempDir())
	if !withProj.HasProject() {
		t.Error("LocalStoreForProject(dir) should enable the project layer")
	}
	// The project layer must live under <projectStoreDir>/skills.
	dir := t.TempDir()
	s := LocalStoreForProject(dir)
	if err := s.Put("p", "---\ndescription: d\n---\n", ScopeProject); err != nil {
		t.Fatalf("Put to project: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, SkillsDirName, "p", "SKILL.md")); err != nil {
		t.Errorf("expected skill under <dir>/%s/: %v", SkillsDirName, err)
	}
}

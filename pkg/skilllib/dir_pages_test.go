package skilllib

import (
	"os"
	"path/filepath"
	"testing"
)

// TestListDirPagesReadsBothCanonicalForms pins the shared reading of a
// skills/ directory: both forms are pages, and the Rel path names the FILE an
// operator opens — `<name>/SKILL.md` for the directory form, never the
// directory. Every host asks here (the library store, `iterion validate`'s
// bundle scan, the catalogue guards), so a form missed here goes missing
// everywhere at once.
func TestListDirPagesReadsBothCanonicalForms(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("flat.md", "---\nname: flat\ndescription: d\n---\n")
	write("nested/SKILL.md", "---\nname: nested\ndescription: d\n---\n")
	write("notes.txt", "x")
	write(".hidden.md", "x")
	write(".hidden-dir/SKILL.md", "x")
	write("nested/reference.md", "an auxiliary file, not a page of its own")
	if err := os.MkdirAll(filepath.Join(dir, "empty-dir"), 0o755); err != nil {
		t.Fatal(err)
	}

	pages, err := ListDirPages(dir)
	if err != nil {
		t.Fatalf("ListDirPages: %v", err)
	}
	got := map[string]string{}
	for _, p := range pages {
		got[p.Name] = p.Rel
		if _, err := os.Stat(p.Path); err != nil {
			t.Errorf("page %q: Path %q does not exist: %v", p.Name, p.Path, err)
		}
	}
	want := map[string]string{"flat": "flat.md", "nested": "nested/SKILL.md"}
	if len(got) != len(want) {
		t.Fatalf("ListDirPages = %v, want exactly %v", got, want)
	}
	for name, rel := range want {
		if got[name] != rel {
			t.Errorf("page %q: Rel = %q, want %q", name, got[name], rel)
		}
	}
}

// TestListDirPagesOnAMissingDirIsNotAnError keeps a bundle without skills
// ordinary: no pages, no error, so every caller's check no-ops instead of
// reporting filesystem health.
func TestListDirPagesOnAMissingDirIsNotAnError(t *testing.T) {
	pages, err := ListDirPages(filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatalf("ListDirPages on a missing dir: %v", err)
	}
	if len(pages) != 0 {
		t.Fatalf("ListDirPages on a missing dir = %v, want none", pages)
	}
}

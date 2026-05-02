package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// gitRepo initialises a fresh repo in t.TempDir() with a single committed
// file ("a.txt") so each test can mutate from a known baseline. The git
// CLI is required on PATH; tests are skipped otherwise so CI without git
// (rare but possible in stripped containers) doesn't fail with a confusing
// exec error.
func gitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	mustRun(t, dir, "init", "-q", "-b", "main")
	mustRun(t, dir, "config", "user.email", "test@example.com")
	mustRun(t, dir, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, dir, "add", "a.txt")
	mustRun(t, dir, "commit", "-q", "-m", "init")
	return dir
}

func mustRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestStatusEmptyClean(t *testing.T) {
	dir := gitRepo(t)
	files, err := Status(dir)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(files) != 0 {
		t.Fatalf("expected no files, got %+v", files)
	}
}

func TestStatusModifiedAddedDeletedUntracked(t *testing.T) {
	dir := gitRepo(t)
	// Modify a.txt
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// New untracked file
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Add another file then delete it after committing.
	if err := os.WriteFile(filepath.Join(dir, "c.txt"), []byte("c\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, dir, "add", "c.txt")
	mustRun(t, dir, "commit", "-q", "-m", "add c")
	if err := os.Remove(filepath.Join(dir, "c.txt")); err != nil {
		t.Fatal(err)
	}

	files, err := Status(dir)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	got := map[string]string{}
	for _, f := range files {
		got[f.Path] = f.Status
	}
	if got["a.txt"] != "M" {
		t.Errorf("a.txt: want M, got %q", got["a.txt"])
	}
	if got["b.txt"] != "??" {
		t.Errorf("b.txt: want ??, got %q", got["b.txt"])
	}
	if got["c.txt"] != "D" {
		t.Errorf("c.txt: want D, got %q", got["c.txt"])
	}
}

func TestStatusRename(t *testing.T) {
	dir := gitRepo(t)
	mustRun(t, dir, "mv", "a.txt", "renamed.txt")
	mustRun(t, dir, "add", "-A")
	files, err := Status(dir)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("want 1 entry, got %+v", files)
	}
	f := files[0]
	if f.Status != "R" {
		t.Errorf("status: want R, got %q", f.Status)
	}
	if f.Path != "renamed.txt" {
		t.Errorf("path: want renamed.txt, got %q", f.Path)
	}
	if f.OldPath != "a.txt" {
		t.Errorf("oldpath: want a.txt, got %q", f.OldPath)
	}
}

func TestStatusNotGitRepo(t *testing.T) {
	dir := t.TempDir()
	_, err := Status(dir)
	if err != ErrNotGitRepo {
		t.Fatalf("want ErrNotGitRepo, got %v", err)
	}
}

func TestDiffModified(t *testing.T) {
	dir := gitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := Diff(dir, "a.txt")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if d.Before == nil || *d.Before != "hello\n" {
		t.Errorf("before: %v", d.Before)
	}
	if d.After == nil || *d.After != "changed\n" {
		t.Errorf("after: %v", d.After)
	}
	if d.Binary {
		t.Errorf("Binary should be false for text diff")
	}
}

func TestDiffUntracked(t *testing.T) {
	dir := gitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "fresh.txt"), []byte("brand new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := Diff(dir, "fresh.txt")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if d.Before != nil {
		t.Errorf("before should be nil for untracked, got %q", *d.Before)
	}
	if d.After == nil || *d.After != "brand new\n" {
		t.Errorf("after: %v", d.After)
	}
}

func TestDiffDeleted(t *testing.T) {
	dir := gitRepo(t)
	if err := os.Remove(filepath.Join(dir, "a.txt")); err != nil {
		t.Fatal(err)
	}
	d, err := Diff(dir, "a.txt")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if d.Before == nil || *d.Before != "hello\n" {
		t.Errorf("before: %v", d.Before)
	}
	if d.After != nil {
		t.Errorf("after should be nil for deleted, got %q", *d.After)
	}
}

func TestDiffBinary(t *testing.T) {
	dir := gitRepo(t)
	// A NUL byte is the canonical "binary" signal git itself uses.
	if err := os.WriteFile(filepath.Join(dir, "blob.bin"), []byte{0x00, 0x01, 0x02}, 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := Diff(dir, "blob.bin")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if !d.Binary {
		t.Errorf("Binary should be true")
	}
	if d.Before != nil || d.After != nil {
		t.Errorf("Before/After should be nil for binary, got %v / %v", d.Before, d.After)
	}
}

func TestValidateRelPath(t *testing.T) {
	cases := []struct {
		in      string
		wantErr bool
	}{
		{"a.txt", false},
		{"sub/a.txt", false},
		{"", true},
		{"/etc/passwd", true},
		{"../escape", true},
		{"sub/../../escape", true},
		{"with\x00nul", true},
	}
	for _, c := range cases {
		err := ValidateRelPath(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("ValidateRelPath(%q): wantErr=%v, got %v", c.in, c.wantErr, err)
		}
	}
}

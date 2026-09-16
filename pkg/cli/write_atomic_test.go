package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// A rewrite lands whole, with the mode asked for, and leaves no temporary
// file beside the target.
func TestAWriteLandsWholeAndLeavesNothingBeside(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.bot")
	if err := os.WriteFile(path, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(path, []byte("after"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "after" {
		t.Fatalf("read %q, %v", got, err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("mode %v, want 0644 — the mode asked for, not the temporary file\x27s", info.Mode().Perm())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("something was left beside the file: %v", entries)
	}
	if err := writeFileAtomic(filepath.Join(dir, "nope", "x.bot"), []byte("x"), 0o644); err == nil {
		t.Fatal("a write into a missing directory succeeded")
	}
	// A rename that fails — the target is a directory — leaves the
	// temporary file nowhere.
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "keep"), []byte("k"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(sub, []byte("x"), 0o644); err == nil {
		t.Fatal("a write over a directory succeeded")
	}
	entries, _ = os.ReadDir(dir)
	if len(entries) != 2 {
		t.Fatalf("a temporary file was left after a failed rename: %v", entries)
	}
}

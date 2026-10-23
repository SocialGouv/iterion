package botscaffold

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// Scaffold publishes the bundle whole or not at all, and never takes a name
// another writer created: a directory created at dir after the scaffold's
// absence check — here holding a README.md — keeps its bytes and gains
// nothing of the scaffold (no main.bot, no manifest.yaml: no bot to list or
// route), the scaffold answers fs.ErrExist, and its staging directory is
// gone.
func TestScaffoldCreatesEveryFileAndReplacesNone(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "my-bot")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	theirs := "another writer's README\n"
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(theirs), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Scaffold(dir, minimalSpec())
	if !errors.Is(err, fs.ErrExist) {
		t.Fatalf("Scaffold over a directory another writer created = %v, want fs.ErrExist", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != theirs {
		t.Fatalf("README.md is %d bytes, want the %d bytes the other writer wrote:\n%s", len(got), len(theirs), got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("the refused scaffold left %v in the directory, want the other writer's README.md alone", names)
	}
	if staged, _ := filepath.Glob(filepath.Join(parent, ".my-bot.scaffold-*")); len(staged) > 0 {
		t.Fatalf("the refused scaffold left its staging directory behind: %v", staged)
	}
}

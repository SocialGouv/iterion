package commands

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDirCommands(t *testing.T) {
	dir := t.TempDir()
	cmdDir := filepath.Join(dir, ".claude", "commands")
	if err := os.MkdirAll(cmdDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// One command with frontmatter description, one without.
	if err := os.WriteFile(filepath.Join(cmdDir, "ship.md"),
		[]byte("---\ndescription: ship the change\n---\nRun tests then commit.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cmdDir, "review.md"),
		[]byte("Review the diff carefully.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := NewRegistry()
	if err := LoadDirCommands(r, dir); err != nil {
		t.Fatalf("LoadDirCommands: %v", err)
	}

	ship, ok := r.Lookup("ship")
	if !ok {
		t.Fatal("/ship not registered")
	}
	if ship.Description != "ship the change" {
		t.Errorf("ship description = %q, want frontmatter value", ship.Description)
	}
	if ship.Category != CategoryPlugin {
		t.Errorf("ship category = %q, want plugin", ship.Category)
	}
	review, ok := r.Lookup("review")
	if !ok {
		t.Fatal("/review not registered")
	}
	if review.Description != "Review the diff carefully." {
		t.Errorf("review description = %q, want first body line", review.Description)
	}

	// A command already registered (e.g. a builtin) is not overridden.
	r2 := NewRegistry()
	r2.Register(Command{Name: "/ship", Description: "builtin ship"})
	if err := LoadDirCommands(r2, dir); err != nil {
		t.Fatal(err)
	}
	if got, _ := r2.Lookup("ship"); got.Description != "builtin ship" {
		t.Errorf("existing /ship overridden: %q", got.Description)
	}
}

func TestLoadDirCommands_NoDir(t *testing.T) {
	r := NewRegistry()
	if err := LoadDirCommands(r, t.TempDir()); err != nil {
		t.Fatalf("missing .claude/commands should be a no-op, got %v", err)
	}
}

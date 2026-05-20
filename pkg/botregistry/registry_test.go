package botregistry

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestList_SortsAndDedups(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "b.bot"), `## ---
## name: zebra
## ---
agent x:
  model: "test"
`)
	writeFile(t, filepath.Join(dir, "a.bot"), `## ---
## name: alpha
## ---
agent y:
  model: "test"
`)
	entries, err := List(ListOptions{Paths: []string{dir}})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries", len(entries))
	}
	if entries[0].Name != "alpha" || entries[1].Name != "zebra" {
		t.Errorf("entries not sorted: %v", entries)
	}
}

func TestList_MissingPathIsSkipped(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "x.bot"), `## ---
## name: x
## ---
`)
	entries, err := List(ListOptions{Paths: []string{dir, "/nonexistent/path/12345"}})
	if err != nil {
		t.Fatalf("missing path should not error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries", len(entries))
	}
}

func TestResolveBotPath_LooseFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "feature_dev.bot")
	writeFile(t, p, `## ---
## name: feature_dev
## ---
`)
	got, err := ResolveBotPath("feature_dev", []string{dir})
	if err != nil {
		t.Fatal(err)
	}
	if got != p {
		t.Errorf("ResolveBotPath = %q, want %q", got, p)
	}
}

func TestResolveBotPath_Bundle(t *testing.T) {
	dir := t.TempDir()
	bundleDir := filepath.Join(dir, "mybot")
	writeFile(t, filepath.Join(bundleDir, "manifest.yaml"), `name: mybot
description: ""
`)
	main := filepath.Join(bundleDir, "main.bot")
	writeFile(t, main, `agent x:
  model: "test"
`)
	got, err := ResolveBotPath("mybot", []string{dir})
	if err != nil {
		t.Fatal(err)
	}
	if got != main {
		t.Errorf("ResolveBotPath = %q, want %q", got, main)
	}
}

func TestResolveBotPath_NotFound(t *testing.T) {
	dir := t.TempDir()
	_, err := ResolveBotPath("ghost", []string{dir})
	if err == nil {
		t.Fatal("expected error for missing bot")
	}
}

func TestEntry_MainFile(t *testing.T) {
	dir := t.TempDir()
	bundleDir := filepath.Join(dir, "b")
	if err := os.MkdirAll(bundleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	loose := filepath.Join(dir, "loose.bot")
	writeFile(t, loose, `## name: x ##`)

	bundleEntry := Entry{Path: bundleDir, Name: "b"}
	if got := bundleEntry.MainFile(); got != filepath.Join(bundleDir, "main.bot") {
		t.Errorf("bundle MainFile = %q", got)
	}
	looseEntry := Entry{Path: loose, Name: "x"}
	if got := looseEntry.MainFile(); got != loose {
		t.Errorf("loose MainFile = %q", got)
	}
}

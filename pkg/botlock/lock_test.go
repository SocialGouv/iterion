package botlock

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	body := `version: 1
dependencies:
  shared-planner:
    source: https://example.test/shared-planner.git
    ref: 0123456789abcdef
    path: planner
    bundle_sha256: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
`
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	lock, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := lock.Dependencies["shared-planner"].Path; got != "planner" {
		t.Fatalf("path = %q", got)
	}
}

func TestLoadRejectsUnpinnedDependency(t *testing.T) {
	dir := t.TempDir()
	body := `version: 1
dependencies:
  shared-planner:
    source: https://example.test/shared-planner.git
    ref: main
    bundle_sha256: latest
`
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("expected invalid hash to fail")
	}
}

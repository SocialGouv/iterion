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

func TestSaveRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := &Lock{Version: CurrentVersion, Dependencies: map[string]Dependency{
		"shared-planner": {
			Source: "https://example.test/shared.git", Ref: "deadbeef", Path: "bots/shared-planner",
			BundleSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
	}}
	if err := Save(dir, want); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Dependencies["shared-planner"] != want.Dependencies["shared-planner"] {
		t.Fatalf("dependency = %#v", got.Dependencies["shared-planner"])
	}
}

func TestSaveDoesNotReplaceLockWithInvalidData(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	if err := os.WriteFile(path, []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Save(dir, &Lock{Version: CurrentVersion, Dependencies: map[string]Dependency{
		"shared-planner": {Source: "x", Ref: "y", BundleSHA256: "invalid"},
	}}); err == nil {
		t.Fatal("expected invalid lock to fail")
	}
	body, err := os.ReadFile(path)
	if err != nil || string(body) != "original\n" {
		t.Fatalf("lock changed: %q err=%v", body, err)
	}
}

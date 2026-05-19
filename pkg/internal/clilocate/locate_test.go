package clilocate

import (
	"os"
	"path/filepath"
	goruntime "runtime"
	"testing"
)

// makeExecutable creates an executable file in dir with the given name.
// Returns the absolute path.
func makeExecutable(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write executable: %v", err)
	}
	return p
}

func TestLocate_ExplicitPath_Found(t *testing.T) {
	dir := t.TempDir()
	bin := makeExecutable(t, dir, "fake-cli")

	got, ok := Locate(bin, Spec{Name: "ignored"})
	if !ok || got != bin {
		t.Fatalf("explicit path: got (%q, %v), want (%q, true)", got, ok, bin)
	}
}

func TestLocate_ExplicitPath_Missing(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "not-there")

	// Even though Fallbacks contains a real file, explicit-miss must
	// return false — the caller asked for a specific path.
	real := makeExecutable(t, dir, "real")
	got, ok := Locate(missing, Spec{Name: "real", Fallbacks: []string{real}})
	if ok || got != "" {
		t.Fatalf("explicit miss should not consult fallbacks; got (%q, %v)", got, ok)
	}
}

func TestLocate_ExplicitPath_Directory(t *testing.T) {
	dir := t.TempDir()
	// Passing a directory as explicit should miss — fileExists checks !IsDir.
	got, ok := Locate(dir, Spec{Name: "ignored"})
	if ok || got != "" {
		t.Fatalf("directory as explicit should miss; got (%q, %v)", got, ok)
	}
}

func TestLocate_PathLookup(t *testing.T) {
	dir := t.TempDir()
	bin := makeExecutable(t, dir, "uniq-test-bin")

	// Prepend dir to PATH and look up the bare name.
	oldPath := os.Getenv("PATH")
	t.Cleanup(func() { os.Setenv("PATH", oldPath) })
	os.Setenv("PATH", dir+string(os.PathListSeparator)+oldPath)

	got, ok := Locate("", Spec{Name: "uniq-test-bin"})
	if !ok || got != bin {
		t.Fatalf("PATH lookup: got (%q, %v), want (%q, true)", got, ok, bin)
	}
}

func TestLocate_FallbackUsedWhenPathMisses(t *testing.T) {
	dir := t.TempDir()
	bin := makeExecutable(t, dir, "fallback-bin")

	// Empty PATH so exec.LookPath misses.
	oldPath := os.Getenv("PATH")
	t.Cleanup(func() { os.Setenv("PATH", oldPath) })
	os.Setenv("PATH", "")

	got, ok := Locate("", Spec{
		Name:      "should-not-exist-anywhere-xyz",
		Fallbacks: []string{bin},
	})
	if !ok || got != bin {
		t.Fatalf("fallback resolution: got (%q, %v), want (%q, true)", got, ok, bin)
	}
}

func TestLocate_FallbackSkipsNonExecutable(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("permission bits are not enforced on windows")
	}
	dir := t.TempDir()
	nonExec := filepath.Join(dir, "non-exec")
	if err := os.WriteFile(nonExec, []byte("text"), 0o644); err != nil {
		t.Fatalf("write non-exec: %v", err)
	}
	exec := makeExecutable(t, dir, "real-bin")

	oldPath := os.Getenv("PATH")
	t.Cleanup(func() { os.Setenv("PATH", oldPath) })
	os.Setenv("PATH", "")

	got, ok := Locate("", Spec{
		Name:      "missing-name",
		Fallbacks: []string{nonExec, exec},
	})
	if !ok || got != exec {
		t.Fatalf("expected fallback to skip non-exec; got (%q, %v), want (%q, true)", got, ok, exec)
	}
}

func TestLocate_AllMiss(t *testing.T) {
	oldPath := os.Getenv("PATH")
	t.Cleanup(func() { os.Setenv("PATH", oldPath) })
	os.Setenv("PATH", "")

	got, ok := Locate("", Spec{
		Name:      "definitely-does-not-exist-abcxyz",
		Fallbacks: []string{"/definitely/not/here"},
	})
	if ok || got != "" {
		t.Fatalf("expected miss, got (%q, %v)", got, ok)
	}
}

func TestCommonBinaryCandidates(t *testing.T) {
	got := CommonBinaryCandidates("foo")
	if len(got) == 0 {
		t.Fatal("expected non-empty candidate list")
	}
	// All entries must contain the binary name as the last component.
	for _, p := range got {
		if filepath.Base(p) != "foo" {
			t.Errorf("candidate %q does not end in /foo", p)
		}
	}
}

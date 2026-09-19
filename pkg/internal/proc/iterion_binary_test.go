package proc

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLocateIterionBinary_AbsolutisesARelativeITERIONBIN pins the one
// absolute form every consumer of the resolver relies on: a relative
// ITERION_BIN is stat'd against the engine's cwd, but the path travels to
// subprocesses running with a DIFFERENT cwd (a tool node's workDir, an
// MCP server, a bind-mount source) where a relative path resolves
// elsewhere or nowhere — silently. A shim symlinked to it would dangle
// with the staging reporting success.
//
// Mutation seen red: drop LocateIterionBinary's filepath.Abs tail — the
// resolver returns the bare relative name.
func TestLocateIterionBinary_AbsolutisesARelativeITERIONBIN(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "iterion")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	t.Setenv("ITERION_BIN", "iterion") // relative, exists, executable

	got := LocateIterionBinary()
	if !filepath.IsAbs(got) {
		t.Fatalf("LocateIterionBinary() = %q, want an absolute path", got)
	}
	if got != bin {
		t.Fatalf("LocateIterionBinary() = %q, want %q", got, bin)
	}
}

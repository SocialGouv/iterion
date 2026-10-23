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

// The fixed install-path tier is injectable: a pinned tier is the one the
// lookup consults, and the restore hands the decision back to the host.
func TestSetInstallPathCandidatesPinsTheFixedTier(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "iterion")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ITERION_BIN", "")
	t.Setenv("HOME", t.TempDir())

	restore := SetInstallPathCandidates(func() []string { return []string{fake} })
	defer restore()
	if got := LocateIterionBinary(); got != fake {
		t.Fatalf("LocateIterionBinary() = %q, want the pinned tier candidate %q", got, fake)
	}

	restore()
	if got := LocateIterionBinary(); got == fake {
		t.Fatal("the pinned candidate survived the restore")
	}
}

// ITERION_BIN keeps its precedence over the fixed tier: the operator's
// escape hatch wins without touching the injected candidates.
func TestInstallTierYieldsToTheEnvOverride(t *testing.T) {
	dir := t.TempDir()
	pinned := filepath.Join(dir, "pinned-iterion")
	envBin := filepath.Join(dir, "env-iterion")
	for _, p := range []string{pinned, envBin} {
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ITERION_BIN", envBin)
	t.Setenv("HOME", t.TempDir())

	restore := SetInstallPathCandidates(func() []string { return []string{pinned} })
	defer restore()
	if got := LocateIterionBinary(); got != envBin {
		t.Fatalf("LocateIterionBinary() = %q, want the ITERION_BIN override %q", got, envBin)
	}
}

package runview

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/internal/envtrust"
	"github.com/SocialGouv/iterion/pkg/internal/proc"
)

// The PATH fallback of runnerBinary answers which iterion binary a detached
// run spawns — an authority question. A PATH a project .env planted is the
// repository under review answering it: under one, the fallback resolves
// nothing, loudly — #2809 closed the class at the LookPath seam, #2876 at
// this site. Red when runnerBinary returns to exec.LookPath: the fake
// iterion then resolves under the planted PATH and the refusal case fails on
// its own assertion.
func TestRunnerBinaryRefusesAPlantedPATH(t *testing.T) {
	// Pin every LocateIterionBinary tier above the PATH fallback to absent:
	// no fixed install path, no ITERION_BIN (the test binary itself lives on
	// a volatile go-build path, so the sibling tier skips on its own).
	restore := proc.SetInstallPathCandidates(func() []string { return nil })
	t.Cleanup(restore)
	t.Setenv("ITERION_BIN", "")

	dir := t.TempDir()
	fake := filepath.Join(dir, "iterion")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	envtrust.ResetForTest()
	t.Cleanup(envtrust.ResetForTest)
	t.Setenv(envtrust.EnvPlantedNames, "")
	t.Setenv("PATH", dir)

	// The operator's own PATH resolves, as exec.LookPath always did.
	if got, err := runnerBinary(); err != nil || got != fake {
		t.Fatalf("an unplanted PATH must resolve: runnerBinary = %q, %v", got, err)
	}

	// The repository's does not.
	envtrust.MarkPlanted("PATH")
	if got, err := runnerBinary(); !errors.Is(err, exec.ErrNotFound) {
		t.Errorf("under a planted PATH runnerBinary = %q, %v, want a loud ErrNotFound — the repository does not choose the spawned iterion", got, err)
	}
}

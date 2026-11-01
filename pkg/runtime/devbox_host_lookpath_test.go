package runtime

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/internal/envtrust"
)

// `devbox install` runs the resolved binary against the run's workspace —
// which devbox that is, is an authority question, and a PATH a project .env
// planted must not answer it (#2809 closed the seam, #2876 this site). Red
// when hostDevboxLookPath returns to exec.LookPath: the fake devbox then
// resolves under the planted PATH and the refusal case fails on its own
// assertion.
func TestHostDevboxLookPathRefusesAPlantedPATH(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "devbox")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	envtrust.ResetForTest()
	t.Cleanup(envtrust.ResetForTest)
	t.Setenv(envtrust.EnvPlantedNames, "")
	t.Setenv("PATH", dir)

	// The operator's own PATH resolves, as exec.LookPath always did.
	if got, err := hostDevboxLookPath(); err != nil || got != fake {
		t.Fatalf("an unplanted PATH must resolve: hostDevboxLookPath = %q, %v", got, err)
	}

	// The repository's does not.
	envtrust.MarkPlanted("PATH")
	if got, err := hostDevboxLookPath(); !errors.Is(err, exec.ErrNotFound) {
		t.Errorf("under a planted PATH hostDevboxLookPath = %q, %v, want a loud ErrNotFound — the repository does not choose the provisioning binary", got, err)
	}
}

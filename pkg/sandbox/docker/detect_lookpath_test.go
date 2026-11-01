package docker

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/internal/envtrust"
)

// Which container CLI the sandbox driver spawns is an authority question:
// the launcher answers it host-side, with the most privileged spawn iterion
// makes. A PATH a project .env planted (applyDotEnv fills only names that
// were UNSET) is the repository under review answering it — #2809 closed the
// class at the LookPath seam, #2876 at this site. Red when Detect returns to
// exec.LookPath: the fake runtime then resolves under the planted PATH and
// the refusal case fails on its own assertion.
func TestDetectResolvesNoRuntimeUnderAPlantedPATH(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "docker")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	envtrust.ResetForTest()
	t.Cleanup(envtrust.ResetForTest)
	t.Setenv(envtrust.EnvPlantedNames, "")
	t.Setenv("PATH", dir)

	// The operator's own PATH resolves, as exec.LookPath always did.
	if rt, err := Detect(); err != nil || rt != RuntimeDocker {
		t.Fatalf("an unplanted PATH must resolve: Detect = %q, %v", rt, err)
	}

	// The repository's does not.
	envtrust.MarkPlanted("PATH")
	if rt, err := Detect(); err == nil {
		t.Errorf("under a planted PATH Detect = %q, want a refusal — the repository does not choose the sandbox runtime", rt)
	}
}

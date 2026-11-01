package sandbox

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/internal/envtrust"
)

// HostHasDocker reports the runtime binary a driver would spawn, so it
// answers the same authority question the drivers' own Detect does: a PATH a
// project .env planted must not name it — #2809 closed the class at the
// LookPath seam, #2876 at this site. Red when HostHasDocker returns to
// exec.LookPath: the fake docker then resolves under the planted PATH and
// the refusal case fails on its own assertion.
func TestHostHasDockerResolvesNothingUnderAPlantedPATH(t *testing.T) {
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
	if got := HostHasDocker(); got != "docker" {
		t.Fatalf("an unplanted PATH must resolve: HostHasDocker = %q, want %q", got, "docker")
	}

	// The repository's does not.
	envtrust.MarkPlanted("PATH")
	if got := HostHasDocker(); got != "" {
		t.Errorf("under a planted PATH HostHasDocker = %q, want \"\" — the repository does not name the sandbox runtime", got)
	}
}

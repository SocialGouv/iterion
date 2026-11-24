package dryrun

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/internal/envtrust"
)

// The dash Bash.Check finds is handed an untrusted bot's script text for
// `sh -n` — which binary parses it is an authority question, and a PATH a
// project .env planted must not answer it: the refusal reads as "no dash",
// the text is reported unchecked (ErrNoChecker), never parsed by the
// repository's binary — #2809 closed the seam, #2876 this site. Red when
// Check returns to exec.LookPath: the fake dash then runs under the planted
// PATH and the refusal case fails on its own assertion.
func TestBashCheckRefusesAPlantedDash(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "dash"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	envtrust.ResetForTest()
	t.Cleanup(envtrust.ResetForTest)
	t.Setenv(envtrust.EnvPlantedNames, "")
	t.Setenv("PATH", dir)

	// The operator's own PATH resolves, as exec.LookPath always did.
	if err := (Bash{}).Check("sh", "echo ok"); err != nil {
		t.Fatalf("an unplanted PATH must resolve dash: Check = %v", err)
	}

	// The repository's does not: the text goes unchecked, said.
	envtrust.MarkPlanted("PATH")
	if err := (Bash{}).Check("sh", "echo ok"); !errors.Is(err, ErrNoChecker) {
		t.Errorf("under a planted PATH Check = %v, want ErrNoChecker — the repository does not choose the syntax checker", err)
	}
}

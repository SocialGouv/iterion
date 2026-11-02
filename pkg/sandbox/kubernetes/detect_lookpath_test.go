package kubernetes

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/envtrust"
)

// Which kubectl the driver shells out to is an authority question: the
// sibling pods it drives run the launcher's workloads. A PATH a project .env
// planted is the repository under review answering it — #2809 closed the
// class at the LookPath seam, #2876 at this site. Red when Detect returns to
// exec.LookPath: the fake kubectl then resolves under the planted PATH, the
// probe proceeds to the namespace check, and the refusal case fails on the
// error it gets.
func TestDetectResolvesNoKubectlUnderAPlantedPATH(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, kubeBinaryName)
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	envtrust.ResetForTest()
	t.Cleanup(envtrust.ResetForTest)
	t.Setenv(envtrust.EnvPlantedNames, "")
	t.Setenv("PATH", dir)

	// This test judges the kubectl lookup order, not the machine: on a
	// pod-shaped host (every ARC runner) the real in-cluster mount exists
	// and Detect legitimately succeeds, so the namespace seam points at an
	// absent scratch path and the probe fails where the test expects.
	nsMount := inClusterNamespacePath
	inClusterNamespacePath = filepath.Join(t.TempDir(), "namespace")
	t.Cleanup(func() { inClusterNamespacePath = nsMount })

	// The operator's own PATH resolves the binary; the probe then fails
	// one check later, on the absent in-cluster namespace mount.
	if _, _, err := Detect(); err == nil || !strings.Contains(err.Error(), "not running in a kubernetes pod") {
		t.Fatalf("an unplanted PATH must resolve kubectl: Detect err = %v, want the namespace check's", err)
	}

	// The repository's resolves nothing: the refusal is the lookup's own
	// "not found", before any namespace question is asked.
	envtrust.MarkPlanted("PATH")
	if _, _, err := Detect(); err == nil || !strings.Contains(err.Error(), "not found on PATH") {
		t.Errorf("under a planted PATH Detect err = %v, want the kubectl lookup refusal — the repository does not choose the binary", err)
	}

	// The doctor's PingContext answers the same question at its own
	// lookup, one call before it would shell the same binary out.
	if _, _, err := PingContext(context.Background()); err == nil || !strings.Contains(err.Error(), "not found on PATH") {
		t.Errorf("under a planted PATH PingContext err = %v, want the kubectl lookup refusal", err)
	}
}

package e2e

import (
	"os/exec"
	"testing"
)

func TestArgoCDLivenessProbe(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal("python3 is required for the ops probe contract")
	}
	cmd := exec.Command(python, "-B", "../charts/argocd-liveness/tests/test_probe.py")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ArgoCD liveness probe: %v\n%s", err, out)
	}
}

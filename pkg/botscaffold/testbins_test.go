package botscaffold

import (
	"os"
	"os/exec"
	"testing"
)

// requireBins resolves the binaries an exec test needs. Missing one skips
// the test on a developer host and FAILS it under CI (the `CI` variable
// every hosted runner sets): a skip there would turn the last loud signal
// that a shape's command runs into silence, and a runner that lacks a tool
// the shapes need is a runner to fix, not a case to wave through.
func requireBins(t *testing.T, bins ...string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, bin := range bins {
		p, err := exec.LookPath(bin)
		if err != nil {
			if os.Getenv("CI") != "" {
				t.Fatalf("%s is not on PATH and CI must ship it: the shapes' commands need it", bin)
			}
			t.Skipf("%s not on PATH", bin)
		}
		out[bin] = p
	}
	return out
}

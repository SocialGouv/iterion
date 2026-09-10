package e2e

import (
	"os/exec"
	"strings"
	"testing"
)

// TestCLIValidateDuplicateFanOutTarget exercises the C249 diagnostic through
// the surface an author actually meets: `iterion validate` on a .bot file.
//
// Two claims, and the second is the one that could silently break: the warning
// must REACH the CLI output, and it must not turn the workflow away. C249
// describes a shape that has always compiled, so a regression to error
// severity would refuse existing .bot files at every launch surface.
func TestCLIValidateDuplicateFanOutTarget(t *testing.T) {
	bin := iterionBinary(t)

	cmd := exec.Command(bin, "validate", "testdata/duplicate_fanout_target.bot")
	cmd.Env = cleanEnvForSubprocess()
	out, err := cmd.CombinedOutput()
	got := string(out)
	if err != nil {
		t.Fatalf("iterion validate must succeed on a warning-only workflow, got %v\n%s", err, got)
	}

	if !strings.Contains(got, "warning [C249]") {
		t.Errorf("validate output does not carry the C249 warning:\n%s", got)
	}
	// Name the router and the duplicated target: without both, an author has
	// to hunt for the edge themselves.
	for _, want := range []string{`"fan_probes"`, `"probe"`, "2 edges", "fan_out_each"} {
		if !strings.Contains(got, want) {
			t.Errorf("C249 CLI output missing %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "result: OK") {
		t.Errorf("a C249 workflow must still validate OK (warn, never refuse):\n%s", got)
	}
	if strings.Contains(got, "error [C249]") {
		t.Errorf("C249 was emitted as an error; it must stay a warning:\n%s", got)
	}
}

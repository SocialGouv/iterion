package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Every driver's Exec funnels through ExecCmd, and what its callers read is
// res.Stdout. When the drain bound expires, the command RAN — os/exec reports
// the sentinel only for a successful exit — but the copy of its output was
// abandoned, so the bytes in hand may be short of what it wrote. That cannot
// be normalised to success the way a non-zero exit is: it stays an error, and
// the error names the bound that ran and the variable that widens it, because
// no retry can find that for the operator.
//
// Mutation seen red (M15): drop the proc.DrainCutShort wrap in ExecCmd — the
// error comes back as the bare "exec: WaitDelay expired before I/O complete",
// naming neither the bound nor the knob.
func TestExecCmd_ACutDrainNamesTheBoundAndTheKnob(t *testing.T) {
	dir := t.TempDir()
	done := filepath.Join(dir, "holder-exited")
	// `trap '' PIPE`: past the bound the pipes are closed under the
	// descendant, and the default SIGPIPE would kill it before it could
	// report that it finished — which the test joins, so no shell outlives
	// the suite.
	script := fmt.Sprintf("( trap '' PIPE; sleep 1.5; printf 'the-tail\\n' 2>/dev/null; : > %q ) &\nprintf 'the-head\\n'\nexit 0", done)
	t.Cleanup(func() {
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(done); err == nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Errorf("the descendant never reported its exit — it may outlive the suite")
	})

	cmd := exec.Command("sh", "-c", script)
	cmd.WaitDelay = 150 * time.Millisecond
	res, err := ExecCmd(cmd, ExecOpts{})

	if _, serr := os.Stat(done); serr == nil {
		t.Fatalf("the descendant was gone before the bound: this run was not the held shape the witness needs")
	}
	if err == nil {
		t.Fatalf("a read cut mid-answer came back as the answer: stdout=%q", res.Stdout)
	}
	for _, want := range []string{"150ms", "ITERION_PROC_WAIT_DELAY", "kept its output open"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not name %q — an operator cannot act on it: %v", want, err)
		}
	}
	// What was copied before the cut is still handed over: the error says
	// the result is short, it does not throw it away.
	if !strings.Contains(string(res.Stdout), "the-head") {
		t.Errorf("Stdout = %q, want what the command had already written", res.Stdout)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0: the command itself succeeded", res.ExitCode)
	}
}

// The other half of the contract, unchanged by #2469: a non-zero exit is NOT
// an error here, it is res.ExitCode. Kept beside the witness above so the two
// readings of "the command ran" stay visible together.
func TestExecCmd_ANonZeroExitIsStillNotAnError(t *testing.T) {
	res, err := ExecCmd(exec.Command("sh", "-c", "printf 'out\\n'; exit 7"), ExecOpts{})
	if err != nil {
		t.Fatalf("ExecCmd: %v", err)
	}
	if res.ExitCode != 7 {
		t.Errorf("ExitCode = %d, want 7", res.ExitCode)
	}
}

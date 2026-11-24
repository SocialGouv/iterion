package delegate

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/internal/proc"
)

// fixtureTestOutput propagates a shorter testing deadline to a re-executed
// test binary so its own fixture groups can stop before this parent does.
func fixtureTestOutput(t *testing.T, args []string, configure func(*exec.Cmd)) ([]byte, error) {
	t.Helper()
	var timeout time.Duration
	if deadline, ok := t.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 1 {
			return nil, context.DeadlineExceeded
		}
		timeout = remaining / 2
	}
	args = append(append([]string(nil), args...), "-test.timeout="+timeout.String())
	return fixtureProcessOutput(t, os.Args[0], args, configure)
}

// fixtureProcessOutput owns a fixture's process group through Wait. A test
// deadline reserves its final quarter for cancellation and reporting instead
// of letting the testing package's alarm kill the parent first. With an
// explicitly unbounded test timeout, the fixture follows t.Context().
// Descendants that detach their own groups remain their launcher's concern.
func fixtureProcessOutput(t *testing.T, name string, args []string, configure func(*exec.Cmd)) ([]byte, error) {
	t.Helper()
	ctx := t.Context()
	var cancel context.CancelFunc
	if deadline, ok := t.Deadline(); ok {
		ctx, cancel = context.WithDeadline(ctx, time.Now().Add(time.Until(deadline)*3/4))
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...) // #nosec G204 — test-owned executable and arguments
	proc.TerminateGroupOnCancel(cmd)
	proc.SetWaitDelay(cmd, time.Second)
	configure(cmd)
	out, err := cmd.CombinedOutput()
	// Teardown is immediate: never retain a process-group id until a later
	// cleanup, after the kernel could have recycled it.
	_, cleanupErr := proc.KillProcessGroup(cmd)
	return out, errors.Join(err, cleanupErr)
}

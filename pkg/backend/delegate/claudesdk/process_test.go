//go:build !windows

package claudesdk

import (
	"bufio"
	"context"
	"errors"
	"os/exec"
	"runtime"
	"syscall"
	"testing"
	"time"
)

// newTestProcess builds a *cliProcess wrapping the given command, configured
// like spawnProcess does (process group leader, drained stderr) so we can
// exercise close() against arbitrary child shapes.
func newTestProcess(t *testing.T, ctx context.Context, name string, args ...string) *cliProcess {
	t.Helper()
	cmd := exec.CommandContext(ctx, name, args...)
	setProcessGroup(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("stderr pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	p := &cliProcess{
		cmd:    cmd,
		stdout: bufio.NewScanner(stdout),
		stderr: stderr,
	}
	go p.drainStderr()
	return p
}

// TestClose_ForceKillsHungSubtree reproduces the failure mode that motivated
// the fix: a Claude CLI subtree that keeps a sleep/MCP-style child alive past
// its parent's logical completion. Without process-group kill the parent's
// cmd.Wait() blocks indefinitely; close() must terminate the whole group and
// return in bounded time.
func TestClose_ForceKillsHungSubtree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix-only: test relies on /bin/sh and POSIX process groups")
	}
	t.Setenv("ITERION_CLAUDE_CODE_CLOSE_GRACE", "200ms")
	t.Setenv("ITERION_CLAUDE_CODE_CLOSE_TERM", "200ms")

	// `sh -c 'sleep 60 & wait'` — sh forks a sleep, then waits for it.
	// Closing sh's stdin doesn't reach the child sleep, so the natural
	// cmd.Wait() would block for ~60s. Killing the whole pgid resolves
	// it within close()'s budget.
	p := newTestProcess(t, context.Background(), "sh", "-c", "sleep 60 & wait")

	start := time.Now()
	err := p.close()
	elapsed := time.Since(start)

	// 200ms grace + 200ms term + small kernel slack — give a generous
	// upper bound so this isn't flaky on loaded CI runners.
	if elapsed > 3*time.Second {
		t.Fatalf("close() exceeded budget: took %v; subtree was not force-killed", elapsed)
	}
	if err != nil {
		// Signal-induced exit is expected and discarded by ignoreSignalExit.
		// Any error here would indicate the discard path missed a case.
		t.Fatalf("close() returned unexpected error after forced kill: %v", err)
	}
}

// TestClose_GracefulExitFastPath confirms we don't pay the kill ladder when
// the child cooperates: a process that exits on its own should be reaped on
// the grace timer, no signals fired.
func TestClose_GracefulExitFastPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix-only")
	}
	t.Setenv("ITERION_CLAUDE_CODE_CLOSE_GRACE", "2s")
	t.Setenv("ITERION_CLAUDE_CODE_CLOSE_TERM", "2s")

	p := newTestProcess(t, context.Background(), "sh", "-c", "exit 0")

	start := time.Now()
	err := p.close()
	elapsed := time.Since(start)

	if elapsed > 1*time.Second {
		t.Fatalf("clean exit took too long: %v — graceful path didn't fire", elapsed)
	}
	if err != nil {
		t.Fatalf("graceful close returned error: %v", err)
	}
}

// TestKillProcessGroup_IdempotentOnESRCH guards the idempotency invariant we
// rely on in the close ladder: the SIGTERM phase may race with the child
// exiting cleanly, in which case the group is already gone and we want to
// continue, not surface ESRCH as a failure.
func TestKillProcessGroup_IdempotentOnESRCH(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix-only")
	}
	cmd := exec.Command("sh", "-c", "exit 0")
	setProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait: %v", err)
	}
	// Process group is gone — Kill should swallow ESRCH.
	if err := killProcessGroup(pid, syscall.SIGTERM); err != nil {
		if !errors.Is(err, syscall.ESRCH) {
			t.Fatalf("expected nil or ESRCH, got %v", err)
		}
		t.Fatalf("ESRCH leaked through: %v", err)
	}
}

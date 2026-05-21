//go:build !windows

package claudesdk

import (
	"bufio"
	"context"
	"errors"
	"os/exec"
	"runtime"
	"sync"
	"testing"
	"time"
)

// TestStream_HonorsCtxCancel reproduces the silent-hang failure mode:
// the Claude CLI stops emitting bytes mid-stream and the per-iteration
// ctx.Done() check never gets a turn because bufio.Scanner.Scan() is
// stuck in a kernel read. Stream MUST return within a bounded window
// after ctx cancellation by force-closing the subprocess (so the pipe
// closes and Scan unblocks).
//
// Pre-fix this test would hang up to the runtime's outer timeout because
// the cat-like child reads stdin forever and never writes stdout.
func TestStream_HonorsCtxCancel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix-only: relies on cat + POSIX process groups")
	}
	t.Setenv("ITERION_CLAUDE_CODE_CLOSE_GRACE", "100ms")
	t.Setenv("ITERION_CLAUDE_CODE_CLOSE_TERM", "100ms")

	// `cat` with no input flag reads stdin, writes anything it reads to
	// stdout. We never write to its stdin, so it sits in a read forever
	// — perfect proxy for a Claude CLI that produced its prompt response
	// then stopped emitting bytes while keeping the subprocess alive.
	p := newCatProcess(t)

	sess := &Session{
		cfg:           applyOptions(nil),
		proc:          p,
		hookCallbacks: make(map[string]HookCallback),
	}
	t.Cleanup(func() { _ = sess.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var (
		mu       sync.Mutex
		gotErr   error
		finished bool
	)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for _, err := range sess.Stream(ctx) {
			if err != nil {
				mu.Lock()
				gotErr = err
				mu.Unlock()
			}
		}
		mu.Lock()
		finished = true
		mu.Unlock()
	}()

	// Give Stream a moment to enter the blocking readLine.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stream did not return within 2s of ctx cancel — readLine still blocking")
	}

	mu.Lock()
	defer mu.Unlock()
	if !finished {
		t.Fatal("Stream returned but loop did not complete")
	}
	if !errors.Is(gotErr, context.Canceled) {
		t.Fatalf("expected ctx.Canceled, got %v", gotErr)
	}
}

// newCatProcess wraps `cat` like newTestProcess does. We need a
// distinct helper because cat reads from stdin; the canonical
// newTestProcess fixture doesn't open a stdin pipe.
func newCatProcess(t *testing.T) *cliProcess {
	t.Helper()
	cmd := exec.Command("cat")
	setProcessGroup(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
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
		stdin:  stdin,
		stdout: bufio.NewScanner(stdout),
		stderr: stderr,
	}
	go p.drainStderr()
	return p
}

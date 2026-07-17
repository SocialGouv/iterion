package schedgate

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRunGuard_OK(t *testing.T) {
	res := RunGuard(context.Background(), GuardSpec{Command: "echo hello", Dir: t.TempDir()})
	if res.Kind != GuardOK {
		t.Fatalf("Kind = %v (err=%v), want GuardOK", res.Kind, res.Err)
	}
	if res.Stdout != "hello\n" {
		t.Fatalf("Stdout = %q, want %q", res.Stdout, "hello\n")
	}
	if res.Err != nil {
		t.Fatalf("Err = %v, want nil", res.Err)
	}
}

func TestRunGuard_NonZeroExit(t *testing.T) {
	res := RunGuard(context.Background(), GuardSpec{Command: "echo nope >&2; exit 7", Dir: t.TempDir()})
	if res.Kind != GuardBlocked {
		t.Fatalf("Kind = %v, want GuardBlocked", res.Kind)
	}
	if res.ExitCode != 7 {
		t.Fatalf("ExitCode = %d, want 7", res.ExitCode)
	}
	if !strings.Contains(res.StderrTail, "nope") {
		t.Fatalf("StderrTail = %q, want to contain %q", res.StderrTail, "nope")
	}
}

func TestRunGuard_Timeout(t *testing.T) {
	start := time.Now()
	res := RunGuard(context.Background(), GuardSpec{Command: "sleep 10", Dir: t.TempDir(), Timeout: 50 * time.Millisecond})
	if res.Kind != GuardError {
		t.Fatalf("Kind = %v, want GuardError", res.Kind)
	}
	if res.ExitCode != -1 {
		t.Fatalf("ExitCode = %d, want -1", res.ExitCode)
	}
	if !errors.Is(res.Err, context.DeadlineExceeded) {
		t.Fatalf("Err = %v, want DeadlineExceeded in chain", res.Err)
	}
	// WaitDelay bounds the orphan-pipe wait: well under the sleep's 10s.
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("guard took %s, want < 5s (WaitDelay bound)", elapsed)
	}
}

func TestRunGuard_DoesNotTouchParentCtx(t *testing.T) {
	parent, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res := RunGuard(parent, GuardSpec{Command: "sleep 1", Dir: t.TempDir(), Timeout: 50 * time.Millisecond})
	if res.Kind != GuardError {
		t.Fatalf("Kind = %v, want GuardError", res.Kind)
	}
	if parent.Err() != nil {
		t.Fatalf("parent ctx polluted: %v", parent.Err())
	}
}

func TestRunGuard_WorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	res := RunGuard(context.Background(), GuardSpec{Command: "pwd", Dir: dir})
	if res.Kind != GuardOK {
		t.Fatalf("Kind = %v, want GuardOK", res.Kind)
	}
	if strings.TrimSpace(res.Stdout) != dir {
		t.Fatalf("pwd = %q, want %q", strings.TrimSpace(res.Stdout), dir)
	}
}

func TestRunGuard_EmptyCommand(t *testing.T) {
	res := RunGuard(context.Background(), GuardSpec{})
	if res.Kind != GuardError {
		t.Fatalf("Kind = %v, want GuardError", res.Kind)
	}
}

func TestTruncTail(t *testing.T) {
	if got := TruncTail("short", 100); got != "short" {
		t.Fatalf("short input mutated: %q", got)
	}
	long := strings.Repeat("a", 100) + "TAIL"
	got := TruncTail(long, 10)
	if !strings.HasSuffix(got, "TAIL") {
		t.Fatalf("tail not preserved: %q", got)
	}
	if !strings.Contains(got, "[truncated 94 bytes]") {
		t.Fatalf("marker missing or wrong: %q", got)
	}
}

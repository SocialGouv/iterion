package hooks

import (
	"context"
	"errors"
	"github.com/SocialGouv/iterion/pkg/claw/internal/testutil"
	"runtime"
	"testing"
	"time"
)

func TestRunPreToolUseCancelPropagatesToShellHook(t *testing.T) {
	testutil.IsolateShellHome(t)
	if runtime.GOOS == "windows" {
		t.Skip("relies on POSIX sleep")
	}
	runner := NewHookRunner(HookConfig{PreToolUse: []string{"sleep 30"}})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	result := runner.RunPreToolUse(ctx, "Read", `{"path":"x"}`)
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Fatalf("RunPreToolUse should have unblocked once ctx was cancelled, took %v", elapsed)
	}
	if !result.Cancelled {
		t.Fatalf("expected Cancelled=true after ctx cancellation, got %+v", result)
	}
}

func TestRunPreToolUseAlreadyCancelled(t *testing.T) {
	testutil.IsolateShellHome(t)
	runner := NewHookRunner(HookConfig{PreToolUse: []string{"echo hi"}})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result := runner.RunPreToolUse(ctx, "Read", `{"path":"x"}`)
	if !result.Cancelled {
		t.Fatalf("expected Cancelled=true when ctx is pre-cancelled, got %+v", result)
	}
}

func TestRunPostToolUseCancelPropagatesToShellHook(t *testing.T) {
	testutil.IsolateShellHome(t)
	if runtime.GOOS == "windows" {
		t.Skip("relies on POSIX sleep")
	}
	runner := NewHookRunner(HookConfig{PostToolUse: []string{"sleep 30"}})

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	result := runner.RunPostToolUse(ctx, "Read", `{}`, "output", false)
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Fatalf("RunPostToolUse should have unblocked once ctx was cancelled, took %v", elapsed)
	}
	if !result.Cancelled {
		t.Fatalf("expected Cancelled=true after ctx cancellation, got %+v", result)
	}
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("ctx error should be DeadlineExceeded, got %v", ctx.Err())
	}
}

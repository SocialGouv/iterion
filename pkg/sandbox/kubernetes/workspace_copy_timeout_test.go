package kubernetes

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// #669 part 1: the workspace-copy phase of the kubernetes driver used
// only the outer context's deadline (the run's max_duration). A stuck
// kubectl-exec tar hangs the run silently until the outer cap fires —
// hours later — with no `sandbox_started` event to warn on. Observed
// live 2026-09-03 (2h 26m spent in this phase before a runner rollout
// wiped the pod and re-delivered the message onto a stale `running`
// status). The phase must fail with a typed, visible error naming both
// the phase and the elapsed wall-clock time.

// A stall past the phase budget must surface as a typed timeout error
// naming the phase and the elapsed time.
func TestRunWithPhaseTimeout_StallSurfacesAsTypedError(t *testing.T) {
	ctx := context.Background()
	err := runWithPhaseTimeout(ctx, "workspace copy", 30*time.Millisecond, func(pctx context.Context) error {
		select {
		case <-pctx.Done():
			return pctx.Err()
		case <-time.After(2 * time.Second):
			t.Fatal("phase context did not fire the deadline")
			return nil
		}
	})
	if err == nil {
		t.Fatal("phase stall did not surface as an error — the run would block on the outer ctx (the exact bug #669 part 1 measured live)")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("phase timeout must wrap context.DeadlineExceeded (errors.Is), got %v", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "workspace copy phase timed out") {
		t.Fatalf("timeout error must name the PHASE, got %q", msg)
	}
	if !strings.Contains(msg, "deadline 30ms exceeded") {
		t.Fatalf("timeout error must name the deadline the operator set (needed to distinguish a 5m fleet default from a raised cap), got %q", msg)
	}
}

// A phase completing under budget must pass through untouched (no
// double-wrapping of a nil error, no bogus timeout report).
func TestRunWithPhaseTimeout_NominalCompletionIsPassthrough(t *testing.T) {
	ctx := context.Background()
	err := runWithPhaseTimeout(ctx, "workspace copy", time.Second, func(context.Context) error {
		return nil
	})
	if err != nil {
		t.Fatalf("nominal phase must not error, got %v", err)
	}
}

// A phase erroring for its own reason (not a timeout) keeps its native
// error shape — the operator needs to see the real cause (e.g. a git
// fixup failure), not "the phase timed out".
func TestRunWithPhaseTimeout_InnerErrorIsNotMisreportedAsTimeout(t *testing.T) {
	ctx := context.Background()
	sentinel := errors.New("tar: broken pipe")
	err := runWithPhaseTimeout(ctx, "workspace copy", time.Second, func(context.Context) error {
		return sentinel
	})
	if err == nil {
		t.Fatal("inner error was swallowed")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("inner tar-broken-pipe misreported as a phase timeout: %v — the ops would chase the wrong bug", err)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("inner error identity lost; got %v", err)
	}
}

// An OUTER context cancellation (run cancel, pod SIGTERM) must keep
// its own shape — the phase-timeout wrapper is only for the phase's
// OWN deadline, otherwise a cooperative stop would look like an
// unbounded stall in every log parser.
func TestRunWithPhaseTimeout_OuterCancelIsNotMisreportedAsPhaseTimeout(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	// Cancel BEFORE fn runs so phaseCtx.Err() will read Canceled, not
	// DeadlineExceeded, when we check it.
	cancel()
	err := runWithPhaseTimeout(parent, "workspace copy", time.Hour, func(pctx context.Context) error {
		return pctx.Err()
	})
	if err == nil {
		t.Fatal("cancelled outer ctx must surface")
	}
	if strings.Contains(err.Error(), "phase timed out") {
		t.Fatalf("outer cancel misreported as phase timeout: %v (would flood the ops channel on every pod SIGTERM)", err)
	}
}

// The env override is honoured (operator can raise the cap for a slow
// cluster or a huge workspace without editing the binary).
func TestResolveWorkspaceCopyTimeout_HonoursEnv(t *testing.T) {
	t.Setenv(workspaceCopyTimeoutEnv, "12m")
	got := resolveWorkspaceCopyTimeout()
	if want := 12 * time.Minute; got != want {
		t.Fatalf("resolveWorkspaceCopyTimeout = %s, want %s (operator override lost)", got, want)
	}
}

// Garbage in env falls back to the default rather than disabling the
// bound — the exact "fail closed" iterion convention.
func TestResolveWorkspaceCopyTimeout_GarbageFallsBackToDefault(t *testing.T) {
	t.Setenv(workspaceCopyTimeoutEnv, "not-a-duration")
	if got := resolveWorkspaceCopyTimeout(); got != DefaultWorkspaceCopyTimeout {
		t.Fatalf("garbage override = %s, want default %s (unbounded copy = the bug this fix closes)", got, DefaultWorkspaceCopyTimeout)
	}
	t.Setenv(workspaceCopyTimeoutEnv, "0")
	if got := resolveWorkspaceCopyTimeout(); got != DefaultWorkspaceCopyTimeout {
		t.Fatalf("zero override = %s, want default %s", got, DefaultWorkspaceCopyTimeout)
	}
}

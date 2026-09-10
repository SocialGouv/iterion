package kubernetes

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// #669 part 1: the workspace-copy phase of the kubernetes driver used
// only the outer context's deadline (the run's max_duration). A stuck
// kubectl-exec tar hangs the run silently until the outer cap fires —
// hours later — with no `sandbox_started` event to warn on. Observed
// live 2026-09-03 (2h 26m spent in this phase before a runner rollout
// wiped the pod and re-delivered the message onto a stale `running`
// status). The phase must fail with a typed, visible error naming both
// the phase and the elapsed wall-clock time.
//
// The bound's own contract is owned by pkg/sandbox (phase_test.go); what
// follows drives THIS driver's phases through it.

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

// Garbage in ITERION_SANDBOX_WORKSPACE_COPY_TIMEOUT must be VISIBLE:
// silently returning the default lets the operator believe the override
// took ("5" reads as five nanoseconds to Go, five minutes to a human).
// Same convention as ITERION_BUDGET_EXIT_GRACE: one stderr line per
// process, naming the value and the default.
func TestResolveWorkspaceCopyTimeout_GarbageEnvIsWarnedOnce(t *testing.T) {
	stderr, restore := captureStderr(t)
	defer restore()

	// Reset the sync.Once so the warn fires within THIS test.
	workspaceCopyTimeoutWarnOnce = sync.Once{}

	t.Setenv(workspaceCopyTimeoutEnv, "5")
	_ = resolveWorkspaceCopyTimeout()
	got := stderr()
	if !strings.Contains(got, workspaceCopyTimeoutEnv) {
		t.Fatalf("stderr = %q, want it to name %s (garbage was silently swallowed before)", got, workspaceCopyTimeoutEnv)
	}
	if !strings.Contains(got, `"5"`) || !strings.Contains(got, DefaultWorkspaceCopyTimeout.String()) {
		t.Fatalf("stderr = %q, want it to echo the operator's value and the default that replaced it", got)
	}

	// Second call in the same process must NOT re-warn (sync.Once).
	_ = resolveWorkspaceCopyTimeout()
	if got := stderr(); got != "" {
		t.Fatalf("second call re-warned: %q — expected sync.Once suppression", got)
	}
}

// The same shape driven through the REAL populateWorkspace: a kubectl
// shim on PATH that never reads its stdin and never exits is the in-pod
// tar side of the copy pipe, wedged. The phase must strike its own
// deadline (the LOCAL kubectl is killed through exec.CommandContext —
// the enforcement the doc comment promises), and the error must carry
// the sentinel, the deadline and the real cause.
func TestRunWithPhaseTimeout_RealWorkspaceCopyStallIsClassified(t *testing.T) {
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skip("tar not on PATH")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not on PATH")
	}
	shimDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(shimDir, kubeBinaryName), []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("payload\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &Run{driver: &Driver{logger: iterlog.Nop()}, podName: "sandbox-x", namespace: "ns"}

	start := time.Now()
	err := sandbox.RunWithPhaseTimeout(context.Background(), iterlog.Nop(), "workspace copy", workspaceCopyTimeoutEnv, 300*time.Millisecond, func(ctx context.Context) error {
		return r.populateWorkspace(ctx, src, "/workspace")
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("a wedged in-pod tar returned no error — the copy would block until the run's max_duration")
	}
	if elapsed > 10*time.Second {
		t.Fatalf("the phase returned after %s — the deadline did not kill the stalled kubectl", elapsed)
	}
	if !errors.Is(err, sandbox.ErrPhaseTimeout) {
		t.Fatalf("real stall does not carry sandbox.ErrPhaseTimeout: %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("real stall does not carry context.DeadlineExceeded: %v", err)
	}
	if !strings.Contains(err.Error(), "in-pod tar extract") {
		t.Fatalf("real stall lost populateWorkspace's own cause: %v", err)
	}
}

// #719: the post_create snippet is the setup phase right after the copy
// and the git fixup, and it ran on the bare context. A hung snippet (a
// package install waiting on a dead mirror, a command that reads stdin)
// parks the run in setup with no typed failure and no redelivery — the
// pod holds the run lease forever, which is the #669 incident class the
// copy bound already closed. Driven through the REAL runPostCreate with
// a kubectl shim that never exits.
func TestRunPostCreate_StallIsBoundedAndTyped(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not on PATH")
	}
	shimDir := t.TempDir()
	shim := filepath.Join(shimDir, kubeBinaryName)
	if err := os.WriteFile(shim, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(sandbox.PostCreateTimeoutEnv, "300ms")

	r := &Run{
		driver:    &Driver{logger: iterlog.Nop(), kubectl: shim},
		podName:   "sandbox-x",
		namespace: "ns",
		prepared:  &Prepared{},
	}
	done := make(chan error, 1)
	go func() { done <- r.runPostCreate(context.Background(), "apt-get install -y the-world") }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a wedged post_create returned no error")
		}
		if !errors.Is(err, sandbox.ErrPhaseTimeout) {
			t.Fatalf("post_create stall does not carry sandbox.ErrPhaseTimeout: %v — the setup classifier cannot park it resumable and the runner cannot nak it", err)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("post_create stall does not carry context.DeadlineExceeded: %v", err)
		}
		if !strings.Contains(err.Error(), "post_create phase timed out") {
			t.Fatalf("timeout error must name the phase, got %q", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runPostCreate never returned — the snippet runs on the bare ctx, so the run sits in sandbox setup until the outer max_duration fires (#669's shape, one phase later)")
	}
}

// The post_create bound reads the SHARED phase knob and the copy knob
// must not move with it: a package install legitimately outlasts a
// workspace copy, and an operator raising one must not have to raise the
// other.
func TestResolvePostCreateTimeout_DoesNotMoveTheCopyBound(t *testing.T) {
	t.Setenv(sandbox.PostCreateTimeoutEnv, "45m")
	if got, want := sandbox.ResolvePostCreateTimeout(), 45*time.Minute; got != want {
		t.Fatalf("ResolvePostCreateTimeout() = %s, want %s (operator override lost)", got, want)
	}
	if got := resolveWorkspaceCopyTimeout(); got != DefaultWorkspaceCopyTimeout {
		t.Fatalf("the post_create override moved the copy bound to %s — the two phases have separate budgets", got)
	}
}

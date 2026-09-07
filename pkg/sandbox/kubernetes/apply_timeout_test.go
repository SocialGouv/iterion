package kubernetes

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
)

// #823 part 2: every per-run resource the driver creates — the credential
// Secret, the CA Secret, the sandbox pod, the NetworkPolicy, and the
// mid-run secret refresh — goes through `kubectl apply` on the BARE run
// context. A wedged apiserver (a control-plane rollout, a throttled
// webhook, a TCP connect that never completes) therefore hangs sandbox
// creation BEFORE the first bounded phase is reached: no
// `sandbox_started`, no typed failure, no redelivery. kubectlCmdContext's
// WaitDelay only bounds orphaned pipes after a cancellation that never
// comes.
//
// Driven through the REAL applyManifest with a kubectl shim that never
// exits — the wedged-apiserver shape as the driver sees it.
func TestApplyManifest_StallIsBoundedAndTyped(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not on PATH")
	}
	shimDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(shimDir, kubeBinaryName), []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(applyTimeoutEnv, "300ms")

	done := make(chan error, 1)
	go func() {
		done <- applyManifest(context.Background(), iterlog.Nop(), "ns", "apply pod", []byte("kind: Pod\n"))
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a wedged apply returned no error")
		}
		if !errors.Is(err, sandbox.ErrPhaseTimeout) {
			t.Fatalf("apply stall does not carry sandbox.ErrPhaseTimeout: %v — the setup classifier cannot park it resumable and the runner cannot nak it", err)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("apply stall does not carry context.DeadlineExceeded: %v", err)
		}
		if !strings.Contains(err.Error(), "apply pod phase timed out") {
			t.Fatalf("timeout error must name WHICH apply stalled, got %q", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("applyManifest never returned — sandbox creation hangs before the first bounded phase, holding the run's lease until max_duration fires")
	}
}

// The bound must also reach kubectl itself: without --request-timeout the
// client waits on the apiserver indefinitely, so a phase deadline that
// fires can only kill the process, never let it report why. The two
// together mean a timeout is a timeout at both ends of the pipe.
func TestApplyManifest_PassesRequestTimeoutToKubectl(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not on PATH")
	}
	shimDir := t.TempDir()
	argvFile := filepath.Join(shimDir, "argv")
	shim := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argvFile + "\nexit 0\n"
	if err := os.WriteFile(filepath.Join(shimDir, kubeBinaryName), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(applyTimeoutEnv, "90s")

	if err := applyManifest(context.Background(), iterlog.Nop(), "ns", "apply pod", []byte("kind: Pod\n")); err != nil {
		t.Fatalf("apply against a healthy shim failed: %v", err)
	}
	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(argv), "--request-timeout=1m30s") {
		t.Fatalf("kubectl argv = %q, want --request-timeout carrying the phase budget", string(argv))
	}
}

// The same class, one helper further: `kubectl delete` is one apiserver
// call too, it runs in the middle of the setup sequence (the stale-pod
// eviction before the pod apply) and on every rollback path, and most
// callers DISCARD its result — so an unbounded one hangs the run with
// nothing to show for it. Bounded by the same budget, but on a plain
// deadline: a cleanup is not a setup phase.
func TestDeleteResource_StallIsBounded(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not on PATH")
	}
	shimDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(shimDir, kubeBinaryName), []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(applyTimeoutEnv, "300ms")

	done := make(chan error, 1)
	go func() { done <- deleteResource(context.Background(), "ns", "pod", "sandbox-x") }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a wedged delete returned no error")
		}
		if errors.Is(err, sandbox.ErrPhaseTimeout) {
			t.Fatalf("a cleanup delete carries the SETUP-phase sentinel: %v — the classifier would park a cleanup stall as a setup timeout", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("deleteResource never returned — a rollback or the stale-pod eviction hangs the run on a wedged apiserver")
	}
}

// The apply family reads its own knob, fail-closed like its siblings: a
// garbage value falls back to the default rather than disabling the bound.
func TestResolveApplyTimeout_HonoursItsOwnEnvFailClosed(t *testing.T) {
	if got := resolveApplyTimeout(); got != DefaultApplyTimeout {
		t.Fatalf("resolveApplyTimeout() = %s with no env, want the default %s", got, DefaultApplyTimeout)
	}
	t.Setenv(applyTimeoutEnv, "5m")
	if got, want := resolveApplyTimeout(), 5*time.Minute; got != want {
		t.Fatalf("resolveApplyTimeout() = %s, want %s (operator override lost)", got, want)
	}
	t.Setenv(applyTimeoutEnv, "5 minutes")
	if got := resolveApplyTimeout(); got != DefaultApplyTimeout {
		t.Fatalf("garbage override = %s, want the default %s (an unbounded apply is the bug this closes)", got, DefaultApplyTimeout)
	}
	// The post_create knob must not move with it.
	if got := sandbox.ResolvePostCreateTimeout(); got != sandbox.DefaultPostCreateTimeout {
		t.Fatalf("the apply override moved the post_create bound to %s — the phases have separate budgets", got)
	}
}

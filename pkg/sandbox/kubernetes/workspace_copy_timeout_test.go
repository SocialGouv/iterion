package kubernetes

import (
	"bytes"
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

// lockedBuffer is a goroutine-safe io.Writer for capturing the driver
// logger: the halfway warning is written from a side goroutine.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func captureLogger() (*iterlog.Logger, *lockedBuffer) {
	buf := &lockedBuffer{}
	return iterlog.New(iterlog.LevelWarn, buf), buf
}

// A stall past the phase budget must surface as a typed timeout error
// naming the phase and the elapsed time.
func TestRunWithPhaseTimeout_StallSurfacesAsTypedError(t *testing.T) {
	ctx := context.Background()
	err := runWithPhaseTimeout(ctx, iterlog.Nop(), "workspace copy", workspaceCopyTimeoutEnv, 30*time.Millisecond, func(pctx context.Context) error {
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
		t.Fatalf("timeout error must name the deadline the operator set (needed to distinguish the fleet default from a raised cap), got %q", msg)
	}
}

// A phase completing under budget must pass through untouched (no
// double-wrapping of a nil error, no bogus timeout report).
func TestRunWithPhaseTimeout_NominalCompletionIsPassthrough(t *testing.T) {
	ctx := context.Background()
	err := runWithPhaseTimeout(ctx, iterlog.Nop(), "workspace copy", workspaceCopyTimeoutEnv, time.Second, func(context.Context) error {
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
	err := runWithPhaseTimeout(ctx, iterlog.Nop(), "workspace copy", workspaceCopyTimeoutEnv, time.Second, func(context.Context) error {
		return sentinel
	})
	if err == nil {
		t.Fatal("inner error was swallowed")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("inner tar-broken-pipe misreported as a phase timeout: %v — the ops would chase the wrong bug", err)
	}
	if errors.Is(err, sandbox.ErrPhaseTimeout) {
		t.Fatalf("inner error carries the phase-timeout sentinel: %v — the setup classifier would park a hard failure as resumable", err)
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
	err := runWithPhaseTimeout(parent, iterlog.Nop(), "workspace copy", workspaceCopyTimeoutEnv, time.Hour, func(pctx context.Context) error {
		return pctx.Err()
	})
	if err == nil {
		t.Fatal("cancelled outer ctx must surface")
	}
	if strings.Contains(err.Error(), "phase timed out") || errors.Is(err, sandbox.ErrPhaseTimeout) {
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

// The wrapped error must expose BOTH sandbox.ErrPhaseTimeout AND
// context.DeadlineExceeded via errors.Is even when the callee's own
// error is neither — a `%w` on the inner cause alone hides the deadline
// shape from the setup classifier. Stub callee: the shape, in isolation.
func TestRunWithPhaseTimeout_WrapsPhaseTimeoutSentinelViaErrorsIs(t *testing.T) {
	ctx := context.Background()
	sentinel := errors.New("kubectl-exec pipe stalled")
	err := runWithPhaseTimeout(ctx, iterlog.Nop(), "workspace copy", workspaceCopyTimeoutEnv, 30*time.Millisecond, func(pctx context.Context) error {
		// Ignores pctx: a callee whose helpers do not respect ctx.
		time.Sleep(80 * time.Millisecond)
		return sentinel
	})
	if err == nil {
		t.Fatal("stall returned no error")
	}
	if !errors.Is(err, sandbox.ErrPhaseTimeout) {
		t.Fatalf("errors.Is(err, sandbox.ErrPhaseTimeout) = false — the setup classifier cannot route this to failed_resumable")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("errors.Is(err, context.DeadlineExceeded) = false — the deadline shape is invisible to every consumer")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("errors.Is(err, inner) = false — the operator loses the actual cause; got %v", err)
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
	err := runWithPhaseTimeout(context.Background(), iterlog.Nop(), "workspace copy", workspaceCopyTimeoutEnv, 300*time.Millisecond, func(ctx context.Context) error {
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

// A callee that ignores its ctx and returns nil past the deadline is
// warned about: a phase that burned its whole budget and won by a hair
// must be visible before the next occurrence trips the bound.
func TestRunWithPhaseTimeout_OverrunButNilCalleeWarns(t *testing.T) {
	logger, out := captureLogger()
	err := runWithPhaseTimeout(context.Background(), logger, "workspace copy", workspaceCopyTimeoutEnv, 10*time.Millisecond, func(pctx context.Context) error {
		time.Sleep(30 * time.Millisecond) // past the deadline, ignoring pctx
		return nil
	})
	if err != nil {
		t.Fatalf("nil-return-past-deadline unexpectedly errored: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "workspace copy phase completed at or past its") {
		t.Fatalf("overrun warning missing: %q (silent success past budget hides the next occurrence's failure)", got)
	}
}

// The halfway-mark warning fires while the phase is still running, so a
// slow-but-healthy copy is visible BEFORE the bound strikes.
func TestRunWithPhaseTimeout_HalfwayMarkWarns(t *testing.T) {
	prev := phaseTimeoutWarnRatio
	phaseTimeoutWarnRatio = 0.25
	defer func() { phaseTimeoutWarnRatio = prev }()

	logger, out := captureLogger()
	err := runWithPhaseTimeout(context.Background(), logger, "workspace copy", workspaceCopyTimeoutEnv, 400*time.Millisecond, func(pctx context.Context) error {
		time.Sleep(200 * time.Millisecond) // past the 25% mark, under the deadline
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "workspace copy phase still running") {
		t.Fatalf("halfway warning did not fire: %q (a slow copy needs a signal BEFORE the deadline)", got)
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
	t.Setenv(postCreateTimeoutEnv, "300ms")

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

// The post_create bound has its OWN env override: a package install
// legitimately outlasts a workspace copy, and an operator raising one
// must not have to raise the other.
func TestResolvePostCreateTimeout_HonoursItsOwnEnv(t *testing.T) {
	if got := resolvePostCreateTimeout(); got != DefaultPostCreateTimeout {
		t.Fatalf("resolvePostCreateTimeout() = %s with no env, want the default %s", got, DefaultPostCreateTimeout)
	}
	t.Setenv(postCreateTimeoutEnv, "45m")
	if got, want := resolvePostCreateTimeout(), 45*time.Minute; got != want {
		t.Fatalf("resolvePostCreateTimeout() = %s, want %s (operator override lost)", got, want)
	}
	// The copy knob must not move with it.
	if got := resolveWorkspaceCopyTimeout(); got != DefaultWorkspaceCopyTimeout {
		t.Fatalf("the post_create override moved the copy bound to %s — the two phases have separate budgets", got)
	}
	// Garbage falls back to the default, fail-closed like the copy's.
	t.Setenv(postCreateTimeoutEnv, "45 minutes")
	if got := resolvePostCreateTimeout(); got != DefaultPostCreateTimeout {
		t.Fatalf("garbage override = %s, want the default %s (an unbounded post_create is the bug this closes)", got, DefaultPostCreateTimeout)
	}
}

// A garbage post_create override must be VISIBLE on stderr, naming its
// OWN key — the copy's convention, and the reason the phase carries the
// key rather than the helper hardcoding one.
func TestResolvePostCreateTimeout_GarbageEnvIsWarnedOnce(t *testing.T) {
	stderr, restore := captureStderr(t)
	defer restore()
	postCreateTimeoutWarnOnce = sync.Once{}

	t.Setenv(postCreateTimeoutEnv, "5")
	_ = resolvePostCreateTimeout()
	got := stderr()
	if !strings.Contains(got, postCreateTimeoutEnv) {
		t.Fatalf("stderr = %q, want it to name %s", got, postCreateTimeoutEnv)
	}
	if !strings.Contains(got, `"5"`) || !strings.Contains(got, DefaultPostCreateTimeout.String()) {
		t.Fatalf("stderr = %q, want the operator's value and the default that replaced it", got)
	}
	_ = resolvePostCreateTimeout()
	if got := stderr(); got != "" {
		t.Fatalf("second call re-warned: %q", got)
	}
}

// The halfway warning names the knob of the phase that is running: an
// operator told to raise the copy timeout for a slow post_create raises
// the wrong one.
func TestRunWithPhaseTimeout_WarningNamesThePhasesOwnKnob(t *testing.T) {
	prev := phaseTimeoutWarnRatio
	phaseTimeoutWarnRatio = 0.25
	defer func() { phaseTimeoutWarnRatio = prev }()

	logger, out := captureLogger()
	err := runWithPhaseTimeout(context.Background(), logger, "post_create", postCreateTimeoutEnv, 400*time.Millisecond, func(context.Context) error {
		time.Sleep(200 * time.Millisecond)
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, postCreateTimeoutEnv) {
		t.Fatalf("halfway warning = %q, want it to name %s", got, postCreateTimeoutEnv)
	}
	if strings.Contains(got, workspaceCopyTimeoutEnv) {
		t.Fatalf("halfway warning = %q, want it NOT to name the copy's knob", got)
	}
}

// A phase that finishes before the halfway mark says nothing: the
// warning is for slow copies, not a heartbeat.
func TestRunWithPhaseTimeout_FastPhaseIsSilent(t *testing.T) {
	logger, out := captureLogger()
	err := runWithPhaseTimeout(context.Background(), logger, "workspace copy", workspaceCopyTimeoutEnv, time.Second, func(context.Context) error {
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := out.String(); got != "" {
		t.Fatalf("a fast phase warned: %q", got)
	}
}

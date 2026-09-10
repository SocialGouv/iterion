package sandbox

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// The setup-phase bound is ONE implementation shared by every driver: a
// phase that only one driver bounds is a phase the fleet does not bound.
// These tests own the helper's contract; each driver keeps an integration
// test driving its own phase through it.

const testPhaseEnv = "ITERION_SANDBOX_TEST_PHASE_TIMEOUT"

// lockedBuffer is a goroutine-safe io.Writer for capturing the logger:
// the halfway warning is written from a side goroutine.
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

// captureStderr redirects os.Stderr to a pipe for the caller's scope,
// returns a `read` callback draining everything so far, and a `restore`
// undoing the redirect.
func captureStderr(t *testing.T) (read func() string, restore func()) {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	restored := false
	restore = func() {
		if restored {
			return
		}
		restored = true
		os.Stderr = orig
		_ = w.Close()
		_ = r.Close()
	}
	read = func() string {
		// Close the write side so ReadAll sees EOF, then re-open a fresh
		// pipe so the caller can drain multiple times without blocking.
		// Order matters — close BEFORE reopening.
		os.Stderr = orig
		_ = w.Close()
		buf, _ := io.ReadAll(r)
		_ = r.Close()
		nr, nw, perr := os.Pipe()
		if perr == nil {
			r, w = nr, nw
			os.Stderr = w
		}
		return string(buf)
	}
	return read, restore
}

// A stall past the phase budget must surface as a typed timeout error
// naming the phase and the elapsed time.
func TestRunWithPhaseTimeout_StallSurfacesAsTypedError(t *testing.T) {
	ctx := context.Background()
	err := RunWithPhaseTimeout(ctx, iterlog.Nop(), "workspace copy", testPhaseEnv, 30*time.Millisecond, func(pctx context.Context) error {
		select {
		case <-pctx.Done():
			return pctx.Err()
		case <-time.After(2 * time.Second):
			t.Error("phase context did not fire the deadline")
			return nil
		}
	})
	if err == nil {
		t.Fatal("phase stall did not surface as an error — the run would block on the outer ctx")
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
	err := RunWithPhaseTimeout(context.Background(), iterlog.Nop(), "workspace copy", testPhaseEnv, time.Second, func(context.Context) error {
		return nil
	})
	if err != nil {
		t.Fatalf("nominal phase must not error, got %v", err)
	}
}

// A phase erroring for its own reason (not a timeout) keeps its native
// error shape — the operator needs to see the real cause, not "the phase
// timed out".
func TestRunWithPhaseTimeout_InnerErrorIsNotMisreportedAsTimeout(t *testing.T) {
	sentinel := errors.New("tar: broken pipe")
	err := RunWithPhaseTimeout(context.Background(), iterlog.Nop(), "workspace copy", testPhaseEnv, time.Second, func(context.Context) error {
		return sentinel
	})
	if err == nil {
		t.Fatal("inner error was swallowed")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("inner tar-broken-pipe misreported as a phase timeout: %v — the ops would chase the wrong bug", err)
	}
	if errors.Is(err, ErrPhaseTimeout) {
		t.Fatalf("inner error carries the phase-timeout sentinel: %v — the setup classifier would park a hard failure as resumable", err)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("inner error identity lost; got %v", err)
	}
}

// An OUTER context cancellation (run cancel, pod SIGTERM) must keep its
// own shape — the phase-timeout wrapper is only for the phase's OWN
// deadline, otherwise a cooperative stop would look like an unbounded
// stall in every log parser.
func TestRunWithPhaseTimeout_OuterCancelIsNotMisreportedAsPhaseTimeout(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	// Cancel BEFORE fn runs so phaseCtx.Err() reads Canceled, not
	// DeadlineExceeded, when we check it.
	cancel()
	err := RunWithPhaseTimeout(parent, iterlog.Nop(), "workspace copy", testPhaseEnv, time.Hour, func(pctx context.Context) error {
		return pctx.Err()
	})
	if err == nil {
		t.Fatal("cancelled outer ctx must surface")
	}
	if strings.Contains(err.Error(), "phase timed out") || errors.Is(err, ErrPhaseTimeout) {
		t.Fatalf("outer cancel misreported as phase timeout: %v (would flood the ops channel on every pod SIGTERM)", err)
	}
}

// The wrapped error must expose BOTH ErrPhaseTimeout AND
// context.DeadlineExceeded via errors.Is even when the callee's own error
// is neither — a `%w` on the inner cause alone hides the deadline shape
// from the setup classifier.
func TestRunWithPhaseTimeout_WrapsPhaseTimeoutSentinelViaErrorsIs(t *testing.T) {
	sentinel := errors.New("kubectl-exec pipe stalled")
	err := RunWithPhaseTimeout(context.Background(), iterlog.Nop(), "workspace copy", testPhaseEnv, 30*time.Millisecond, func(context.Context) error {
		// Ignores pctx: a callee whose helpers do not respect ctx.
		time.Sleep(80 * time.Millisecond)
		return sentinel
	})
	if err == nil {
		t.Fatal("stall returned no error")
	}
	if !errors.Is(err, ErrPhaseTimeout) {
		t.Fatalf("errors.Is(err, ErrPhaseTimeout) = false — the setup classifier cannot route this to failed_resumable")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("errors.Is(err, context.DeadlineExceeded) = false — the deadline shape is invisible to every consumer")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("errors.Is(err, inner) = false — the operator loses the actual cause; got %v", err)
	}
}

// A callee that ignores its ctx and returns nil past the deadline is
// warned about: a phase that burned its whole budget and won by a hair
// must be visible before the next occurrence trips the bound.
func TestRunWithPhaseTimeout_OverrunButNilCalleeWarns(t *testing.T) {
	logger, out := captureLogger()
	err := RunWithPhaseTimeout(context.Background(), logger, "workspace copy", testPhaseEnv, 10*time.Millisecond, func(context.Context) error {
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
	err := RunWithPhaseTimeout(context.Background(), logger, "workspace copy", testPhaseEnv, 400*time.Millisecond, func(context.Context) error {
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

// The halfway warning names the knob of the phase that is running: an
// operator told to raise the copy timeout for a slow post_create raises
// the wrong one.
func TestRunWithPhaseTimeout_WarningNamesThePhasesOwnKnob(t *testing.T) {
	prev := phaseTimeoutWarnRatio
	phaseTimeoutWarnRatio = 0.25
	defer func() { phaseTimeoutWarnRatio = prev }()

	logger, out := captureLogger()
	err := RunWithPhaseTimeout(context.Background(), logger, "post_create", PostCreateTimeoutEnv, 400*time.Millisecond, func(context.Context) error {
		time.Sleep(200 * time.Millisecond)
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, PostCreateTimeoutEnv) {
		t.Fatalf("halfway warning = %q, want it to name %s", got, PostCreateTimeoutEnv)
	}
	if strings.Contains(got, testPhaseEnv) {
		t.Fatalf("halfway warning = %q, want it NOT to name another phase's knob", got)
	}
}

// A phase that finishes before the halfway mark says nothing: the warning
// is for slow phases, not a heartbeat.
func TestRunWithPhaseTimeout_FastPhaseIsSilent(t *testing.T) {
	logger, out := captureLogger()
	err := RunWithPhaseTimeout(context.Background(), logger, "workspace copy", testPhaseEnv, time.Second, func(context.Context) error {
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := out.String(); got != "" {
		t.Fatalf("a fast phase warned: %q", got)
	}
}

// The env override is honoured (an operator can raise the cap for a slow
// cluster or a huge workspace without editing the binary), and garbage
// falls back to the default rather than disabling the bound.
func TestResolvePhaseTimeout_HonoursEnvAndFailsClosed(t *testing.T) {
	var once sync.Once
	if got := ResolvePhaseTimeout(testPhaseEnv, 15*time.Minute, &once); got != 15*time.Minute {
		t.Fatalf("unset override = %s, want the default", got)
	}
	t.Setenv(testPhaseEnv, "12m")
	if got, want := ResolvePhaseTimeout(testPhaseEnv, 15*time.Minute, &once), 12*time.Minute; got != want {
		t.Fatalf("ResolvePhaseTimeout = %s, want %s (operator override lost)", got, want)
	}
	for _, bad := range []string{"not-a-duration", "0", "-5m"} {
		t.Setenv(testPhaseEnv, bad)
		if got := ResolvePhaseTimeout(testPhaseEnv, 15*time.Minute, &once); got != 15*time.Minute {
			t.Fatalf("override %q = %s, want the default (an unbounded phase is the bug this closes)", bad, got)
		}
	}
}

// Garbage in a phase knob must be VISIBLE: silently returning the default
// lets the operator believe the override took ("5" reads as five
// nanoseconds to Go, five minutes to a human). One stderr line per
// process, naming the key, the value and the default.
func TestResolvePhaseTimeout_GarbageEnvIsWarnedOnce(t *testing.T) {
	stderr, restore := captureStderr(t)
	defer restore()

	var once sync.Once
	t.Setenv(testPhaseEnv, "5")
	_ = ResolvePhaseTimeout(testPhaseEnv, 15*time.Minute, &once)
	got := stderr()
	if !strings.Contains(got, testPhaseEnv) {
		t.Fatalf("stderr = %q, want it to name %s (garbage was silently swallowed before)", got, testPhaseEnv)
	}
	if !strings.Contains(got, `"5"`) || !strings.Contains(got, (15*time.Minute).String()) {
		t.Fatalf("stderr = %q, want it to echo the operator's value and the default that replaced it", got)
	}

	// Second call in the same process must NOT re-warn (sync.Once).
	_ = ResolvePhaseTimeout(testPhaseEnv, 15*time.Minute, &once)
	if got := stderr(); got != "" {
		t.Fatalf("second call re-warned: %q — expected sync.Once suppression", got)
	}
}

// The post_create budget is a PHASE knob, not a driver knob: both drivers
// resolve it here, so an operator raising it for a slow toolchain install
// raises it wherever the phase runs.
func TestResolvePostCreateTimeout_HonoursItsOwnEnv(t *testing.T) {
	if got := ResolvePostCreateTimeout(); got != DefaultPostCreateTimeout {
		t.Fatalf("ResolvePostCreateTimeout() = %s with no env, want the default %s", got, DefaultPostCreateTimeout)
	}
	t.Setenv(PostCreateTimeoutEnv, "45m")
	if got, want := ResolvePostCreateTimeout(), 45*time.Minute; got != want {
		t.Fatalf("ResolvePostCreateTimeout() = %s, want %s (operator override lost)", got, want)
	}
	t.Setenv(PostCreateTimeoutEnv, "45 minutes")
	if got := ResolvePostCreateTimeout(); got != DefaultPostCreateTimeout {
		t.Fatalf("garbage override = %s, want the default %s (an unbounded post_create is the bug this closes)", got, DefaultPostCreateTimeout)
	}
}

// A garbage post_create override must be VISIBLE on stderr, naming its
// OWN key — the reason the phase carries the key rather than the helper
// hardcoding one.
func TestResolvePostCreateTimeout_GarbageEnvIsWarnedOnce(t *testing.T) {
	stderr, restore := captureStderr(t)
	defer restore()
	postCreateTimeoutWarnOnce = sync.Once{}

	t.Setenv(PostCreateTimeoutEnv, "5")
	_ = ResolvePostCreateTimeout()
	got := stderr()
	if !strings.Contains(got, PostCreateTimeoutEnv) {
		t.Fatalf("stderr = %q, want it to name %s", got, PostCreateTimeoutEnv)
	}
	if !strings.Contains(got, `"5"`) || !strings.Contains(got, DefaultPostCreateTimeout.String()) {
		t.Fatalf("stderr = %q, want the operator's value and the default that replaced it", got)
	}
	_ = ResolvePostCreateTimeout()
	if got := stderr(); got != "" {
		t.Fatalf("second call re-warned: %q", got)
	}
}

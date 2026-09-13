package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// Everything between "the container/pod exists" and "the first node runs"
// is SETUP, and each phase carries its own bound. An unbounded phase does
// not fail — it waits, and a run waiting in setup has no `sandbox_started`
// event, holds its queue lease, and only dies when the run's own
// max_duration fires hours later. The helpers below are the ONE
// implementation of that bound: a phase added later cannot come with
// different failure semantics, and a phase shared by two drivers cannot
// be bounded on one and not the other.

// DefaultPostCreateTimeout bounds the post_create snippet on every driver
// that honours [Spec.PostCreate]. A hung snippet (a package install
// waiting on a dead mirror, a command that reads stdin) holds the run in
// setup with no typed failure and no redelivery.
//
// Thirty minutes because a post_create legitimately outlasts the phases
// around it — it is where a devcontainer installs its toolchain — so it
// carries its own budget rather than sharing another's. Overridable per
// host via [PostCreateTimeoutEnv].
const DefaultPostCreateTimeout = 30 * time.Minute

// PostCreateTimeoutEnv is the post_create override key. The budget belongs
// to the PHASE, not to a driver: an operator raising it for a slow
// toolchain install raises it everywhere the phase runs.
const PostCreateTimeoutEnv = "ITERION_SANDBOX_POST_CREATE_TIMEOUT"

// postCreateTimeoutWarnOnce bounds the "unparseable override" warning to
// one stderr line per process (the ITERION_BUDGET_EXIT_GRACE convention).
var postCreateTimeoutWarnOnce sync.Once

// ResolvePostCreateTimeout returns the effective post_create budget.
func ResolvePostCreateTimeout() time.Duration {
	return ResolvePhaseTimeout(PostCreateTimeoutEnv, DefaultPostCreateTimeout, &postCreateTimeoutWarnOnce)
}

// ResolvePhaseTimeout returns a setup phase's effective timeout,
// honouring its env override with a fail-safe fallback: a garbage or
// non-positive value ("banana", "0", "-5m", or "5" — which Go parses as
// five NANOseconds, not the five minutes the operator meant) falls back
// to the default (a setup phase left unbounded is exactly the bug this
// exists to close) and warns once, naming the key, the value and the
// default. One resolver for every phase, so a knob added later cannot
// come with different failure semantics.
func ResolvePhaseTimeout(envKey string, def time.Duration, warnOnce *sync.Once) time.Duration {
	raw := strings.TrimSpace(os.Getenv(envKey))
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		warnOnce.Do(func() {
			// Stderr, not a logger: a leaf helper with none in reach.
			fmt.Fprintf(os.Stderr,
				"iterion: %s=%q is not a positive Go duration (use e.g. 5m, 15m, 2h) — using the default %s\n",
				envKey, raw, def)
		})
		return def
	}
	return d
}

// phaseTimeoutWarnRatio is where the halfway-mark warning fires, as a
// fraction of the phase budget: a slow-but-healthy phase shows up on the
// runner log BEFORE the bound strikes, with enough runway left to tell
// "clogged and finishing" from "wedged". A var so tests can lower it.
var phaseTimeoutWarnRatio = 0.5

// RunWithPhaseTimeout runs fn under a bounded child context. When THIS
// phase's deadline strikes it returns an error naming the phase and the
// wall-clock elapsed time, wrapping [ErrPhaseTimeout],
// context.DeadlineExceeded AND fn's own error through errors.Join, so
// every consumer can classify the shape with errors.Is (the setup
// classifier routes it to failed_resumable, the runner NAKs it) and the
// operator still reads the actual cause. An outer ctx cancellation (run
// cancel, pod SIGTERM) keeps its own shape: a cooperative stop is not a
// stall.
//
// The bound is only as strong as fn's ctx discipline. fn is called
// synchronously and nothing races the deadline: the child ctx expires,
// and whatever fn does with that is the whole enforcement. For a
// subprocess phase that is exec.CommandContext killing the LOCAL process;
// a process the local one merely pipes into (the in-pod tar behind
// `kubectl exec`) is not reached by that signal (Setpgid signals only the
// leader), it dies with the pipe. A callee that ignores its ctx and
// returns nil after the deadline therefore completes — and is warned
// about, so a phase that burned its whole budget and won by a hair is
// visible before the next occurrence trips the bound.
//
// The halfway warning (phaseTimeoutWarnRatio × timeout) runs on a side
// goroutine that fn's return cancels; on an early return it emits
// nothing. Both warnings name envKey — the knob of THIS phase — so an
// operator reading them raises the bound that actually applies.
func RunWithPhaseTimeout(ctx context.Context, logger *iterlog.Logger, phase, envKey string, timeout time.Duration, fn func(context.Context) error) error {
	if logger == nil {
		logger = iterlog.Nop()
	}
	phaseCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()

	// The warn delay is computed here, not in the goroutine, so the
	// ratio is read before the goroutine exists (tests lower it).
	warnAfter := time.Duration(float64(timeout) * phaseTimeoutWarnRatio)
	done := make(chan struct{})
	go func() {
		select {
		case <-done:
		case <-time.After(warnAfter):
			logger.Warn("sandbox: %s phase still running after %s of its %s budget — a stall from here on fails the phase (%s raises the bound)",
				phase, time.Since(start).Round(time.Second), timeout, envKey)
		}
	}()

	err := fn(phaseCtx)
	close(done)

	if err == nil {
		// The clock, not only the context: the deadline is delivered by the
		// timer's own goroutine, which a starved scheduler can run AFTER a
		// callee that slept past the budget has already returned — the
		// overrun is then real and the context still reads nil.
		if phaseCtx.Err() != nil || time.Since(start) >= timeout {
			logger.Warn("sandbox: %s phase completed at or past its %s budget (elapsed %s) — raise %s or investigate the delay",
				phase, timeout, time.Since(start).Round(time.Millisecond), envKey)
		}
		return nil
	}
	if phaseCtx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
		return fmt.Errorf("sandbox: %s phase timed out after %s (deadline %s exceeded): %w",
			phase, time.Since(start).Round(time.Millisecond), timeout,
			errors.Join(ErrPhaseTimeout, context.DeadlineExceeded, err))
	}
	return err
}

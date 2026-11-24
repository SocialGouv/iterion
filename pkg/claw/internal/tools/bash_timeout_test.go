package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/claw/internal/permissions"
)

// TestExecuteBashKillsOrphanGrandchild guards against the hang where a
// grandchild process inherits stdout/stderr, outlives its bash parent
// after the timeout SIGKILL, and keeps the pipes open — wedging
// cmd.Wait() forever. The Setpgid + cmd.Cancel + WaitDelay combo must
// bring ExecuteBash back well under the 32s budget.
//
// The command spawns a bash that backgrounds a long-sleeping child
// inheriting stdout, then sleeps long enough to outlast the 30s bash
// timeout if the orphan held the pipe open. Without the fix this test
// would hang past `time` 60s. With the fix it returns inside ~32s
// (30s ctx timeout + 2s WaitDelay) with a typed timeout error.
func TestExecuteBashKillsOrphanGrandchild(t *testing.T) {
	// Pinned: this test asserts against the DEFAULT budget, so an ambient
	// CLAW_BASH_TIMEOUT must not reach it.
	t.Setenv("CLAW_BASH_TIMEOUT", "")
	start := time.Now()
	out, err := ExecuteBash(
		context.Background(),
		map[string]any{"command": "sleep 120 & sleep 120"},
		permissions.ModeAllow, "",
	)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected timeout error, got nil (output=%q)", out)
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("expected 'timed out' in error, got: %v", err)
	}
	// 30s timeout + 2s WaitDelay = ceiling 32s; give some slack for CI.
	if elapsed > 40*time.Second {
		t.Errorf("ExecuteBash hung past WaitDelay: took %s", elapsed)
	}
}

// TestExecuteBashHonorsCallerCancel verifies that cancelling the
// caller's context propagates to the spawned bash. Pre-fix, ctx was
// silently dropped and only the internal 30s timeout could stop a
// runaway command.
func TestExecuteBashHonorsCallerCancel(t *testing.T) {
	// Pinned: this test asserts against the DEFAULT budget, so an ambient
	// CLAW_BASH_TIMEOUT must not reach it.
	t.Setenv("CLAW_BASH_TIMEOUT", "")
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := ExecuteBash(
		ctx,
		map[string]any{"command": "sleep 60"},
		permissions.ModeAllow, "",
	)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected cancellation error, got nil")
	}
	// Should return within ~2s of cancel (200ms + WaitDelay margin).
	if elapsed > 5*time.Second {
		t.Errorf("ExecuteBash didn't honor caller cancel: took %s", elapsed)
	}
}

// TestBashTimeoutResolvesTheKnob covers the parse forms and the fallbacks
// without spawning anything: an unset or unparsable value must land on the
// default rather than on zero, since zero means "no bound here" and a typo
// must never silently remove the bound.
func TestBashTimeoutResolvesTheKnob(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  string
		want time.Duration
	}{
		{"unset falls back to the default", "", DefaultBashTimeout},
		{"a Go duration", "15m", 15 * time.Minute},
		{"a bare number is seconds", "90", 90 * time.Second},
		{"zero leaves the caller's context as the only bound", "0", 0},
		{"a typo must not remove the bound", "15minutes", DefaultBashTimeout},
		{"empty after trimming", "   ", DefaultBashTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CLAW_BASH_TIMEOUT", tc.env)
			if got := bashTimeout(); got != tc.want {
				t.Errorf("bashTimeout() = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestExecuteBashHonorsTheConfiguredTimeout proves the knob reaches the call
// and not only the resolver. The budget is deliberately SHORTER than
// DefaultBashTimeout: a test that raised it would pass just as well with the
// env ignored, since the command would finish either way.
func TestExecuteBashHonorsTheConfiguredTimeout(t *testing.T) {
	t.Setenv("CLAW_BASH_TIMEOUT", "1s")

	start := time.Now()
	_, err := ExecuteBash(
		context.Background(),
		map[string]any{"command": "sleep 20"},
		permissions.ModeAllow, "",
	)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error = %v, want a timeout", err)
	}
	if !strings.Contains(err.Error(), "CLAW_BASH_TIMEOUT") {
		t.Errorf("error = %v, want it to name the way out — a bound that does not say how to raise it sends the reader to the wrong place", err)
	}
	// 1s budget + 2s WaitDelay; anything near 30s means the env was ignored.
	if elapsed > 10*time.Second {
		t.Errorf("took %s — the configured budget was not applied", elapsed)
	}
}

// TestAZeroBudgetLeavesTheCallersContextAsTheOnlyBound pins the one thing a
// zero budget must do: step aside. It must NOT be installed as a deadline of
// zero — mutating the `limit > 0` branch to `true` does exactly that, and left
// the whole suite green while every bash call died instantly with empty output.
//
// It deliberately claims NOTHING about reaping the process group. An earlier
// version of this test did, and was inert: it passed a caller context WITH a
// deadline, whose Done is non-nil, which is the case the code already handled —
// a witness reachable another way. What a zero budget really costs, when the
// caller's context never cancels, is documented at the call site and is not
// testable here without hanging the suite.
func TestAZeroBudgetLeavesTheCallersContextAsTheOnlyBound(t *testing.T) {
	t.Setenv("CLAW_BASH_TIMEOUT", "0")

	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := ExecuteBash(ctx, map[string]any{"command": "sleep 60"}, permissions.ModeAllow, "")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected the caller's deadline to end the call")
	}
	// Under 200ms means a zero budget was installed as a deadline — the call
	// would be refused before running rather than bounded by its caller.
	if elapsed < 200*time.Millisecond {
		t.Errorf("returned in %s — a 0 budget was installed as a deadline instead of leaving the caller's context as the bound", elapsed)
	}
	// Past ~5s means the caller's deadline did not end the call at all.
	if elapsed > 5*time.Second {
		t.Errorf("returned in %s — the caller's deadline did not bound the call", elapsed)
	}
}

// TestASecondsValueThatOverflowsKeepsTheBound covers the way this knob loses its
// meaning by accident: int64(time.Duration) IS a count of nanoseconds, so an
// operator writing the budget in nanoseconds produces a NEGATIVE duration, which
// reads as "no bound" — the exact opposite of what they asked for.
func TestASecondsValueThatOverflowsKeepsTheBound(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  string
		want time.Duration
	}{
		{"the largest sane seconds value still resolves", "9223372036", 9223372036 * time.Second},
		{"one past it must not go negative", "9223372037", DefaultBashTimeout},
		{"the default spelled in nanoseconds", "30000000000", DefaultBashTimeout},
		{"fifteen minutes spelled in nanoseconds", "900000000000", DefaultBashTimeout},
		// A large NEGATIVE value wrapped back to a POSITIVE duration — measured
		// at 512ns, which refuses `echo hello` instantly while blaming the knob.
		{"a large negative must not wrap back to positive", "-9223372037", DefaultBashTimeout},
		{"the pathological negative that resolved to 512ns", "-15817289833210771", DefaultBashTimeout},
		{"an ordinary negative still means no bound", "-1", -1 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CLAW_BASH_TIMEOUT", tc.env)
			got := bashTimeout()
			if got != tc.want {
				t.Errorf("bashTimeout() = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestATimeoutMessageNamesWhoDecidedIt: DeadlineExceeded is true whether the
// budget or the CALLER's deadline fired. Naming the knob either way sends its
// reader — an LLM agent — at the wrong remedy, with a duration the call never
// ran for.
func TestATimeoutMessageNamesWhoDecidedIt(t *testing.T) {
	t.Setenv("CLAW_BASH_TIMEOUT", "")

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := ExecuteBash(ctx, map[string]any{"command": "sleep 60"}, permissions.ModeAllow, "")
	if err == nil {
		t.Fatal("expected a timeout")
	}
	if strings.Contains(err.Error(), "CLAW_BASH_TIMEOUT") {
		t.Errorf("error = %v — the caller's deadline fired, so pointing at the knob sends the reader to the wrong place", err)
	}
	if strings.Contains(err.Error(), DefaultBashTimeout.String()) {
		t.Errorf("error = %v — it names a duration the call never ran for", err)
	}
}

// TestTheTwoTimeoutDialsCompose pins the seam between the deployment dial
// (CLAW_BASH_TIMEOUT) and the per-call one the tool schema exposes
// (`timeout_seconds`).
//
// They are different powers and must not shadow each other: the env one is an
// operator setting the floor every call runs at, the input one is the model
// asking for room on a build or a test suite. An explicit ask therefore wins —
// a caller that names a bound has said something more specific than a default.
//
// Without this, merging the two features could leave either dial inert and
// every existing test would still pass: each one only ever exercises its own.
func TestTheTwoTimeoutDialsCompose(t *testing.T) {
	t.Run("an explicit ask overrides a longer deployment default", func(t *testing.T) {
		t.Setenv("CLAW_BASH_TIMEOUT", "300s")

		start := time.Now()
		_, err := ExecuteBash(
			context.Background(),
			map[string]any{"command": "sleep 60", "timeout_seconds": 1},
			permissions.ModeAllow, "",
		)
		if err == nil || !strings.Contains(err.Error(), "timed out") {
			t.Fatalf("error = %v, want a timeout at the caller's 1s, not the 300s default", err)
		}
		if !strings.Contains(err.Error(), "timeout_seconds") {
			t.Errorf("error = %v, want it to name the dial that decided — pointing at CLAW_BASH_TIMEOUT here sends the reader to a knob that would not have changed the outcome", err)
		}
		if elapsed := time.Since(start); elapsed > 20*time.Second {
			t.Errorf("took %s — the deployment default shadowed the caller's ask", elapsed)
		}
	})

	t.Run("the deployment default still applies with no explicit ask", func(t *testing.T) {
		t.Setenv("CLAW_BASH_TIMEOUT", "1s")

		start := time.Now()
		_, err := ExecuteBash(
			context.Background(),
			map[string]any{"command": "sleep 60"},
			permissions.ModeAllow, "",
		)
		if err == nil || !strings.Contains(err.Error(), "timed out") {
			t.Fatalf("error = %v, want the 1s deployment default to bound the call", err)
		}
		if elapsed := time.Since(start); elapsed > 20*time.Second {
			t.Errorf("took %s — the per-call dial shadowed the deployment default", elapsed)
		}
	})
}

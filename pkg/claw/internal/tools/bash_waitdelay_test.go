package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/claw/internal/permissions"
)

// TestExecuteBashWaitDelayOverrideNamesTheKnob is the witness for
// CLAW_PROC_WAIT_DELAY: a command that exits successfully while a
// background descendant keeps the output pipe open past the bound. With
// the override at 100ms the call returns in well under a second (not
// after the default 2s, and certainly not after the descendant's 30s),
// the early output survives, and the error NAMES the knob and says the
// tail may be missing — the DrainCutShort rule for a tool whose answer
// is the bytes (#2469's split, iterion #2851). A hard-coded bound (the
// mutation this witnesses against) reddens the elapsed assertion.
func TestExecuteBashWaitDelayOverrideNamesTheKnob(t *testing.T) {
	t.Setenv("CLAW_BASH_TIMEOUT", "")
	t.Setenv("CLAW_PROC_WAIT_DELAY", "100ms")
	start := time.Now()
	out, err := ExecuteBash(
		context.Background(),
		map[string]any{"command": "echo early; sleep 30 &"},
		permissions.ModeAllow, "",
	)
	elapsed := time.Since(start)

	if elapsed > time.Second {
		t.Errorf("the 100ms drain override did not apply: the call took %s (a hard-coded 2s bound, or none, would show here)", elapsed)
	}
	if err == nil {
		t.Fatalf("expected the drain cut to stay an error, got nil (output=%q)", out)
	}
	if !strings.Contains(err.Error(), "CLAW_PROC_WAIT_DELAY") {
		t.Errorf("the cut error must name the knob, got: %v", err)
	}
	if !strings.Contains(err.Error(), "tail may be missing") {
		t.Errorf("the cut error must say the output may be truncated, got: %v", err)
	}
	if !strings.Contains(out, "early") {
		t.Errorf("the output written BEFORE the shell exited must survive the cut, got: %q", out)
	}
}

// TestExecuteBashWaitDelayZeroWaitsForTheDescendant pins the escape
// hatch: 0 removes the bound, so the call waits for the descendant
// holding the pipe and returns the COMPLETE output with no cut error.
// The descendant lives 1s — long enough that a bound below it would cut,
// short enough for CI.
func TestExecuteBashWaitDelayZeroWaitsForTheDescendant(t *testing.T) {
	t.Setenv("CLAW_BASH_TIMEOUT", "")
	t.Setenv("CLAW_PROC_WAIT_DELAY", "0")
	start := time.Now()
	_, err := ExecuteBash(
		context.Background(),
		map[string]any{"command": "sleep 1 &"},
		permissions.ModeAllow, "",
	)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("unbounded drain with a descendant that exits: expected success, got %v", err)
	}
	if elapsed < 900*time.Millisecond {
		t.Errorf("the unbounded drain did not wait for the descendant: returned in %s", elapsed)
	}
}

// TestExecuteBashWaitDelayNegativeIsUnbounded pins the normalization on
// BOTH parse paths: os/exec arms its drain timer only for a POSITIVE
// WaitDelay, so a raw negative would fire it at once — cutting every
// drain, the opposite of the documented reading. Negative normalizes to
// 0 (unbounded, the bashTimeout shape): the call waits out the
// descendant and answers without the cut error. "-3s" exercises the
// duration path, "-5" the bare-seconds one — dropping either path's
// normalization reddens its case.
func TestExecuteBashWaitDelayNegativeIsUnbounded(t *testing.T) {
	for _, value := range []string{"-3s", "-5"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("CLAW_BASH_TIMEOUT", "")
			t.Setenv("CLAW_PROC_WAIT_DELAY", value)
			out, err := ExecuteBash(
				context.Background(),
				map[string]any{"command": "sleep 1 &"},
				permissions.ModeAllow, "",
			)
			if err != nil {
				t.Fatalf("a negative bound must read as unbounded, not cut at 0s: %v (output %q)", err, out)
			}
		})
	}
}

// TestExecuteBashWaitDelayDefaultSurvives pins the site default: with no
// override the drain is still bounded (the descendant's 30s does not
// park the call) at the site's own 2s number — overridable but never
// silently absent.
func TestExecuteBashWaitDelayDefaultSurvives(t *testing.T) {
	t.Setenv("CLAW_BASH_TIMEOUT", "")
	t.Setenv("CLAW_PROC_WAIT_DELAY", "")
	start := time.Now()
	_, err := ExecuteBash(
		context.Background(),
		map[string]any{"command": "sleep 30 &"},
		permissions.ModeAllow, "",
	)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a descendant holding the pipe past the default bound: expected the cut error, got nil")
	}
	if elapsed < 1500*time.Millisecond || elapsed > 6*time.Second {
		t.Errorf("the default 2s drain bound did not decide the call: took %s", elapsed)
	}
}

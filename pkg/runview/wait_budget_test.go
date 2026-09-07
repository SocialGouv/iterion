package runview

import (
	"testing"
	"time"
)

// waitBudget turns a caller's unloaded-machine wait into the bound that
// actually fails it.
//
// A hand-picked wall clock describes the handoff; it is not a bound a busy
// machine should be able to reach. `go test ./...` runs packages concurrently,
// and a merge-queue build runs several groups on one shared runner, so the
// engine lifecycles these tests wait on take multiples of their quiet-machine
// time — which turned TestServiceLaunch_SubbotReattachAfterRestart's 30 s into
// a merge-queue ejector (35 s, group build 34116255645).
//
// Two derivations, no third number invented: the caller's figure times
// waitSlowdown, clamped to what is left of `-timeout` minus a margin, so the
// wait always fails as its own assertion — naming what never happened —
// rather than as a package-wide panic naming whichever test was in flight.
// Never shorter than the caller asked for.
func waitBudget(t *testing.T, within time.Duration) time.Duration {
	t.Helper()
	budget := within * waitSlowdown
	if dl, ok := t.Deadline(); ok {
		if room := time.Until(dl) - waitDeadlineMargin; room < budget {
			budget = room
		}
	}
	if budget < within {
		budget = within
	}
	return budget
}

const (
	// waitSlowdown is how much slower a contended runner is than the quiet
	// machine a caller's figure was measured on. 6 covers the ~7x observed
	// between this repo's developer boxes and a five-group merge build
	// (3.6 ms vs 27 ms per card on the same store sweep) without the clamp
	// below; past the clamp, `-timeout` decides.
	waitSlowdown = 6
	// waitDeadlineMargin is what a scaled wait leaves of `-timeout` for the
	// failure to be reported and the package to unwind.
	waitDeadlineMargin = 30 * time.Second
)

// TestWaitBudgetIsDerivedNotPicked keeps the helper from decaying into the
// caller's own constant: it must scale, and it must never outlive the
// harness deadline it is supposed to fail inside of.
func TestWaitBudgetIsDerivedNotPicked(t *testing.T) {
	base := time.Second
	got := waitBudget(t, base)
	if got < base {
		t.Fatalf("budget %s is under the caller's %s — the clamp may only shorten a scale-up", got, base)
	}
	dl, hasDeadline := t.Deadline()
	if !hasDeadline {
		if want := base * waitSlowdown; got != want {
			t.Fatalf("budget with no -timeout = %s, want %s", got, want)
		}
		return
	}
	room := time.Until(dl) - waitDeadlineMargin
	if room > base*waitSlowdown && got != base*waitSlowdown {
		t.Fatalf("budget = %s, want the %dx scale-up (%s room left)", got, waitSlowdown, room)
	}
	if g := waitBudget(t, 24*time.Hour); g > room && g != 24*time.Hour {
		t.Fatalf("a 24h wait was not clamped to the %s left of -timeout: %s", room, g)
	}
}

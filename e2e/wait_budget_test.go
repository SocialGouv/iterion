package e2e

import (
	"flag"
	"strconv"
	"testing"
	"time"
)

// TestWaitBudgetScalesWithLoad pins the two derivations waitBudget promises.
// Without them a caller's hand-picked wall clock is the bound again, and a
// loaded runner reaches it while nothing is wrong — the ejection class of
// #860 (TestDispatcherE2E_CancelInFlight: 10.07 s of a 10 s budget on a
// five-group merge build).
//
// Serial: it mutates the process-wide `test.parallel` flag.
func TestWaitBudgetScalesWithLoad(t *testing.T) {
	f := flag.Lookup("test.parallel")
	if f == nil {
		t.Fatal("test.parallel is gone — waitLoadFactor is stuck at 1 and every wait is back to its unscaled budget")
	}
	restore := f.Value.String()
	t.Cleanup(func() {
		if err := f.Value.Set(restore); err != nil {
			t.Fatalf("restore test.parallel: %v", err)
		}
	})

	set := func(n int) {
		t.Helper()
		if err := f.Value.Set(strconv.Itoa(n)); err != nil {
			t.Fatalf("set test.parallel=%d: %v", n, err)
		}
	}

	set(1)
	if got := waitLoadFactor(); got != 1 {
		t.Fatalf("load factor at -parallel 1 = %d, want 1", got)
	}
	set(6)
	if got := waitLoadFactor(); got != 6 {
		t.Fatalf("load factor at -parallel 6 = %d, want 6", got)
	}

	// The scale is what a busy machine gets; the clamp is what keeps it
	// inside `-timeout`. Both are exercised through a sub-test with a real
	// harness deadline (this package always runs with one in CI).
	base := 2 * time.Second
	got := waitBudget(t, base)
	if got < base {
		t.Fatalf("budget %s is under the caller's own %s — the clamp may only shorten a scale-up", got, base)
	}
	if dl, ok := t.Deadline(); ok {
		if room := time.Until(dl) - waitDeadlineMargin; room > base && got > room {
			t.Fatalf("budget %s exceeds the %s left of -timeout — a wait would fail as a package panic, not as its own assertion", got, room)
		}
	} else if want := base * 6; got != want {
		t.Fatalf("budget with no deadline = %s, want %s (6x)", got, want)
	}

	// A budget wider than what -timeout has left is clamped, never grown.
	huge := 24 * time.Hour
	if dl, ok := t.Deadline(); ok {
		room := time.Until(dl) - waitDeadlineMargin
		if g := waitBudget(t, huge); g != huge && g > room {
			t.Fatalf("budget for a %s wait = %s, above the %s remaining", huge, g, room)
		}
	}
}

// TestReserveLoopbackPortNeverRepeats: the in-process half of the
// reserve-then-release race. Two parallel tests inside that window used to
// be able to receive the same port, which surfaced as a daemon failing to
// bind ("address already in use") in an unrelated test's log.
func TestReserveLoopbackPortNeverRepeats(t *testing.T) {
	t.Parallel()
	seen := map[int]bool{}
	for i := 0; i < 64; i++ {
		p := reserveLoopbackPort(t)
		if seen[p] {
			t.Fatalf("port %d handed out twice", p)
		}
		seen[p] = true
	}
	// The draw above depends on the kernel not re-offering a port it just
	// took back, which it usually does not — so assert the mechanism that
	// makes the guarantee, not only the outcome.
	probe := 1 // never in the ephemeral range, so no real reservation collides
	if !claimLoopbackPort(probe) {
		t.Fatalf("port %d read as already claimed before anyone claimed it", probe)
	}
	if claimLoopbackPort(probe) {
		t.Fatalf("port %d claimed twice — the claim set is inert and the draw above proves nothing", probe)
	}
}

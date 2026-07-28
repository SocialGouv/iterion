package runtime

import (
	"fmt"
	goruntime "runtime"
	"testing"
	"time"
)

// The cancellation and wedged-branch tests in this package all share one
// shape: run the engine in a goroutine and assert it comes back before a
// wall-clock budget. Two of them flaked on 2026-07-28 during a loaded
// `task check` — TestFanOutBoundedCancellationNoDeadlock at exactly 5.00s (its
// own deadline) and TestFanOutInternalCancellationAbandonsWedgedBranch at
// 5.526s against a 2s ceiling — and both passed in isolation immediately after.
//
// A tempting cause was checked and REJECTED: branchCancelGracePeriod defaults to
// 5s and the first test's budget is also 5s, but forcing
// ITERION_BRANCH_CANCEL_GRACE to 10s leaves it green in 0.13s, so the drain does
// not wait the grace there and the two 5s values are a coincidence.
//
// So these are absolute wall-clock bounds giving way under contention, with no
// cause to remove. What they lacked was any way to tell a genuine hang from a
// slow host after the fact, and any warning that the margin was shrinking
// before it ran out — which is what the helpers below add.

// awaitRunWithin waits for an engine goroutine to deliver its result, and
// returns how long that took along with the result.
//
// A wait that eats more than half its budget is logged: raising a deadline is
// the reflex for a timing flake, and doing it silently trades a visible flake
// for an invisible regression — the assertion keeps passing while the thing it
// watches gets slower, until it crosses the new ceiling too. Nobody could say
// whether the passing runs of these tests took 30ms or 4.9s.
//
// A wait that never completes fails with a goroutine dump. Every one of these
// budgets guards a cross-goroutine convergence — a collector draining branches,
// a semaphore releasing — so "it never came back" is nearly always "something
// is parked somewhere", and a CI failure is the only chance to see where.
func awaitRunWithin(t *testing.T, done <-chan error, budget time.Duration, what string) (time.Duration, error) {
	t.Helper()
	start := time.Now()
	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case err := <-done:
		elapsed := time.Since(start)
		if elapsed > budget/2 {
			t.Logf("waited %s of a %s budget for %s — passing, but it used most of its margin",
				elapsed.Round(time.Millisecond), budget, what)
		}
		return elapsed, err
	case <-timer.C:
		t.Fatalf("timed out after %s waiting for %s\n%s", budget, what, goroutineDump())
		return 0, nil
	}
}

// requireReturnedWithin asserts a latency CEILING — the run had to come back
// promptly, not merely eventually — and always records what was observed.
//
// Always, not only on a breach: this is the inverse of awaitRunWithin's
// half-budget rule. There the budget is generous and only a near-miss is
// interesting; here the ceiling IS the assertion, so the trend towards it is
// the whole signal, and a green run that quietly drifted from 40ms to 1.9s is
// exactly the thing that turns into the next flake.
func requireReturnedWithin(t *testing.T, elapsed, ceiling time.Duration, what string) {
	t.Helper()
	t.Logf("%s returned in %s (ceiling %s)", what, elapsed.Round(time.Millisecond), ceiling)
	if elapsed > ceiling {
		t.Fatalf("%s returned too slowly: %s > %s\n%s", what, elapsed, ceiling, goroutineDump())
	}
}

// goroutineDump renders every goroutine's stack, capped so a failure stays
// readable in a CI log.
func goroutineDump() string {
	buf := make([]byte, 512<<10)
	n := goruntime.Stack(buf, true)
	const limit = 64 << 10
	if n > limit {
		return fmt.Sprintf("%s\n... goroutine dump truncated (%d of %d bytes)", buf[:limit], limit, n)
	}
	return string(buf[:n])
}

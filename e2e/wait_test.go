package e2e

import (
	"flag"
	"fmt"
	goruntime "runtime"
	"strconv"
	"testing"
	"time"
)

// waitUntil polls cond until it holds, failing the test if it has not by
// `within`.
//
// It replaces the hand-rolled deadline loop this suite had at two dozen call
// sites, and exists for two properties those loops did not have:
//
//   - A slow-but-passing wait is REPORTED. Widening a deadline is the reflex
//     for a timing flake, and it silently converts a visible flake into an
//     invisible regression: the assertion keeps passing while the thing it
//     watches gets steadily slower, until it crosses the new ceiling too. The
//     "used most of its budget" log line is the only signal that shows up
//     before that happens. TestDispatcherE2E_CancelInFlight has already been
//     widened twice (2s → 10s) and still flaked; nobody could say whether the
//     passing runs took 30ms or 9s.
//   - A failure dumps the goroutines. Every wait here watches a
//     cross-goroutine handoff — an actor draining a command channel, a worker
//     returning, a claim being released — so "it never happened" is nearly
//     always "something is parked somewhere". On CI there is no second chance
//     to attach a debugger, and a bare "cancel did not flush running entry"
//     names the symptom while withholding every fact needed to fix it.
//
// `what` completes the sentence "timed out waiting for …". Optional `detail`
// closures are evaluated AT FAILURE TIME (a snapshot captured at call time
// would predate the wait, which is exactly when it is useless).
//
// `within` is the budget on an unloaded machine. What actually FAILS the wait
// is waitBudget(t, within) — that figure scaled by how many tests share the
// CPU and clamped to the test's own deadline. A caller's hand-picked wall
// clock is fine as a description of the handoff; it is not a bound a loaded
// runner should be able to reach, which is how a healthy suite ejects PRs
// from the merge queue (#860, and TestDispatcherE2E_CancelInFlight at
// 10.07 s of a 10 s budget on a five-group merge build). The drift log below
// still measures against the UNSCALED `within`, so the "it is getting
// slower" signal keeps its original sensitivity.
func waitUntil(t *testing.T, within time.Duration, what string, cond func() bool, detail ...func() string) {
	t.Helper()
	const poll = 20 * time.Millisecond
	budget := waitBudget(t, within)
	start := time.Now()
	deadline := start.Add(budget)
	for {
		if cond() {
			// Half the budget: enough headroom that a normally-scheduled run
			// stays quiet, tight enough that the drift preceding the next
			// flake is on the record.
			if elapsed := time.Since(start); elapsed > within/2 {
				t.Logf("waited %s of a %s budget for %s — passing, but it used most of its margin",
					elapsed.Round(time.Millisecond), within, what)
			}
			return
		}
		if !time.Now().Before(deadline) {
			break
		}
		time.Sleep(poll)
	}
	msg := fmt.Sprintf("timed out after %s (%s scaled for load) waiting for %s", budget, within, what)
	for _, d := range detail {
		if d != nil {
			msg += "\n" + d()
		}
	}
	t.Fatalf("%s\n%s", msg, goroutineDump())
}

// waitBudget turns a caller's unloaded-machine budget into the one that
// actually fails the wait.
//
// Two derivations, no hand-picked number:
//   - Load. This package runs its tests in parallel, so up to `-parallel` of
//     them share the CPU with the background loop each is waiting on.
//     Multiplying by that count is what keeps a wait describing the handoff
//     rather than the scheduler.
//   - The harness deadline. The result is clamped to what is left of
//     `-timeout` minus a margin, so a scaled wait always fails as this
//     assertion (naming what never happened, with the goroutine dump) rather
//     than as a package-wide panic naming whichever test was in flight.
//
// Never returns less than `within`: the clamp may only shorten a scale-up.
func waitBudget(t *testing.T, within time.Duration) time.Duration {
	t.Helper()
	budget := within * time.Duration(waitLoadFactor())
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

// waitDeadlineMargin is how much of `-timeout` a scaled wait leaves for the
// failure to be reported (the goroutine dump alone can be 64 KiB) and for
// the rest of the package to unwind.
const waitDeadlineMargin = 30 * time.Second

// waitLoadFactor is the effective `-parallel`, i.e. how many of this
// package's tests may be running at once. 1 when the flag is unreadable, so
// an unknown harness never inflates a budget.
func waitLoadFactor() int {
	f := flag.Lookup("test.parallel")
	if f == nil {
		return 1
	}
	n, err := strconv.Atoi(f.Value.String())
	if err != nil || n < 1 {
		return 1
	}
	return n
}

// goroutineDump renders every goroutine's stack, capped so a failure stays
// readable in a CI log. The parked goroutine is the answer to "why did this
// never happen", and a CI failure is the only chance to see it.
func goroutineDump() string {
	buf := make([]byte, 512<<10)
	n := goruntime.Stack(buf, true)
	const limit = 64 << 10
	if n > limit {
		return fmt.Sprintf("%s\n... goroutine dump truncated (%d of %d bytes)",
			buf[:limit], limit, n)
	}
	return string(buf[:n])
}

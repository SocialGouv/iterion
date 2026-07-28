package e2e

import (
	"fmt"
	goruntime "runtime"
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
func waitUntil(t *testing.T, within time.Duration, what string, cond func() bool, detail ...func() string) {
	t.Helper()
	const poll = 20 * time.Millisecond
	start := time.Now()
	deadline := start.Add(within)
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
	msg := fmt.Sprintf("timed out after %s waiting for %s", within, what)
	for _, d := range detail {
		if d != nil {
			msg += "\n" + d()
		}
	}
	t.Fatalf("%s\n%s", msg, goroutineDump())
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

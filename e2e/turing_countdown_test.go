package e2e

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestTuringCountdown is the executable proof for ADR-050 / docs/dsl-totality-and-tc.md:
// the DSL's bounded skeleton (an `unbounded` loop + a `compute` node + a
// self-feeding `with:` accumulator + a `when`-exit) expresses a
// while-loop-with-state and TERMINATES BY ITS OWN LOGIC — not by exhausting the
// fuel ceiling. examples/turing/countdown.bot counts n from `start` down to 0.
//
// The decisive assertion is the execution count: the loop's fuel is 200, so a
// run that converged via the `when done` edge executes `step` exactly start+1
// times; a run that only stopped because it ran out of fuel would execute it
// ~200 times. Counting proves the former.
func TestTuringCountdown(t *testing.T) {
	// The bot's `start` var defaults to 5; the run uses that default.
	const start = 5
	wf := compileFixture(t, "turing/countdown.bot")

	s := tmpStore(t)
	// The bot has no LLM/tool nodes, so the executor is never invoked; a bare
	// stub satisfies the engine constructor.
	eng := runtime.New(wf, s, newScenarioExecutor())

	if err := eng.Run(context.Background(), "e2e-turing-countdown", nil); err != nil {
		t.Fatalf("run countdown: %v", err)
	}

	r, err := s.LoadRun(context.Background(), "e2e-turing-countdown")
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if r.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want finished", r.Status)
	}

	// Final state: n decremented all the way to 0 and the base case fired.
	art, err := s.LoadLatestArtifact(context.Background(), "e2e-turing-countdown", "step")
	if err != nil {
		t.Fatalf("load latest step artifact: %v", err)
	}
	if got := toInt(art.Data["n"]); got != 0 {
		t.Errorf("final n = %d, want 0", got)
	}
	if art.Data["done"] != true {
		t.Errorf("final done = %v, want true", art.Data["done"])
	}

	// Convergence-by-logic, not by-fuel: step ran exactly start+1 times
	// (n = 5,4,3,2,1,0). The loop's fuel is 200; a fuel-bounded run would show
	// ~200 executions. This is the property a static halting proof can't give
	// but the runtime delivers.
	events, err := s.LoadEvents(context.Background(), "e2e-turing-countdown")
	if err != nil {
		t.Fatalf("load events: %v", err)
	}
	var stepStarts int
	for _, ev := range events {
		if ev.Type == store.EventNodeStarted && ev.NodeID == "step" {
			stepStarts++
		}
	}
	if want := start + 1; stepStarts != want {
		t.Errorf("step executed %d times, want %d (converged via when-exit, not fuel=200)", stepStarts, want)
	}
}

// toInt coerces a JSON-decoded numeric (float64 / int64 / int) to int for
// assertion. Compute outputs round-trip through JSON, so integers may arrive as
// float64.
func toInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	default:
		return -1
	}
}

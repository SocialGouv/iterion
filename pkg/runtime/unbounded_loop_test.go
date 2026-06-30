package runtime

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

func newEngineWith(t *testing.T, wf *ir.Workflow) *Engine {
	t.Helper()
	return New(wf, tmpStore(t), newStubExecutor())
}

// TestResolveLoopMax_Unbounded checks the fuel-ceiling precedence for an
// unbounded loop: per-loop fuel > budget.max_iterations > hard default.
func TestResolveLoopMax_Unbounded(t *testing.T) {
	cases := []struct {
		name   string
		fuel   int
		budget int
		want   int
	}{
		{"per-loop fuel wins", 200, 50, 200},
		{"budget fallback", 0, 50, 50},
		{"hard default", 0, 0, defaultUnboundedFuel},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wf := &ir.Workflow{
				Name:  "w",
				Nodes: map[string]ir.Node{},
				Loops: map[string]*ir.Loop{},
			}
			if tc.budget > 0 {
				wf.Budget = &ir.Budget{MaxIterations: tc.budget}
			}
			loop := &ir.Loop{Name: "l", Unbounded: true, FuelCap: tc.fuel}
			eng := newEngineWith(t, wf)
			rs := eng.newRunState("r", nil)
			if got := eng.resolveLoopMax(loop, rs); got != tc.want {
				t.Errorf("resolveLoopMax = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestLoopStalled verifies the liveness monitor: identical outputs accumulate
// staleness up to the threshold, and any change resets the window.
func TestLoopStalled(t *testing.T) {
	wf := &ir.Workflow{Name: "w", Nodes: map[string]ir.Node{}, Loops: map[string]*ir.Loop{}}
	eng := newEngineWith(t, wf)
	rs := eng.newRunState("r", nil)

	same := map[string]interface{}{"approved": false, "n": 1}
	// First crossing: records signature, not stalled.
	if eng.loopStalled("l", same, rs) {
		t.Fatal("stalled on first crossing")
	}
	// Repeats accumulate; stalls once unchanged for maxLoopStall crossings.
	stalledAt := -1
	for i := 1; i <= maxLoopStall+2; i++ {
		if eng.loopStalled("l", same, rs) {
			stalledAt = i
			break
		}
	}
	if stalledAt != maxLoopStall {
		t.Errorf("stalled at crossing %d, want %d", stalledAt, maxLoopStall)
	}

	// A changed output resets the staleness window.
	rs2 := eng.newRunState("r2", nil)
	eng.loopStalled("l", map[string]interface{}{"n": 1}, rs2)
	eng.loopStalled("l", map[string]interface{}{"n": 1}, rs2)
	if eng.loopStalled("l", map[string]interface{}{"n": 2}, rs2) {
		t.Fatal("changed output should reset staleness, not report stalled")
	}
	if rs2.loopStaleness["l"] != 0 {
		t.Errorf("staleness after change = %d, want 0", rs2.loopStaleness["l"])
	}
}

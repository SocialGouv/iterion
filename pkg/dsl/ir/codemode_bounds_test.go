package ir

import (
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/scripthost"
)

// The bounds ladder and the three diagnostics the `codemode:` block earns.
//
// What each test exists to catch is written on it, because a bound is a thing
// whose absence is invisible: a node that silently runs under the defaults
// looks exactly like a node whose declaration was honoured, until the day a
// program is refused at 64 calls with nothing in the run saying why.

func TestTheNodesBoundsBeatTheWorkflowsFieldByField(t *testing.T) {
	t.Parallel()
	wf := &CodemodeBounds{ToolCalls: 256, Wall: 2 * time.Minute, Memory: 1 << 29}
	node := &CodemodeBounds{Wall: 5 * time.Minute}

	got := ResolveCodemodeBounds(node, wf)
	if got == nil {
		t.Fatalf("resolution dropped both rungs")
	}
	// The node's own field wins where it set one.
	if got.Wall != 5*time.Minute {
		t.Errorf("Wall = %s, want the node's 5m", got.Wall)
	}
	// And the workflow's survives where the node said nothing. This is the
	// assertion that fails if the ladder is ever made nearest-rung-wins:
	// a workflow raising tool_calls for a whole bot would be erased by a
	// node that only wanted a longer wall.
	if got.ToolCalls != 256 {
		t.Errorf("ToolCalls = %d, want the workflow's 256 to survive a node that did not set it", got.ToolCalls)
	}
	if got.Memory != 1<<29 {
		t.Errorf("Memory = %d, want the workflow's 1<<29 to survive", got.Memory)
	}
	// A field neither rung set stays zero, which is what Clamp reads as
	// "nobody chose" — the IR does not pre-fill defaults, or a stale value
	// would travel as though an author had asked for it.
	if got.CPU != 0 {
		t.Errorf("CPU = %s, want 0 (nobody chose) rather than a pre-filled default", got.CPU)
	}
}

func TestNeitherRungDeclaringBoundsResolvesToNothing(t *testing.T) {
	t.Parallel()
	if got := ResolveCodemodeBounds(nil, nil); got != nil {
		t.Errorf("ResolveCodemodeBounds(nil, nil) = %+v, want nil — an empty bound set would travel as a declaration", got)
	}
}

func TestTheCeilingCensusNamesEveryFieldAboveIt(t *testing.T) {
	t.Parallel()
	ceil := scripthost.Ceiling()
	// Every one of the seven, one past its ceiling at once: the census has
	// to name them all, not stop at the first. A `return` on the first
	// finding would leave six fields clamped with nothing said.
	over := CodemodeBoundsAboveCeiling(&CodemodeBounds{
		ToolCalls:       ceil.ToolCalls + 1,
		Wall:            ceil.Wall + time.Second,
		CPU:             ceil.CPU + time.Second,
		Memory:          ceil.Memory + 1,
		ScriptBytes:     ceil.ScriptBytes + 1,
		ResultBytes:     ceil.ResultBytes + 1,
		ToolResultBytes: ceil.ToolResultBytes + 1,
	})
	if len(over) != 7 {
		t.Fatalf("named %d fields above the ceiling, want all 7: %v", len(over), over)
	}
	for _, want := range []string{"tool_calls", "wall", "cpu", "memory", "script_bytes", "result_bytes", "tool_result_bytes"} {
		found := false
		for _, phrase := range over {
			if strings.HasPrefix(phrase, want+" ") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s is above its ceiling and the census does not name it: %v", want, over)
		}
	}
}

func TestABoundExactlyAtTheCeilingIsNotAboveIt(t *testing.T) {
	t.Parallel()
	ceil := scripthost.Ceiling()
	// The boundary, both halves. A `>=` here would warn about the one value
	// an operator is most likely to write deliberately — the ceiling itself.
	if over := CodemodeBoundsAboveCeiling(&ceil); len(over) > 0 {
		t.Errorf("the ceiling itself is reported above the ceiling: %v", over)
	}
	if over := CodemodeBoundsAboveCeiling(nil); len(over) > 0 {
		t.Errorf("no bounds reported above the ceiling: %v", over)
	}
}

func TestTheDefaultsAreUnderTheCeiling(t *testing.T) {
	t.Parallel()
	// Not a tautology: these are two independently written tables, and a
	// default above its own ceiling would make every un-declaring node warn.
	def := scripthost.DefaultLimits()
	if over := CodemodeBoundsAboveCeiling(&def); len(over) > 0 {
		t.Errorf("DefaultLimits is above Ceiling in %v — every node that declares nothing would be clamped", over)
	}
}

// The one-type decision, asserted at COMPILE time because that is when it can
// be broken. If CodemodeBounds is ever made a parallel struct, the seven
// numbers acquire a second spelling and the `wall_ms` defect the host's own
// field doc records — a hand-written bound enforced a million times tighter —
// becomes available again. Two distinct named struct types are not assignable
// in Go however identical their fields, so this line stops compiling the day
// the alias is replaced by a copy.
var _ scripthost.Limits = CodemodeBounds{}

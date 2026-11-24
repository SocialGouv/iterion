package ir

import (
	"reflect"
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

	got := ResolveCodemodeBounds(nil, node, wf)
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
	if got := ResolveCodemodeBounds(nil, nil, nil); got != nil {
		t.Errorf("ResolveCodemodeBounds(nil, nil, nil) = %+v, want nil — an empty bound set would travel as a declaration", got)
	}
}

func TestTheCeilingCensusNamesEveryFieldAboveIt(t *testing.T) {
	t.Parallel()
	ceil := scripthost.Ceiling()
	// Every one of the bounds, one past its ceiling at once: the census has
	// to name them all, not stop at the first. A `return` on the first
	// finding would leave the remaining fields clamped with nothing said.
	over := CodemodeBoundsAboveCeiling(&CodemodeBounds{
		ToolCalls:           ceil.ToolCalls + 1,
		Wall:                ceil.Wall + time.Second,
		CPU:                 ceil.CPU + time.Second,
		Memory:              ceil.Memory + 1,
		ScriptBytes:         ceil.ScriptBytes + 1,
		ResultBytes:         ceil.ResultBytes + 1,
		ToolResultBytes:     ceil.ToolResultBytes + 1,
		ConcurrentToolCalls: ceil.ConcurrentToolCalls + 1,
	})
	wants := []string{"tool_calls", "concurrent_tool_calls", "wall", "cpu", "memory", "script_bytes", "result_bytes", "tool_result_bytes"}
	if len(over) != len(wants) {
		t.Fatalf("named %d fields above the ceiling, want %d: %v", len(over), len(wants), over)
	}
	for _, want := range wants {
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
// be broken. If CodemodeBounds is ever made a parallel struct, the
// numbers acquire a second spelling and the `wall_ms` defect the host's own
// field doc records — a hand-written bound enforced a million times tighter —
// becomes available again. Two distinct named struct types are not assignable
// in Go however identical their fields, so this line stops compiling the day
// the alias is replaced by a copy.
var _ scripthost.Limits = CodemodeBounds{}

// Each bound has HAND-WRITTEN entries in four places — the
// parser's switch, ast.CodemodeBlock.Bounded, ResolveCodemodeBounds's literal
// and CodemodeBoundsAboveCeiling's calls. A new field added to
// scripthost.Limits would be accepted by none of them, and
// ResolveCodemodeBounds would ERASE it: it builds its result from a field
// literal, so a value a node set would come back zero on every codemode node.
//
// These two census tests read the field set from the TYPE, so the next bound
// cannot land without an author surface. Written after an adversarial round
// added an eighth bound properly (field, DefaultLimits, Ceiling, Clamp) and
// measured: pkg/scripthost noticed, and nothing in the DSL layer did.
//
// The census is also why the two sibling tests above may keep their explicit
// literals: those assert the SHAPE of a report, this one asserts COVERAGE.
func TestEveryBoundOfTheHostIsNamedByTheCeilingCensus(t *testing.T) {
	t.Parallel()
	ceil := scripthost.Ceiling()
	rt := reflect.TypeOf(ceil)
	over := reflect.New(rt).Elem()
	// Every field one past its ceiling at once, set by reflection rather
	// than by name.
	for i := 0; i < rt.NumField(); i++ {
		cv := reflect.ValueOf(ceil).Field(i)
		over.Field(i).SetInt(cv.Int() + 1)
	}
	bounds := over.Interface().(scripthost.Limits)
	named := CodemodeBoundsAboveCeiling(&bounds)
	if len(named) != rt.NumField() {
		t.Fatalf("scripthost.Limits has %d bounds, the census named %d: %v — a bound the census cannot see is a bound clamped with nothing said",
			rt.NumField(), len(named), named)
	}
}

func TestEveryBoundOfTheHostSurvivesResolution(t *testing.T) {
	t.Parallel()
	rt := reflect.TypeOf(scripthost.Limits{})
	for i := 0; i < rt.NumField(); i++ {
		name := rt.Field(i).Name
		node := reflect.New(rt).Elem()
		// A value no default and no ceiling can collide with, so a field
		// that comes back right cannot have come back right by accident.
		node.Field(i).SetInt(7)
		asked := node.Interface().(scripthost.Limits)
		if name == "StoreEntries" || name == "StoreBytes" {
			entries, bytes := ResolveCodemodeStoreLimits(nil, &asked)
			if (name == "StoreEntries" && entries != 7) || (name == "StoreBytes" && bytes != 7) {
				t.Errorf("%s: declared 7, bank resolved entries=%d bytes=%d", name, entries, bytes)
			}
			if got := ResolveCodemodeBounds(nil, nil, &asked); got != nil && (got.StoreEntries != 0 || got.StoreBytes != 0) {
				t.Errorf("%s: run bank quotas leaked into the per-program resolver: %+v", name, got)
			}
			continue
		}
		got := ResolveCodemodeBounds(nil, &asked, nil)
		if got == nil {
			t.Fatalf("%s: resolution dropped a declared bound set entirely", name)
		}
		if v := reflect.ValueOf(*got).Field(i).Int(); v != 7 {
			t.Errorf("%s: declared 7, resolved %d — the field literal in ResolveCodemodeBounds does not carry it", name, v)
		}
	}
}

// The (nil, workflow) arm — declare once at the workflow, inherit on every
// node, which is the ladder's most common real shape and the one this file's
// own doc defends. Measured as unwitnessed: a mutant returning nil as soon as
// the NODE declared nothing survived ./pkg/dsl/..., ./pkg/backend/model/ and
// ./pkg/queue/ with every package ok, because the sibling tests always pass a
// non-nil node and the (nil, nil) test only covers both-absent.
func TestAWorkflowRungAloneReachesANodeThatDeclaresNothing(t *testing.T) {
	t.Parallel()
	got := ResolveCodemodeBounds(nil, nil, &CodemodeBounds{ToolCalls: 256, Wall: 9 * time.Minute})
	if got == nil {
		t.Fatalf("a node declaring nothing resolved to nil: the workflow's declaration reached nothing")
	}
	if got.ToolCalls != 256 || got.Wall != 9*time.Minute {
		t.Errorf("resolved %+v, want the workflow's 256 calls and 9m", got)
	}
}

// The census ORDER, which the function's own doc promises ("in the order the
// DSL block lists them"). Measured as unwitnessed: reversing the calls
// survived ./pkg/dsl/..., because the sibling test checks set membership by
// prefix and never the sequence. A promise a doc makes is an oracle.
func TestTheCeilingCensusNamesFieldsInTheBlocksOwnOrder(t *testing.T) {
	t.Parallel()
	ceil := scripthost.Ceiling()
	over := CodemodeBoundsAboveCeiling(&CodemodeBounds{
		ToolCalls:           ceil.ToolCalls + 1,
		Wall:                ceil.Wall + time.Second,
		CPU:                 ceil.CPU + time.Second,
		Memory:              ceil.Memory + 1,
		ScriptBytes:         ceil.ScriptBytes + 1,
		ResultBytes:         ceil.ResultBytes + 1,
		ToolResultBytes:     ceil.ToolResultBytes + 1,
		ConcurrentToolCalls: ceil.ConcurrentToolCalls + 1,
	})
	want := []string{"tool_calls", "concurrent_tool_calls", "wall", "cpu", "memory", "script_bytes", "result_bytes", "tool_result_bytes"}
	if len(over) != len(want) {
		t.Fatalf("named %d fields, want %d: %v", len(over), len(want), over)
	}
	for i, w := range want {
		if !strings.HasPrefix(over[i], w+" ") {
			t.Errorf("position %d is %q, want %s — the doc promises the block's own order", i, over[i], w)
		}
	}
}

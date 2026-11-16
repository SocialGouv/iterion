package ir

import (
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/scripthost"
)

// compileCodemode turns one `codemode:` declaration into the two things the
// IR carries: the MODE, which is what the capability cell and the executor
// read, and the BOUNDS, which are what the script host is handed.
//
// They stay two fields rather than one struct on purpose. The mode already
// travels on the wire — schema v31 added it inside `ir_compiled` — and
// `ResolveCodemode` already walks its ladder and `CodemodePolicy` already
// carries its provenance. Folding the bounds into that string would make the
// switch and its limits one value that two unrelated readers narrow. The AST
// folds them (SandboxBlock's shape) because that is the parser's duality to
// read; the IR splits them because that is the wire's and the executor's.
//
// scope and name are the declaration's own ("agent", "a"), the vocabulary
// every other property-value finding in this file uses: they position the
// diagnostics on the `codemode:` line and let `iterion validate --json` name
// the property and close the span, which docs/references/diagnostics.md
// promises for any finding about a property's VALUE.
//
// THE RUNG DECIDES ONE OF THE THREE DIAGNOSTICS. A `mode: off` beside a
// declared bound is C348 at the NODE rung only, and that asymmetry is the
// whole of why the rung is passed in: `codemode:` is two ladders —
// [ResolveCodemode] for the mode, [ResolveCodemodeBounds] for the bounds —
// walked separately. At the workflow rung the bounds are the DEFAULT handed
// to every node that turns codemode on itself, so "off by default, 256 calls
// for whoever enables it" is a coherent declaration and the ordinary shape of
// a bot with one codemode node. Calling those bounds inert was false, and
// both of its remedies changed behaviour: dropping the bounds moved every
// on-node to DefaultLimits — the silent re-making of an operator's choice
// that pkg/queue's v32 rationale calls the correctness bug the additive-field
// rule exists for — and dropping `mode: off` turned codemode on for the whole
// bot. TestTheWorkflowRungsBoundsAreADefaultNotDeadText holds it.
func (c *compiler) compileCodemode(cb *ast.CodemodeBlock, scope, name string) (string, *CodemodeBounds) {
	if cb == nil {
		return "", nil
	}
	mode := normCodemode(cb.Mode)
	// At the node rung an explicit `off` beats any workflow `on`
	// (ResolveCodemode takes the nearest declared value), so no program of
	// this node runs and its own bounds bound nothing.
	inert := mode == "off" && scope != "workflow"
	bounds := c.codemodeBounds(cb, scope, name, !inert)
	if inert && bounds != nil {
		c.warnfAtProperty(DiagCodemodeBoundsWithModeOff, scope, name, "codemode",
			"%s %q declares codemode bounds beside `mode: off`: no program of this node runs, so the bounds bound nothing. Drop `mode: off` to use them, or drop the bounds",
			scope, name)
	}
	return mode, bounds
}

// codemodeBounds reads the seven bound properties, parsing the two durations.
// It returns nil when the block declared none — the block form with only a
// mode is the short form, and a nil bound set is what leaves every field to
// scripthost.DefaultLimits.
//
// A duration is parsed HERE rather than in the parser so a bad one gets a
// diagnostic with a position and the vocabulary of units, instead of a parse
// error that can only say "expected string". A refused duration leaves its
// field unset, so the DEFAULT applies — which is tighter than a bound an
// author RAISED, and looser than one they were LOWERING. C349 is what makes
// either visible; the fallback is not silent, but it is not always tighter,
// and an earlier draft of this comment claimed it was.
//
// reportCeiling is false where the bounds are already known to be inert: a
// C347 saying a value "is clamped where the program is spawned" is text about
// a spawn that will not happen, beside a C348 that says so.
func (c *compiler) codemodeBounds(cb *ast.CodemodeBlock, scope, name string, reportCeiling bool) *CodemodeBounds {
	if !cb.Bounded() {
		return nil
	}
	b := CodemodeBounds{
		ToolCalls:       cb.ToolCalls,
		Memory:          cb.Memory,
		ScriptBytes:     cb.ScriptBytes,
		ResultBytes:     cb.ResultBytes,
		ToolResultBytes: cb.ToolResultBytes,
	}
	// Gated with the census, for the same reason and at the same place: on an
	// inert block a C349 saying "the DEFAULT applies instead" is text about an
	// enforcement that will not happen, beside a C348 saying no program runs.
	// Honouring the rule at one of its two sites was the first draft's defect.
	if reportCeiling {
		b.Wall = c.codemodeDuration(cb.Wall, "wall", scope, name)
		b.CPU = c.codemodeDuration(cb.CPU, "cpu", scope, name)
	} else {
		b.Wall = parseCodemodeDuration(cb.Wall)
		b.CPU = parseCodemodeDuration(cb.CPU)
	}

	if reportCeiling {
		for _, phrase := range CodemodeBoundsAboveCeiling(&b) {
			c.warnfAtProperty(DiagCodemodeBoundAboveCeiling, scope, name, "codemode",
				"%s %q declares a codemode bound above the platform ceiling (%s): it is clamped to the ceiling where the program is spawned, not refused. Lower the value to say what will happen",
				scope, name, phrase)
		}
		// The FLOOR half of the same story (#3290): a memory bound below the
		// measured minimum a Go child works under cannot do real work — the
		// platform raises it where it spawns the program and reports the
		// raise. Warned here for the reason the ceiling is: the author
		// learns at compile, not from a run that came back with more memory
		// than they declared.
		if floor := scripthost.MemoryFloor().Memory; b.Memory > 0 && b.Memory < floor {
			c.warnfAtProperty(DiagCodemodeMemoryBelowFloor, scope, name, "codemode",
				"%s %q declares codemode memory: %d, below the %d-byte floor a Go child works under: the platform raises the bound to the floor where the program is spawned, and allocation past it is refused (typed). Declare %d or more, or drop the line and take the default",
				scope, name, b.Memory, floor, floor)
		}
	}
	return &b
}

// codemodeDuration parses a quoted duration (`"90s"`, `"2m"`), the spelling
// `timeout:` uses. Zero means absent.
// parseCodemodeDuration is codemodeDuration without the diagnostics: the arm
// for a block whose bounds are already known to be inert. The two agree on
// every input — empty, unparseable, negative, overflowing, valid — which is
// what makes the split safe; measured rather than assumed.
func parseCodemodeDuration(raw string) time.Duration {
	if raw == "" {
		return 0
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		return 0
	}
	return d
}

func (c *compiler) codemodeDuration(raw, prop, scope, name string) time.Duration {
	if raw == "" {
		return 0
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		c.warnfAtProperty(DiagCodemodeBoundUnreadable, scope, name, "codemode",
			"%s %q declares codemode %s: %q, which is not a duration (%v): the DEFAULT applies instead (%s), which is NOT the bound you declared — looser if you were lowering it, tighter if you were raising it. Write it the way timeout: is written — \"90s\", \"2m\"",
			scope, name, prop, raw, err, codemodeDurationDefault(prop))
		return 0
	}
	if d < 0 {
		c.warnfAtProperty(DiagCodemodeBoundUnreadable, scope, name, "codemode",
			"%s %q declares codemode %s: %q, a NEGATIVE duration: the DEFAULT applies instead (%s), which is NOT the bound you declared",
			scope, name, prop, raw, codemodeDurationDefault(prop))
		return 0
	}
	return d
}

// codemodeDurationDefault names the default a refused duration falls back to,
// READ from the table that defines it. The first draft hand-copied "wall 30s,
// cpu 10s" into two format strings and a documentation row, and a `cpu:`
// message named the wall's number — three copies of two values with nothing
// tying them to DefaultLimits, in the package that already imports it.
func codemodeDurationDefault(prop string) string {
	def := scripthost.DefaultLimits()
	if prop == "cpu" {
		return "cpu " + def.CPU.String()
	}
	return "wall " + def.Wall.String()
}

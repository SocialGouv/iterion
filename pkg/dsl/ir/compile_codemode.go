package ir

import (
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
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
// A `mode: off` written beside a declared bound is C348: the author said two
// things that cannot both be meant, and the bounds are kept as written so the
// diagnostic can name them instead of a repair hiding them.
func (c *compiler) compileCodemode(cb *ast.CodemodeBlock, what string) (string, *CodemodeBounds) {
	if cb == nil {
		return "", nil
	}
	mode := normCodemode(cb.Mode)
	bounds := c.codemodeBounds(cb, what)
	if mode == "off" && bounds != nil {
		c.warnfAtSpan(DiagCodemodeBoundsWithModeOff, cb.Span,
			"%s declares codemode bounds beside `mode: off`: the bounds are inert, because no program runs. Drop `mode: off` to use them, or drop the bounds", what)
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
// field unset, which falls back to the default: tighter than what the author
// asked for, never looser, and said out loud.
func (c *compiler) codemodeBounds(cb *ast.CodemodeBlock, what string) *CodemodeBounds {
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
	b.Wall = c.codemodeDuration(cb.Wall, "wall", what, cb.Span)
	b.CPU = c.codemodeDuration(cb.CPU, "cpu", what, cb.Span)

	if over := CodemodeBoundsAboveCeiling(&b); len(over) > 0 {
		for _, phrase := range over {
			c.warnfAtSpan(DiagCodemodeBoundAboveCeiling, cb.Span,
				"%s declares a codemode bound above the platform ceiling (%s): it is clamped to the ceiling where the program is spawned, not refused. Lower the value to say what will happen", what, phrase)
		}
	}
	return &b
}

// codemodeDuration parses a quoted duration (`"90s"`, `"2m"`), the spelling
// `timeout:` uses. Zero means absent.
func (c *compiler) codemodeDuration(raw, prop, what string, sp ast.Span) time.Duration {
	if raw == "" {
		return 0
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		c.warnfAtSpan(DiagCodemodeBoundUnreadable, sp,
			"%s declares codemode %s: %q, which is not a duration (%v): the default applies instead. Write it the way timeout: is written — \"90s\", \"2m\"", what, prop, raw, err)
		return 0
	}
	if d < 0 {
		c.warnfAtSpan(DiagCodemodeBoundUnreadable, sp,
			"%s declares codemode %s: %q, a NEGATIVE duration: the default applies instead", what, prop, raw)
		return 0
	}
	return d
}

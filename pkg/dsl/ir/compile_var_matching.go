package ir

import (
	"regexp"
	"regexp/syntax"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)

// ValueMatchesPattern reports whether value satisfies the RE2 pattern a var
// declared with `[matching: "<re>"]`.
//
// It is the ONE reading of a var pattern: the compiler checks a literal
// default and a preset value through it, and the runtime's launch gate
// checks the operator's value through it. Two copies of this decision would
// drift, and the whole point of the constraint is that the value a run
// refuses and the value `iterion validate` refuses are the same value.
//
// The pattern is matched exactly as the author wrote it — Go matches by
// SEARCH, so an unanchored pattern also admits a value that merely contains
// a match. That is the author's choice; C163 warns about it at compile time
// rather than a guard silently anchoring a pattern the author did not.
func ValueMatchesPattern(pattern, value string) (bool, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return false, err
	}
	return re.MatchString(value), nil
}

// compileVarMatching validates a var's `[matching: "<re>"]` constraint and
// returns the pattern to carry on the IR, or "" when the constraint is
// invalid for this var (the diagnostic having been emitted).
//
// Patterns are string-only (C160), for the reason the message states: a
// pattern constrains text, and for a non-string var the declared type is
// already the constraint. `json` and `string[]` are refused for a sharper
// reason — they render differently on the default and the override path, so
// a pattern would certify one rendering while the run uses the other.
func (c *compiler) compileVarMatching(f *ast.VarField) string {
	vt := convertVarType(f.Type)
	if vt != VarString {
		c.errorfAtSpan(DiagVarMatchingNonString, f.Span,
			"var %q: [matching: ...] is only valid on string vars, not %s — for a non-string var the declared type is the constraint",
			f.Name, vt.String())
		return ""
	}
	re, err := syntax.Parse(f.Matching, syntax.Perl)
	if err != nil {
		c.errorfAtSpan(DiagVarMatchingUncompilable, f.Span,
			"var %q: [matching: %q] is not a valid RE2 pattern: %v", f.Name, f.Matching, err)
		return ""
	}
	if !patternIsAnchored(re.Simplify()) {
		c.warnfAtSpan(DiagVarMatchingUnanchored, f.Span,
			"var %q: pattern %q is not anchored, so it also admits a value that merely CONTAINS a match (Go matches by search) — anchor it with ^...$",
			f.Name, f.Matching)
	}
	return f.Matching
}

// patternIsAnchored reports whether every path through re must match the
// whole value.
//
// It reads the PARSED regexp, never the pattern's source text: a check that
// looked for a leading "^" would certify a spelling, and the next author
// writes `\A`, or `(a$|^b)`, or `(?m)^…$` — which looks anchored and is not,
// because under the m flag the anchors become LINE anchors and a newline
// payload passes. Here that case falls out of the structure: `^` parses to
// OpBeginLine instead of OpBeginText and the pattern is reported unanchored.
func patternIsAnchored(re *syntax.Regexp) bool {
	return startsAnchored(re) && endsAnchored(re)
}

// startsAnchored and endsAnchored descend to where the anchors actually
// sit. They are not the same question asked twice on the whole pattern:
// `^(?:a$|b$)` anchors its start once at the top and its end inside each
// branch, and `(^a)($)` splits them across two groups — both match the
// whole value, and a check that demanded the two anchors at the ends of
// ONE concat would warn at them.
func startsAnchored(re *syntax.Regexp) bool {
	switch re.Op {
	case syntax.OpBeginText:
		return true
	case syntax.OpCapture:
		return len(re.Sub) == 1 && startsAnchored(re.Sub[0])
	case syntax.OpAlternate:
		for _, sub := range re.Sub {
			if !startsAnchored(sub) {
				return false
			}
		}
		return len(re.Sub) > 0
	case syntax.OpConcat:
		return len(re.Sub) > 0 && startsAnchored(re.Sub[0])
	default:
		return false
	}
}

func endsAnchored(re *syntax.Regexp) bool {
	switch re.Op {
	case syntax.OpEndText:
		return true
	case syntax.OpCapture:
		return len(re.Sub) == 1 && endsAnchored(re.Sub[0])
	case syntax.OpAlternate:
		for _, sub := range re.Sub {
			if !endsAnchored(sub) {
				return false
			}
		}
		return len(re.Sub) > 0
	case syntax.OpConcat:
		n := len(re.Sub)
		return n > 0 && endsAnchored(re.Sub[n-1])
	default:
		return false
	}
}

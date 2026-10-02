package ir

import (
	"regexp"
	"regexp/syntax"
	"slices"

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

// checkPresetConstraint holds a preset's literal value to the constraints
// its var declares — `[enum: ...]` and `[matching: "<re>"]` — through
// ValueMatchesPattern, the same reading the launch gate uses.
//
// A preset value is a literal written in the `.bot`, as checkable as a
// default and governed by the same constraint. It is not C126/C161 because
// the SITE differs, and the preset family already draws that line: a type
// mismatch on a default is C109, the same mismatch on a preset value is
// C071, because the line to edit and the remedy are not the same. This
// check follows that split rather than inventing a second one.
//
// It WARNS rather than refuses, and that is the whole difference between
// this and C126/C161. A bad default poisons every run; a bad preset value
// is read only by a run that selects that preset, and usually that run is
// refused, precisely, by the launch gate. Only usually: an engine-resolved
// var (reviewtopology's `review_mode`, `mono_family`, `plan_review`,
// `llm_families`) is overwritten between the preset merge and the gate, so
// the value never reaches it — which is the second reason not to state the
// refusal as a fact in the diagnostic's own text. Made an
// error, this blocks a run that selects another preset or none at all, and
// it makes a PAUSED run unresumable once its declaration is tightened,
// which `Engine.Run`'s gate deliberately protects against and docs/dsl.md
// promises (a compile error is not reachable by `resume --force`). C152 is
// the precedent: a `with:` literal that will misbehave at run time on some
// consumers warns at compile time.
//
// Only a value the compiler can read WHOLE is judged. The run expands a
// preset value as it expands a default or a `--var` (docs/dsl.md, "a var's
// text has one reading"), and the compile-time environment is not the
// launch environment — so a value the expander REWRITES is left to the
// gate, which sees it expanded. The question is asked of the expander
// itself (carriesLiveReference), never of the text: `yolo$`, `yolo${` and
// `100$` all carry a `$` the expander never acts on, and a spelling rule
// would have shipped them unchecked.
//
// No type guard: a non-string var cannot carry either constraint
// (C125/C160 zero them), so the "declares no constraint" return below is
// what actually covers those — a second guard on the same condition would
// be dead code dressed as a safety net.
func (c *compiler) checkPresetConstraint(preset string, pv *ast.PresetValue, v *Var, value any) {
	if v == nil || pv == nil {
		return
	}
	if len(v.EnumValues) == 0 && v.Matching == "" {
		return
	}
	s, ok := value.(string)
	if !ok {
		return
	}
	if carriesLiveReference(s) {
		return
	}
	if len(v.EnumValues) > 0 && !slices.Contains(v.EnumValues, s) {
		c.warnfAtSpan(DiagPresetViolatesConstraint, pv.Span,
			"preset %q sets var %q to %q, which is not one of the enum values (%s)",
			preset, pv.Key, s, quoteList(v.EnumValues))
	}
	if v.Matching == "" {
		return
	}
	matched, err := ValueMatchesPattern(v.Matching, s)
	switch {
	case err != nil:
		// Unreachable through a compiled program (C162 refuses the pattern
		// and leaves the IR none), and reported rather than skipped for the
		// reason the launch gate states: a pattern that does not compile
		// must never read as "the value passed".
		c.errorfAtSpan(DiagVarMatchingUncompilable, pv.Span,
			"var %q: declared pattern %q does not compile: %v", pv.Key, v.Matching, err)
	case !matched:
		c.warnfAtSpan(DiagPresetViolatesConstraint, pv.Span,
			"preset %q sets var %q to %q, which does not match its pattern %q",
			preset, pv.Key, s, v.Matching)
	}
}

// carriesLiveReference reports whether the run's expander would REWRITE s —
// the behaviour checkPresetConstraint's skip actually needs.
//
// It asks the expander instead of reading the text, because a `$` is not a
// reference: `yolo$`, `100$`, `yo$-lo` and the forgotten-brace `yolo${` are
// all left exactly as written (expandWithDefault acts on a `$` only when a
// name follows, and an unclosed `${` renders verbatim by construction). A
// rule spelled over the text skipped all four, and a preset value the
// expander never touches is precisely the one the compiler can still judge.
func carriesLiveReference(s string) bool {
	const sentinel = "\x00iterion-live-ref\x00"
	return ExpandWithDefault(s, func(string) string { return sentinel }) != s
}

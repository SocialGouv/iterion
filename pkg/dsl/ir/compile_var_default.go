package ir

import (
	"fmt"
	"slices"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)

// checkConstrainedVarDefault holds a constrained var's default to its
// constraint — `[enum: ...]` (C126) and `[matching: "<re>"]` (C161) by
// ONE reading, because the value of the constraint feature is that the
// author reads one rule. This is the only place a default is checked:
// `resolveVars` seeds defaults from the workflow's vars and the launch
// gate iterates the operator's inputs only, so a default excused here is
// checked on no path at all while its declaration reads as constrained.
//
// A LITERAL default is compared as written, and a violation is an ERROR
// (C126/C161): the compiler reads the whole value, and it is the value
// every run starts with.
//
// A default carrying an env reference is the shape #1610 names: `${...}`
// expands at run start, not at compile time, so comparing the text
// verbatim always lost — `workspace_dir: string [matching: "^/.+$"] =
// "${PROJECT_DIR}"` was impossible while `${PROJECT_DIR}` is the most
// common default shape in the catalogue. The honest semantics: expand
// what has an answer with no environment (the `${VAR:-default}` forms,
// through the same reading resolveVarTextAsWritten gives a published
// contract), then —
//
//   - a live reference REMAINS → C181, a warning: the constraint is
//     UNVERIFIABLE at compile time. Named, never silently excused —
//     silently unchecked is worse than loudly refused — and warned
//     rather than refused, because the value depends on the launch
//     environment, which is the operator's explicit choice;
//   - fully RESOLVED → the compile-time reading is judged, as a WARNING
//     (C182), never C126/C161's error: it is the value only while the
//     launch environment sets nothing, and a refusal would reject a bot
//     that runs clean under the operator's env.
func (c *compiler) checkConstrainedVarDefault(f *ast.VarField, v *Var, s string) {
	if !carriesLiveReference(s) {
		c.checkLiteralVarDefault(f, v, s)
		return
	}
	expanded, err := resolveVarTextAsWritten(s, VarString)
	es, ok := expanded.(string)
	if err != nil || !ok || carriesLiveReference(es) {
		c.warnfAtSpan(DiagVarDefaultUnverifiable, f.Span,
			"var %q default %q carries an environment reference compile time cannot resolve, so its constraint (%s) is checked on NO path — the launch gate reads the operator's values, never a default; make the default literal, or know that the value the launch environment expands it to is unchecked",
			f.Name, s, constraintSummary(v))
		return
	}
	// The compile-time reading: the value every run starts with while the
	// launch environment sets nothing. A warning, never the literal
	// path's error — the operator's env decides otherwise, and no run
	// path re-checks a default.
	if len(v.EnumValues) > 0 && !slices.Contains(v.EnumValues, es) {
		c.warnfAtSpan(DiagVarDefaultExpandedViolates, f.Span,
			"var %q default %q expands (with nothing set) to %q, which is not one of the enum values (%s); the launch environment decides the actual value, and no run path re-checks a default",
			f.Name, s, es, quoteList(v.EnumValues))
	}
	if v.Matching != "" {
		matched, merr := ValueMatchesPattern(v.Matching, es)
		if merr == nil && !matched {
			c.warnfAtSpan(DiagVarDefaultExpandedViolates, f.Span,
				"var %q default %q expands (with nothing set) to %q, which does not match its pattern %q; the launch environment decides the actual value, and no run path re-checks a default",
				f.Name, s, es, v.Matching)
		}
	}
}

// checkLiteralVarDefault is the literal arm, moved out of compileVars
// unchanged: C126/C161, errors, on the text exactly as written.
func (c *compiler) checkLiteralVarDefault(f *ast.VarField, v *Var, s string) {
	if len(v.EnumValues) > 0 && !slices.Contains(v.EnumValues, s) {
		c.errorfAtSpan(DiagVarDefaultNotInEnum, f.Span,
			"var %q default %q is not one of the enum values (%s)", f.Name, s, quoteList(v.EnumValues))
	}
	if v.Matching == "" {
		return
	}
	matched, err := ValueMatchesPattern(v.Matching, s)
	switch {
	case err != nil:
		// Unreachable while C162 and this check agree on what compiles,
		// and reported rather than skipped for the reason the launch
		// gate states: a pattern that does not compile must never read
		// as "the default passed".
		c.errorfAtSpan(DiagVarMatchingUncompilable, f.Span,
			"var %q: declared pattern %q does not compile: %v", f.Name, v.Matching, err)
	case !matched:
		c.errorfAtSpan(DiagVarDefaultNotMatching, f.Span,
			"var %q default %q does not match its own pattern %q", f.Name, s, v.Matching)
	}
}

// constraintSummary names the constraint(s) a var carries, for the C181
// message that must say WHAT cannot be verified.
func constraintSummary(v *Var) string {
	var parts []string
	if len(v.EnumValues) > 0 {
		parts = append(parts, "enum: "+quoteList(v.EnumValues))
	}
	if v.Matching != "" {
		parts = append(parts, fmt.Sprintf("matching: %q", v.Matching))
	}
	return strings.Join(parts, ", ")
}

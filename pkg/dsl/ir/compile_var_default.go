package ir

import (
	"fmt"
	"slices"
	"strings"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)

// engineVarSentinel is what the compile-time lookup answers for an
// engine-supplied name (EngineSuppliedVarNames, fallback_apply.go — the
// names the RUN's expander answers itself, pinned against varExpandFn by
// a pkg/runtime test): never "", so a `${PROJECT_DIR:-fallback}` form
// never reads its fallback, and a marker the still-live check recognises
// (it contains no `$`, so carriesLiveReference alone would miss it). A
// compile-time reading that resolved one of them against an empty
// environment would certify a value no run ever starts with (#1610's
// HIGH review finding, executed both directions):
// `${PROJECT_SCRATCH_DIR:-relative}` read as "relative" false-positive
// C182 on a var every run satisfies, and `${PROJECT_SCRATCH_DIR:-/tmp/x}`
// read as "/tmp/x" compiled SILENT on a var every run violates.
const engineVarSentinel = "\x00iterion-engine-var\x00"

// checkConstrainedVarDefault holds a constrained var's default to its
// constraint — `[enum: ...]` (C126) and `[matching: "<re>"]` (C161) by
// ONE reading, because the value of the constraint feature is that the
// author reads one rule. This is the only place a default is checked:
// `resolveVars` seeds defaults from the workflow's vars and the launch
// gate iterates the operator's inputs only, so a default excused here is
// checked on no path at all while its declaration reads as constrained.
//
// A default the compiler can read WHOLE — the text as written, when it
// carries no reference the expander acts on (a `$` alone is not a
// reference) — is judged with the literal path's verdicts (C126/C161,
// errors): the value it will start with is known, and it violates.
//
// Every default carrying a reference is expanded through the compile-time
// reading (the `${VAR:-default}` forms resolve, the engine-supplied names
// answer a sentinel, anything else stays as written) — and only that
// reading: no reference is environment-independent. Every rewrite the
// run's expander makes consults its lookup, and that lookup is the
// process environment with no name filter (varExpandFn ends in
// os.Getenv), so `${VAR:+alt}` and `${VAR-default}` — the expander has no
// `:+` / `-` operators — are lookups of a variable literally named
// `VAR:+alt`, which a launch can set, and a computed name (`${${SEL}}`,
// `${PRE${X}}`) resolves inside-out to whatever the environment makes
// of it. Then:
//
//   - a live reference REMAINS (an unresolvable name, or an
//     engine-supplied one) → C181, a warning: the constraint is
//     UNVERIFIABLE at compile time. Named, never silently excused —
//     silently unchecked is worse than loudly refused — and warned
//     rather than refused, because the value depends on the launch
//     (the operator's environment, or the engine's own answer), which
//     no compile verdict can speak for;
//   - fully RESOLVED → the compile-time reading is judged, as a WARNING
//     (C182), never C126/C161's error: it is the value only while the
//     launch environment sets nothing, and a refusal would reject a bot
//     that runs clean under the operator's env.
func (c *compiler) checkConstrainedVarDefault(f *ast.VarField, v *Var, s string) {
	if !carriesLiveReference(s) {
		c.checkLiteralVarDefault(f, v, s)
		return
	}
	expanded, err := compileTimeDefaultReading(s)
	es, ok := expanded.(string)
	if err == nil && ok && !strings.Contains(es, engineVarSentinel) && !carriesLiveReference(es) {
		// Fully resolved by the compile-time reading — but only because
		// every reference carried a `:-default`. The launch environment
		// still decides: warn (C182), never the literal path's error.
		c.checkExpandedVarDefault(f, v, s, es)
		return
	}
	c.warnfAtSpan(DiagVarDefaultUnverifiable, f.Span,
		"var %q default %q carries a reference compile time cannot resolve — an environment variable, or an engine-supplied name (%s) the RUN answers itself — so its constraint (%s) is checked on NO path: the launch gate reads the operator's values, never a default; make the default literal, or know that the value it expands to at launch is unchecked",
		f.Name, s, strings.Join(EngineSuppliedVarNames, ", "), constraintSummary(v))
}

// compileTimeDefaultReading is the compile-time reading of a constrained
// var's default: the run's own reading of a `string` var's text
// (resolveVarText with the full expander) over compileTimeVarLookup,
// keeping what it cannot resolve as written so a live reference stays
// visible to the caller.
func compileTimeDefaultReading(s string) (any, error) {
	return resolveVarText(s, VarString, varExpander(VarString, compileTimeVarLookup, true))
}

// compileTimeVarLookup is the environment the compile-time reading of a
// var default consults: nothing, except a sentinel for the
// engine-supplied names — the run answers those itself, so no reading
// taken here may resolve them.
func compileTimeVarLookup(key string) string {
	if slices.Contains(EngineSuppliedVarNames, key) {
		return engineVarSentinel
	}
	return ""
}

// checkExpandedVarDefault is the C182 arm: the default's references all
// carried `:-default` forms, so the compile-time reading IS the value a
// run starts with while the launch environment sets nothing — no
// engine-supplied name can be in it (those route to C181). Judged as a
// warning, never the literal path's error: the launch environment
// decides the actual value, and a refusal would reject a bot that runs
// clean under the operator's env.
func (c *compiler) checkExpandedVarDefault(f *ast.VarField, v *Var, written, reading string) {
	if len(v.EnumValues) > 0 && !slices.Contains(v.EnumValues, reading) {
		c.warnfAtSpan(DiagVarDefaultExpandedViolates, f.Span,
			"var %q default %q expands (with nothing set in the launch environment) to %q, which is not one of the enum values (%s) — the value a bare launch starts with; the launch environment decides the actual value, and no run path re-checks a default",
			f.Name, written, reading, quoteList(v.EnumValues))
	}
	if v.Matching == "" {
		return
	}
	matched, err := ValueMatchesPattern(v.Matching, reading)
	if err == nil && !matched {
		c.warnfAtSpan(DiagVarDefaultExpandedViolates, f.Span,
			"var %q default %q expands (with nothing set in the launch environment) to %q, which does not match its pattern %q — the value a bare launch starts with; the launch environment decides the actual value, and no run path re-checks a default",
			f.Name, written, reading, v.Matching)
	}
}

// checkLiteralVarDefault is the literal arm: C126/C161, errors, on a
// default the compiler reads whole — the text exactly as written, which
// carries no reference the expander acts on.
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

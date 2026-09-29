package ir

import (
	"os"
	"strings"
	"testing"
)

// TestC181_C182_ConstrainedVarDefaultWithEnvReference is the class table
// of #1610: a constrained var's default carrying an env reference was a
// FALSE C126/C161 error, because `${...}` expands at run start and the
// compiler compared the text verbatim. The honest semantics: expand what
// has an answer with no environment (the `${VAR:-default}` forms), then
// name what stays unverifiable (C181) or violates the compile-time
// reading (C182) — warnings, never refusals, because the launch
// environment is the operator's explicit choice. Enum and matching read
// the same rule: the value of the constraint feature is that the author
// reads one.
func TestC181_C182_ConstrainedVarDefaultWithEnvReference(t *testing.T) {
	cases := []struct {
		name     string
		varsLine string
		code     DiagCode // "" = neither C181 nor C182 (and no C126/C161)
		want     int
	}{
		// The ticket's own probes, enum arm.
		{"probe: an unresolvable reference is unverifiable, enum",
			`  mode: string [enum: "a", "b"] = "${SOMEVAR}"`, DiagVarDefaultUnverifiable, 1},
		{"probe: a :-default the compiler can judge, and it satisfies, enum",
			`  mode: string [enum: "a", "b"] = "${SOMEVAR:-a}"`, "", 0},
		{"probe: a :-default the compiler can judge, and it violates, enum",
			`  mode: string [enum: "a", "b"] = "${SOMEVAR:-c}"`, DiagVarDefaultExpandedViolates, 1},
		{"probe: a literal default still passes clean",
			`  mode: string [enum: "a", "b"] = "a"`, "", 0},
		// The matching arm reads the same rule.
		{"the ticket's motivating shape is now possible, matching",
			`  workspace_dir: string [matching: "^/.+$"] = "${PROJECT_DIR}"`, DiagVarDefaultUnverifiable, 1},
		{"a :-default that satisfies the pattern stays silent, matching",
			`  workspace_dir: string [matching: "^/.+$"] = "${WORKSPACE_DIR:-/tmp}"`, "", 0},
		{"a :-default that violates the pattern warns, matching",
			`  workspace_dir: string [matching: "^/.+$"] = "${WORKSPACE_DIR:-relative}"`, DiagVarDefaultExpandedViolates, 1},
		// Shapes around the rule.
		{"a bare unbraced reference is as unresolvable as the braced one",
			`  mode: string [enum: "a", "b"] = "$SOMEVAR"`, DiagVarDefaultUnverifiable, 1},
		{"one resolvable and one unresolvable reference: still unverifiable",
			`  mode: string [enum: "a", "b"] = "${A:-a}${B}"`, DiagVarDefaultUnverifiable, 1},
		// The HIGH review finding, both directions the reviewer executed:
		// the RUN answers the engine-supplied names itself, so a
		// compile-time reading must never resolve them — the `:-` form
		// does not fire, and the default is unverifiable (C181).
		{"engine-owned name, fallback violates: NOT a C182 false positive",
			`  scratch: string [matching: "^/.+$"] = "${PROJECT_SCRATCH_DIR:-relative}"`, DiagVarDefaultUnverifiable, 1},
		{"engine-owned name, fallback satisfies: NOT a false silence either",
			`  scratch: string [matching: "^/tmp.*$"] = "${PROJECT_SCRATCH_DIR:-/tmp/x}"`, DiagVarDefaultUnverifiable, 1},
		{"engine-owned name, no fallback: unverifiable all the same",
			`  scratch: string [matching: "^/.+$"] = "${PROJECT_SCRATCH_DIR}"`, DiagVarDefaultUnverifiable, 1},
		{"every engine-owned name is treated as run-answered",
			`  scratch: string [matching: "^/.+$"] = "${PROJECT_DIR:-x}${PROJECT_MEMORY_DIR:-y}${BUNDLE_DIR:-z}${BUNDLE_SKILLS_DIR:-w}"`, DiagVarDefaultUnverifiable, 1},
		// No reference is environment-independent: the run's lookup is the
		// process environment with no name filter, and a nested segment's
		// answer is the next lookup's KEY. The unsupported-operator forms
		// look up a variable literally named `X:+a`, a computed name is
		// whatever the environment makes of it — C181, never the literal
		// path's "always expands to" error (both executed in
		// TestLookupDependentFormsAreUnverifiable).
		{"${X:+alt} looks up a variable named X:+alt — unverifiable, not C126",
			`  mode: string [enum: "a", "b"] = "${X:+a}"`, DiagVarDefaultUnverifiable, 1},
		{"${X-default} looks up a variable named X-default — unverifiable, not C161",
			`  agent: string [matching: "^[a-z]+$"] = "${X-default}"`, DiagVarDefaultUnverifiable, 1},
		{"unsupported-operator forms the constraint would admit as empty are unverifiable all the same",
			`  agent: string [matching: "^.*$"] = "${X:+ignored}${Y-also-ignored}"`, DiagVarDefaultUnverifiable, 1},
		{"a computed name resolves at launch — unverifiable, not C126",
			`  mode: string [enum: "a", "b"] = "${${SELECTOR}}"`, DiagVarDefaultUnverifiable, 1},
		{"a name computed from a prefix and a reference — unverifiable, not C126",
			`  mode: string [enum: "a", "b"] = "${PRE${X}}"`, DiagVarDefaultUnverifiable, 1},
		{"a name computed from a bare reference — unverifiable, not C126",
			`  mode: string [enum: "a", "b"] = "${$X}"`, DiagVarDefaultUnverifiable, 1},
		{"a nested :-default chain resolves with nothing set — judged, and it violates",
			`  mode: string [enum: "a", "b"] = "${${X:-A}:-c}"`, DiagVarDefaultExpandedViolates, 1},
		{"a {{vars.x}} default is NOT an env reference — the literal-text rule stands",
			`  mode: string [enum: "a", "b"] = "{{vars.other}}"`, "", 0}, // → C126, asserted below
		{"a trailing dollar is not a reference — the literal-text rule stands",
			`  mode: string [enum: "a", "b"] = "c$"`, "", 0}, // → C126, asserted below
		{"an unconstrained var with an env default draws nothing",
			`  free: string = "${SOMEVAR}"`, "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := compileFile(t, varDefaultSrc(tc.varsLine))
			for _, code := range []DiagCode{DiagVarDefaultUnverifiable, DiagVarDefaultExpandedViolates} {
				want := 0
				if code == tc.code {
					want = tc.want
				}
				if got := countCode(r, code); got != want {
					t.Errorf("%s count = %d, want %d\ndiagnostics: %v", code, got, want, r.Diagnostics)
				}
			}
			// The false error is gone in every one of these shapes: no
			// C126/C161 may ride along — except where the case IS the
			// literal rule, asserted separately below.
			if tc.code == "" {
				return
			}
			for _, code := range []DiagCode{DiagVarDefaultNotInEnum, DiagVarDefaultNotMatching} {
				if got := countCode(r, code); got != 0 {
					t.Errorf("%s count = %d, want 0 — the verbatim comparison of an env reference was the false error\ndiagnostics: %v",
						code, got, r.Diagnostics)
				}
			}
		})
	}
}

// TestC181_C182_AreWarnings pins the severities: both diagnostics warn.
// An error would refuse a bot whose launch environment makes the default
// legal — the operator's env is an explicit choice — and would strand a
// paused run whose declaration gained a reference-carrying default.
func TestC181_C182_AreWarnings(t *testing.T) {
	cases := []struct {
		varsLine string
		code     DiagCode
	}{
		{`  mode: string [enum: "a", "b"] = "${SOMEVAR}"`, DiagVarDefaultUnverifiable},
		{`  mode: string [enum: "a", "b"] = "${SOMEVAR:-c}"`, DiagVarDefaultExpandedViolates},
	}
	for _, tc := range cases {
		r := compileFile(t, varDefaultSrc(tc.varsLine))
		found := false
		for _, d := range r.Diagnostics {
			if d.Code == tc.code {
				found = true
				if d.Severity != SeverityWarning {
					t.Errorf("%s severity = %s, want warning", tc.code, d.Severity)
				}
			}
		}
		if !found {
			t.Errorf("expected a %s diagnostic, got %v", tc.code, r.Diagnostics)
		}
		if r.HasErrors() {
			t.Errorf("%s made the program fail to compile: %v", tc.code, r.Diagnostics)
		}
	}
}

// TestC181_MessageNamesTheConstraint: the warning's whole reason to exist
// is honesty — it must say WHICH constraint is unchecked, and why (the
// launch gate never reads a default), not merely that something is wrong.
func TestC181_MessageNamesTheConstraint(t *testing.T) {
	r := compileFile(t, varDefaultSrc(`  workspace_dir: string [matching: "^/.+$"] = "${PROJECT_DIR}"`))
	var msg string
	for _, d := range r.Diagnostics {
		if d.Code == DiagVarDefaultUnverifiable {
			msg = d.Message
		}
	}
	if msg == "" {
		t.Fatalf("no C181 raised\ndiagnostics: %v", r.Diagnostics)
	}
	for _, want := range []string{`var "workspace_dir"`, `"${PROJECT_DIR}"`, `matching: "^/.+$"`, "NO path"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not carry %q", msg, want)
		}
	}
}

// TestLiteralDefaultsWithDollarsStayErrors: the literal arm did not move.
// A `$` is not a reference (the expander never acts on `c$`), so the
// compiler still reads the whole value — and a violation is the error it
// always was. `{{vars.other}}` is not an env form at all: the runtime
// keeps it as written, so the literal text IS the value, and C126 stays.
func TestLiteralDefaultsWithDollarsStayErrors(t *testing.T) {
	r := compileFile(t, varDefaultSrc(`  mode: string [enum: "a", "b"] = "c$"`))
	if got := countCode(r, DiagVarDefaultNotInEnum); got != 1 {
		t.Errorf("C126 count = %d, want 1 — a trailing dollar is not a reference\ndiagnostics: %v", got, r.Diagnostics)
	}
	r = compileFile(t, varDefaultSrc(`  mode: string [enum: "a", "b"] = "{{vars.other}}"`))
	if got := countCode(r, DiagVarDefaultNotInEnum); got != 1 {
		t.Errorf("C126 count = %d, want 1 — {{vars.x}} is not expanded in a var default, the text is the value\ndiagnostics: %v", got, r.Diagnostics)
	}
	r = compileFile(t, varDefaultSrc(`  agent: string [matching: "^[a-z]+$"] = "Code X$"`))
	if got := countCode(r, DiagVarDefaultNotMatching); got != 1 {
		t.Errorf("C161 count = %d, want 1\ndiagnostics: %v", got, r.Diagnostics)
	}
}

// TestLookupDependentFormsAreUnverifiable: a default whose reading any
// launch can move is C181, never the literal path's error. Two shapes
// once read as "always the empty string" and were refused — both false,
// executed here through the run's own reading:
//
//   - `${X:+alt}` / `${X-default}`: the expander has no `:+` / `-`
//     operators, so the segment is looked up WHOLE as a name, and the
//     run's lookup ends in a bare os.Getenv (varExpandFn,
//     pkg/runtime/engine_resolve.go) — a launch environment carrying a
//     variable named `X:+a` answers it;
//   - a computed name (`${${SELECTOR}}`, `${PRE${X}}`, `${$X}`): the
//     segments resolve inside-out, so one lookup's answer is the next
//     lookup's KEY.
func TestLookupDependentFormsAreUnverifiable(t *testing.T) {
	for _, line := range []string{
		`  mode: string [enum: "a", "b"] = "${X:+a}"`,
		`  agent: string [matching: "^[a-z]+$"] = "${X-default}"`,
		`  mode: string [enum: "a", "b"] = "${${SELECTOR}}"`,
	} {
		r := compileFile(t, varDefaultSrc(line))
		if got := countCode(r, DiagVarDefaultUnverifiable); got != 1 {
			t.Errorf("%s: C181 count = %d, want 1\ndiagnostics: %v", line, got, r.Diagnostics)
		}
		for _, code := range []DiagCode{DiagVarDefaultNotInEnum, DiagVarDefaultNotMatching} {
			if got := countCode(r, code); got != 0 {
				t.Errorf("%s: %s count = %d, want 0 — the run's reading depends on the launch environment\ndiagnostics: %v", line, code, got, r.Diagnostics)
			}
		}
		if r.HasErrors() {
			t.Errorf("%s: the program fails to compile: %v", line, r.Diagnostics)
		}
	}

	// The witnesses: the run's reading of each form moves with the
	// environment, to a value the constraint admits.
	t.Setenv("X:+a", "b")
	if v, err := ResolveVarText("${X:+a}", VarString, os.Getenv); err != nil || v != "b" {
		t.Errorf("the run reads ${X:+a} as %#v (err %v) under X:+a=b, want \"b\" — the fixture is inert", v, err)
	}
	env := map[string]string{"SELECTOR": "TARGET", "TARGET": "a", "X": "FIX", "PREFIX": "a", "T": "a"}
	lookup := func(k string) string { return env[k] }
	for _, def := range []string{"${${SELECTOR}}", "${PRE${X}}"} {
		if v, err := ResolveVarText(def, VarString, lookup); err != nil || v != "a" {
			t.Errorf("the run reads %s as %#v (err %v), want \"a\" — the fixture is inert", def, v, err)
		}
	}
	env["X"] = "T"
	if v, err := ResolveVarText("${$X}", VarString, lookup); err != nil || v != "a" {
		t.Errorf("the run reads ${$X} as %#v (err %v), want \"a\" — the fixture is inert", v, err)
	}
}

// TestConstrainedVarDefaultExpansionAgreesWithTheRun is the property the
// C182 judgement rests on: what the compiler expands a `${VAR:-default}`
// default to with no environment IS what the run starts with when the
// launch environment sets nothing — both go through the same expander.
// If the two readings drifted, C182 would judge a value no run uses.
func TestConstrainedVarDefaultExpansionAgreesWithTheRun(t *testing.T) {
	const def = "${SOMEVAR:-a}"
	compileReading, err := resolveVarTextAsWritten(def, VarString)
	if err != nil {
		t.Fatalf("resolveVarTextAsWritten: %v", err)
	}
	runReading, err := ResolveVarText(def, VarString, func(string) string { return "" })
	if err != nil {
		t.Fatalf("ResolveVarText: %v", err)
	}
	if compileReading != runReading {
		t.Fatalf("compile-time reading %v ≠ the run's empty-env reading %v", compileReading, runReading)
	}
	if compileReading != "a" {
		t.Fatalf("fixture is inert: reading = %v, want \"a\"", compileReading)
	}
}

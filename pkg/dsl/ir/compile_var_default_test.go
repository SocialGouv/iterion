package ir

import (
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
			`  workspace_dir: string [matching: "^/.+$"] = "${PROJECT_DIR:-/tmp}"`, "", 0},
		{"a :-default that violates the pattern warns, matching",
			`  workspace_dir: string [matching: "^/.+$"] = "${PROJECT_DIR:-relative}"`, DiagVarDefaultExpandedViolates, 1},
		// Shapes around the rule.
		{"a bare unbraced reference is as unresolvable as the braced one",
			`  mode: string [enum: "a", "b"] = "$SOMEVAR"`, DiagVarDefaultUnverifiable, 1},
		{"one resolvable and one unresolvable reference: still unverifiable",
			`  mode: string [enum: "a", "b"] = "${A:-a}${B}"`, DiagVarDefaultUnverifiable, 1},
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
			if strings.HasPrefix(tc.name, "a {{vars.x}}") || strings.HasPrefix(tc.name, "a trailing dollar") {
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

package ir

import "testing"

// TestC125_VarEnumNonString verifies an [enum: ...] constraint on a
// non-string var type is rejected: enums constrain string values only.
func TestC125_VarEnumNonString(t *testing.T) {
	cases := []struct {
		varsLine string
		want     int
	}{
		{`  count: int [enum: "a", "b"]`, 1},
		{`  flag: bool [enum: "a"]`, 1},
		{`  ratio: float [enum: "a"]`, 1},
		{`  blob: json [enum: "a"]`, 1},
		{`  tags: string[] [enum: "a"]`, 1},
		{`  mode: string [enum: "a", "b"]`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.varsLine, func(t *testing.T) {
			r := compileFile(t, varDefaultSrc(tc.varsLine))
			if got := countCode(r, DiagVarEnumNonString); got != tc.want {
				t.Errorf("C125 count = %d, want %d\ndiagnostics: %v", got, tc.want, r.Diagnostics)
			}
		})
	}
}

// TestC126_VarDefaultNotInEnum verifies a default outside the enum set is
// an error, and that a wrong-TYPE default stays C109 territory (no C126
// double-flag).
func TestC126_VarDefaultNotInEnum(t *testing.T) {
	cases := []struct {
		varsLine string
		want     int
	}{
		{`  mode: string [enum: "autonomous", "interview"] = "yolo"`, 1},
		{`  mode: string [enum: "autonomous", "interview"] = "autonomous"`, 0},
		{`  mode: string [enum: "autonomous", "interview"]`, 0}, // no default, nothing to check
		{`  mode: string [enum: "a", "b"] = 5`, 0},              // wrong type → C109, not C126
	}
	for _, tc := range cases {
		t.Run(tc.varsLine, func(t *testing.T) {
			r := compileFile(t, varDefaultSrc(tc.varsLine))
			if got := countCode(r, DiagVarDefaultNotInEnum); got != tc.want {
				t.Errorf("C126 count = %d, want %d\ndiagnostics: %v", got, tc.want, r.Diagnostics)
			}
		})
	}
	// The wrong-type case still surfaces as C109.
	r := compileFile(t, varDefaultSrc(`  mode: string [enum: "a", "b"] = 5`))
	if got := countCode(r, DiagVarDefaultTypeMismatch); got != 1 {
		t.Errorf("C109 count = %d, want 1 for a non-string default on a string enum var", got)
	}
}

// TestC127_VarEnumDuplicate verifies duplicate enum values warn and are
// deduplicated in the compiled IR (first occurrence kept, order preserved).
func TestC127_VarEnumDuplicate(t *testing.T) {
	r := compileFile(t, varDefaultSrc(`  mode: string [enum: "a", "b", "a"] = "b"`))
	if got := countCode(r, DiagVarEnumDuplicate); got != 1 {
		t.Fatalf("C127 count = %d, want 1\ndiagnostics: %v", got, r.Diagnostics)
	}
	v := r.Workflow.Vars["mode"]
	if v == nil {
		t.Fatal("var mode missing from compiled workflow")
	}
	if len(v.EnumValues) != 2 || v.EnumValues[0] != "a" || v.EnumValues[1] != "b" {
		t.Errorf("EnumValues = %v, want deduplicated [a b]", v.EnumValues)
	}
}

// TestVarEnumSeverities pins C125/C126 as errors and C127 as a warning.
func TestVarEnumSeverities(t *testing.T) {
	cases := []struct {
		varsLine string
		code     DiagCode
		want     Severity
	}{
		{`  count: int [enum: "a"]`, DiagVarEnumNonString, SeverityError},
		{`  mode: string [enum: "a", "b"] = "c"`, DiagVarDefaultNotInEnum, SeverityError},
		{`  mode: string [enum: "a", "a"] = "a"`, DiagVarEnumDuplicate, SeverityWarning},
	}
	for _, tc := range cases {
		t.Run(string(tc.code), func(t *testing.T) {
			r := compileFile(t, varDefaultSrc(tc.varsLine))
			found := false
			for _, d := range r.Diagnostics {
				if d.Code == tc.code {
					found = true
					if d.Severity != tc.want {
						t.Errorf("%s severity = %s, want %s", tc.code, d.Severity, tc.want)
					}
				}
			}
			if !found {
				t.Fatalf("expected a %s diagnostic, got %v", tc.code, r.Diagnostics)
			}
		})
	}
}

// TestVarEnumCarriedIntoIR verifies a clean declaration lands on the
// compiled ir.Var so downstream consumers (runtime launch gate, studio
// schema surface) see the constraint.
func TestVarEnumCarriedIntoIR(t *testing.T) {
	r := compileFile(t, varDefaultSrc(`  mode: string [enum: "autonomous", "interview"] = "autonomous"`))
	for _, code := range []DiagCode{DiagVarEnumNonString, DiagVarDefaultNotInEnum, DiagVarEnumDuplicate} {
		if got := countCode(r, code); got != 0 {
			t.Errorf("%s count = %d, want 0", code, got)
		}
	}
	v := r.Workflow.Vars["mode"]
	if v == nil {
		t.Fatal("var mode missing from compiled workflow")
	}
	if len(v.EnumValues) != 2 || v.EnumValues[0] != "autonomous" || v.EnumValues[1] != "interview" {
		t.Errorf("EnumValues = %v, want [autonomous interview]", v.EnumValues)
	}
	if v.Default != "autonomous" {
		t.Errorf("Default = %v, want autonomous", v.Default)
	}
}

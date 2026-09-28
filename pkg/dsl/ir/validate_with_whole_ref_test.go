package ir

import (
	"strings"
	"testing"
)

// c180Fixture is the probe of #1604, minimally completed so it compiles:
// a list-typed var delivered WHOLE — the mapping is exactly one reference
// — into a `string` input field of the destination. `varLine` and
// `fieldType` vary per case; `mapping` is spliced in as raw `.bot`
// source, quotes included.
func c180Fixture(varLine, fieldType, mapping string) string {
	return `dsl: 2

vars:
` + varLine + `

schema sink:
  label: ` + fieldType + `

emit go:
  event: "go"

tool show:
  input: sink
  command: ` + "`printf 'label=<%s>\\n' {{input.label}}`" + `

workflow w:
  worktree: none
  sandbox: none
  entry: go
  go -> show with { label: ` + mapping + ` }
  show -> done
`
}

// TestC180_WholeListRefIntoStringField is the class table: one row per
// shape a whole-value `with:` mapping can take onto a `string` field.
// The `string[]` and `json` vars fire — since #1285 the var IS a list on
// the default path too, so the list arrives whole and nothing checks it
// at run time; a scalar var, an outputs reference and every text-arriving
// mapping stay silent (the last two are C152's reading of the same edge,
// and a `string` field is the one a string can satisfy).
func TestC180_WholeListRefIntoStringField(t *testing.T) {
	cases := []struct {
		name      string
		varLine   string
		fieldType string
		mapping   string
		fires     bool
	}{
		{"the ticket's probe: a string[] var whole into a string field",
			`  b: string[] = "claw,claude_code"`, "string", `"{{vars.b}}"`, true},
		{"a json var whole into a string field",
			`  cfg: json = "{\"a\": 1}"`, "string", `"{{vars.cfg}}"`, true},
		{"a string var whole into a string field",
			`  b: string = "claw"`, "string", `"{{vars.b}}"`, false},
		{"a bool var whole into a string field is another divergence, not this one",
			`  flag: bool = true`, "string", `"{{vars.flag}}"`, false},
		{"a string[] var whole into a string[] field is the honest binding",
			`  b: string[] = "claw,claude_code"`, "string[]", `"{{vars.b}}"`, false},
		{"a string[] var whole into a json field is the honest binding",
			`  b: string[] = "claw,claude_code"`, "json", `"{{vars.b}}"`, false},
		{"an outputs reference carries no statically-known list type here",
			`  b: string[] = "claw,claude_code"`, "string", `"{{outputs.go}}"`, false},
		{"an undeclared var is C033's, not C180's",
			`  b: string[] = "claw,claude_code"`, "string", `"{{vars.nope}}"`, false},
		{"interpolated into prose the value arrives as text, and a string field takes a string",
			`  b: string[] = "claw,claude_code"`, "string", `"{{vars.b}} "`, false},
		{"a ref-less literal arrives as text, and a string field takes a string",
			`  b: string[] = "claw,claude_code"`, "string", `"claw"`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := compileText(t, c180Fixture(tc.varLine, tc.fieldType, tc.mapping))
			fired := hasDiagAtSeverity(r, "C180", SeverityWarning)
			if fired != tc.fires {
				t.Errorf("C180 fired=%v, want %v:\n%v", fired, tc.fires, r.Diagnostics)
			}
			if tc.fires {
				assertNoC180Error(t, r)
			}
		})
	}
}

// assertNoC180Error pins the severity: a warning, like C152 — the shape
// runs today on tolerant consumers, and a refusal would reject a bot a
// run survives.
func assertNoC180Error(t *testing.T, r *CompileResult) {
	t.Helper()
	for _, d := range r.Diagnostics {
		if d.Code == DiagWithWholeRefListToString && d.Severity != SeverityWarning {
			t.Errorf("C180 severity = %v, want warning", d.Severity)
		}
	}
}

// TestC180_MessageNamesTheVarAndTheRemedy: the author has to find one
// line in a file of them — the diagnostic names the edge, the mapping
// key, the var and its declared type, and says where the list lands and
// what to declare instead.
func TestC180_MessageNamesTheVarAndTheRemedy(t *testing.T) {
	r := compileText(t, c180Fixture(`  b: string[] = "claw,claude_code"`, "string", `"{{vars.b}}"`))
	var msg string
	for _, d := range r.Diagnostics {
		if d.Code == DiagWithWholeRefListToString {
			msg = d.Message
		}
	}
	if msg == "" {
		t.Fatalf("no C180 raised\ndiagnostics: %v", r.Diagnostics)
	}
	for _, want := range []string{
		`edge go -> show, with "label"`, "`string[]` var \"b\"",
		`field "label" of input schema "sink"`, "declare the field `string[]` or `json`",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not carry %q", msg, want)
		}
	}
}

// TestC180_FiresOncePerMapping: one mapping, one diagnostic — the check
// must not also fire on the arrival-as-text reading of the same edge
// (C152 stays silent on a `string` field by design).
func TestC180_FiresOncePerMapping(t *testing.T) {
	r := compileText(t, c180Fixture(`  b: string[] = "claw,claude_code"`, "string", `"{{vars.b}}"`))
	n := 0
	for _, d := range r.Diagnostics {
		if d.Code == DiagWithWholeRefListToString {
			n++
		}
		if d.Code == DiagWithLiteralTypeMismatch {
			t.Errorf("C152 fired alongside C180 on a whole-reference mapping: %s", d.Message)
		}
	}
	if n != 1 {
		t.Fatalf("C180 count = %d, want exactly 1\ndiagnostics: %v", n, r.Diagnostics)
	}
}

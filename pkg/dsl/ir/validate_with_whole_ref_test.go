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
// A `string[]` var fires — since #1285 it IS a list on the default path
// too, so the list arrives whole and nothing checks it at run time. A
// `json` var fires on what its default DOCUMENT is (the run parses it
// before expanding anything, so the text decides): a list or an object
// fires, a scalar or a null document stays silent (R9a800e — a string
// arrives, which is what the field declares), and no default fires on
// what the launch may supply. A scalar var, an outputs reference and
// every text-arriving mapping stay silent (the last two are C152's
// reading of the same edge, and a `string` field is the one a string can
// satisfy).
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
		{"a json var whose default document is an object, whole into a string field",
			`  cfg: json = "{\"a\": 1}"`, "string", `"{{vars.cfg}}"`, true},
		{"a json var whose default document is a list of scalars",
			`  cfg: json = "[\"a\", \"b\"]"`, "string", `"{{vars.cfg}}"`, true},
		{"a json var whose default document is a list holding an object",
			`  cfg: json = "[{\"a\": 1}]"`, "string", `"{{vars.cfg}}"`, true},
		{"a json var with no default: the launch supplies the document",
			`  cfg: json`, "string", `"{{vars.cfg}}"`, true},
		{"a json var whose default document is a string scalar arrives as a string",
			`  cfg: json = "\"claw\""`, "string", `"{{vars.cfg}}"`, false},
		{"a json var whose default document is a number is a scalar",
			`  cfg: json = "3"`, "string", `"{{vars.cfg}}"`, false},
		{"a json var whose default is a non-JSON word arrives as that word",
			`  cfg: json = "claw"`, "string", `"{{vars.cfg}}"`, false},
		{"a json var whose default is a ${...} reference arrives as the string it expands to",
			`  cfg: json = "${DOC}"`, "string", `"{{vars.cfg}}"`, false},
		{"a json var whose default document is null delivers no list",
			`  cfg: json = "null"`, "string", `"{{vars.cfg}}"`, false},
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
	// The named mechanism is the one the reviewer EXECUTED (the MEDIUM
	// finding): one node execution, the list spread into the command's
	// argv — never a per-element execution, which does not exist
	// anywhere in the engine.
	if !strings.Contains(msg, "spread into its argv") {
		t.Errorf("message %q does not name the argv-spread mechanism", msg)
	}
	if strings.Contains(msg, "once per element") {
		t.Errorf("message %q names a per-element execution that does not exist", msg)
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

// TestC180_MessageMatchesTheDocumentShape: the mechanism the message
// names is the one shellEscapeValue applies to THAT shape. A list of
// scalars is spread into argv; an object — or a list holding a
// collection — is ONE JSON token, so a message claiming the spread for
// it would be false (R9a800e's class: a false claim in the diagnostic's
// own text). A var with no default says the launch supplies the document.
func TestC180_MessageMatchesTheDocumentShape(t *testing.T) {
	cases := []struct {
		varLine       string
		want, notWant []string
	}{
		{`  cfg: json = "[\"a\", \"b\"]"`, []string{"a list,", "spread into its argv"}, []string{"JSON token"}},
		{`  cfg: json = "{\"a\": 1}"`, []string{"an object,", "ONE JSON token"}, []string{"spread into its argv"}},
		{`  cfg: json = "[{\"a\": 1}]"`, []string{"a list holding a collection,", "ONE JSON token"}, []string{"spread into its argv"}},
		{`  cfg: json`, []string{"the launch supplies", "if it is a list", "an object arrives as ONE JSON token"}, nil},
	}
	for _, tc := range cases {
		r := compileText(t, c180Fixture(tc.varLine, "string", `"{{vars.cfg}}"`))
		var msg string
		for _, d := range r.Diagnostics {
			if d.Code == DiagWithWholeRefListToString {
				msg = d.Message
			}
		}
		if msg == "" {
			t.Errorf("%s: no C180 raised\ndiagnostics: %v", tc.varLine, r.Diagnostics)
			continue
		}
		for _, want := range tc.want {
			if !strings.Contains(msg, want) {
				t.Errorf("%s: message %q does not carry %q", tc.varLine, msg, want)
			}
		}
		for _, bad := range tc.notWant {
			if strings.Contains(msg, bad) {
				t.Errorf("%s: message %q claims %q, a mechanism this shape does not take", tc.varLine, msg, bad)
			}
		}
	}
}

// TestC180_MessageNamesTheDestinationsOwnReading: the shell mechanism is a
// tool's. On an agent the list reaches a prompt, which renders its JSON
// text — a message naming a `command:` there would describe a node that
// does not exist on this edge.
func TestC180_MessageNamesTheDestinationsOwnReading(t *testing.T) {
	src := `dsl: 2

vars:
  b: string[] = "claw,claude_code"

schema sink:
  label: string

schema out:
  ok: bool

prompt p:
  Label: {{input.label}}

emit go:
  event: "go"

agent show:
  input: sink
  output: out
  system: p

workflow w:
  worktree: none
  sandbox: none
  entry: go
  go -> show with { label: "{{vars.b}}" }
  show -> done
`
	r := compileText(t, src)
	var msg string
	for _, d := range r.Diagnostics {
		if d.Code == DiagWithWholeRefListToString {
			msg = d.Message
		}
	}
	if msg == "" {
		t.Fatalf("no C180 raised on an agent destination\ndiagnostics: %v", r.Diagnostics)
	}
	if !strings.Contains(msg, "a prompt renders its JSON text") {
		t.Errorf("message %q does not name the agent's own reading", msg)
	}
	for _, bad := range []string{"command:", "argv"} {
		if strings.Contains(msg, bad) {
			t.Errorf("message %q names %q — a tool's mechanism, on an agent node", msg, bad)
		}
	}
}

// TestC180_ShellMechanismOnlyWhereACommandReadsTheField: the argv spread
// is shellEscapeValue's, and only a tool `command:` reading
// {{input.<key>}} in the escaped form reaches it. A `script:` tool has no
// command at all (its body gets a JSON literal), the raw `{{!…}}` form
// splices the JSON text unquoted, and a command that never reads the field
// spreads nothing — naming a command's argv on any of them would be a
// false claim in the diagnostic's own text.
func TestC180_ShellMechanismOnlyWhereACommandReadsTheField(t *testing.T) {
	fixture := func(toolBody string) string {
		return strings.Replace(c180Fixture(`  b: string[] = "claw,claude_code"`, "string", `"{{vars.b}}"`),
			"  command: `printf 'label=<%s>\\n' {{input.label}}`\n", toolBody, 1)
	}
	cases := []struct {
		name, toolBody string
		spread         bool
	}{
		{"a command reading the field escaped spreads it",
			"  command: `printf 'label=<%s>\\n' {{input.label}}`\n", true},
		{"a script body gets a JSON literal, and has no command",
			"  language: py\n  script: |\n    print({{input.label}})\n", false},
		{"the raw form splices the JSON text unquoted",
			"  command: `printf '%s\\n' {{!input.label}}`\n", false},
		{"a command that never reads the field spreads nothing",
			"  command: `echo hi`\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := fixture(tc.toolBody)
			if !strings.Contains(src, tc.toolBody) {
				t.Fatalf("fixture is inert: the tool body was not spliced in\n%s", src)
			}
			r := compileText(t, src)
			var msg string
			for _, d := range r.Diagnostics {
				if d.Code == DiagWithWholeRefListToString {
					msg = d.Message
				}
			}
			if msg == "" {
				t.Fatalf("no C180 raised\ndiagnostics: %v", r.Diagnostics)
			}
			if got := strings.Contains(msg, "spread into its argv"); got != tc.spread {
				t.Errorf("message names the argv spread = %v, want %v: %q", got, tc.spread, msg)
			}
			if !tc.spread {
				if strings.Contains(msg, "command:") {
					t.Errorf("message %q names a `command:` reading the field — this tool has none", msg)
				}
				if !strings.Contains(msg, "a `script:` body a JSON literal") {
					t.Errorf("message %q does not name the non-shell readings", msg)
				}
			}
		})
	}
}

package ir

import (
	"strings"
	"testing"
)

// c152Fixture is a two-node bot whose edge maps one ref-less literal
// onto the input field `v` of the given type, through a compute that
// passes the field to its own typed output — the consumer that
// measurably breaks on the text. `lit` is spliced into the mapping as
// raw `.bot` source, quotes included, so a fixture can carry escapes.
func c152Fixture(fieldType, lit string) string {
	return `dsl: 2

schema kout:
  ok: bool

schema pin:
  v: ` + fieldType + `

compute pass:
  input: pin
  output: pin
  expr:
    v: "input.v"

tool kick:
  command: ` + "`echo hi`" + `
  output: kout

workflow w:
  worktree: none
  sandbox: none
  entry: kick
  kick -> pass with { v: ` + lit + ` }
  pass -> done
`
}

// c152Message returns the one C152 message the fixture produced.
func c152Message(t *testing.T, r *CompileResult) string {
	t.Helper()
	var msgs []string
	for _, d := range r.Diagnostics {
		if d.Code == DiagWithLiteralTypeMismatch {
			msgs = append(msgs, d.Message)
		}
	}
	if len(msgs) != 1 {
		t.Fatalf("expected exactly one C152, got %d:\n%v", len(msgs), r.Diagnostics)
	}
	return msgs[0]
}

// TestC152ArmPerFieldType is the class table: one row per field type a
// text-arriving value can reach, with the verdict a string earns
// there. `bool` / `int` / `float` / `string[]` fire on EVERY
// text-arriving value — a string is never one of them, whatever it
// spells — so `xs: "a"` fires like `"[\"a\"]"` and `"null"` do;
// `json` accepts a string, so a plain word or a number stays silent
// and only an attempted encoding fires — bracketed, braced, quoted, a
// JSON keyword, whitespace-padded. Removing an arm reddens its rows;
// re-gating the `string[]` arm on an array shape reddens the `"a"` and
// `"a, b"` rows; widening the `json` arm to "not valid JSON" reddens
// the `"fast"` row; dropping the keyword set reddens `"true"`;
// dropping the TrimSpace reddens the padded row.
func TestC152ArmPerFieldType(t *testing.T) {
	cases := []struct {
		typ   string
		lit   string
		fires bool
	}{
		{"bool", `"yes"`, true},
		{"bool", `"true"`, true},
		{"bool", `"{{\"{{\"}}"`, true},
		{"int", `"42"`, true},
		{"int", `"many"`, true},
		{"float", `"3.14"`, true},
		{"string[]", `"a"`, true},
		{"string[]", `"a, b"`, true},
		{"string[]", `"[\"a\",\"b\"]"`, true},
		{"string[]", `"null"`, true},
		{"json", `"[]"`, true},
		{"json", `"{}"`, true},
		{"json", `"''"`, true},
		{"json", `"null"`, true},
		{"json", `"true"`, true},
		{"json", `"false"`, true},
		{"json", `" [] "`, true},
		{"json", `"abc\""`, true},
		{"json", `"the Smiths'"`, false},
		{"json", `"fast"`, false},
		{"json", `"42"`, false},
		{"json", `""`, false},
		{"string", `"hello world"`, false},
		{"string", `"[]"`, false},
	}
	for _, tc := range cases {
		got := compileText(t, c152Fixture(tc.typ, tc.lit))
		fired := hasDiagAtSeverity(got, "C152", SeverityWarning)
		if fired != tc.fires {
			t.Errorf("%s field, literal %s: C152 fired=%v, want %v:\n%v", tc.typ, tc.lit, fired, tc.fires, got.Diagnostics)
		}
	}
}

// TestC152StringArrayRemedyNamesTheListForm: the `string[]` arm's
// remedy spells the list the literal would have to be — `["a"]` for
// the text `a`, the list itself for a text that spells one, a
// one-element list for a text that spells nothing (`"null"` becomes
// the string "null" in a list, never JSON null). A remedy that echoes
// the text as written reddens the first two cases; one that drops the
// non-nil guard reddens the third.
func TestC152StringArrayRemedyNamesTheListForm(t *testing.T) {
	msg := c152Message(t, compileText(t, c152Fixture("string[]", `"a"`)))
	if want := "prints `{\"v\": [\"a\"]}`"; !strings.Contains(msg, want) {
		t.Fatalf("string[] remedy for the text `a` does not name the list form %s:\n%s", want, msg)
	}
	if !strings.Contains(msg, "one string of 1 bytes, not a list") {
		t.Fatalf("string[] arrival does not say the text arrives as one string:\n%s", msg)
	}
	msg = c152Message(t, compileText(t, c152Fixture("string[]", `"[\"a\", \"b\"]"`)))
	if want := "prints `{\"v\": [\"a\",\"b\"]}`"; !strings.Contains(msg, want) {
		t.Fatalf("string[] remedy for a spelled list does not echo the list %s:\n%s", want, msg)
	}
	msg = c152Message(t, compileText(t, c152Fixture("string[]", `"null"`)))
	if want := "prints `{\"v\": [\"null\"]}`"; !strings.Contains(msg, want) {
		t.Fatalf("string[] remedy for `null` is not the one-element string list %s:\n%s", want, msg)
	}
	if strings.Contains(msg, "prints `{\"v\": null}`") {
		t.Fatalf("string[] remedy treated the text `null` as JSON null:\n%s", msg)
	}
}

// TestC152SuggestionSanitisesTheLiteral: a literal carrying a quote, a
// backtick and a newline reaches the remedy through the JSON encoder,
// never verbatim — the suggestion stays one pastable line. Echoing the
// raw text reddens the `string[]` case (the newline lands in the
// message) and the `json` case (the whitespace the compaction removes
// comes back).
func TestC152SuggestionSanitisesTheLiteral(t *testing.T) {
	// Source text `"[\"a\", `b`\"]\n"` — the lexer decodes it to
	// `["a", `b`"]` followed by a newline.
	lit := "\"[\\\"a\\\", `b`\\\"]\\n\""
	msg := c152Message(t, compileText(t, c152Fixture("string[]", lit)))
	if strings.Contains(msg, "\n") {
		t.Fatalf("a raw newline reached the C152 message — the literal was echoed unsanitised:\n%q", msg)
	}
	if want := "prints `{\"v\": [\"[\\\"a\\\", `b`\\\"]\\n\"]}`"; !strings.Contains(msg, want) {
		t.Fatalf("string[] remedy does not carry the JSON-encoded one-element list %s:\n%s", want, msg)
	}

	msg = c152Message(t, compileText(t, c152Fixture("json", `"{ \"k\" : 1 }"`)))
	if want := "prints `{\"v\": {\"k\":1}}`"; !strings.Contains(msg, want) {
		t.Fatalf("json remedy does not echo the compacted value %s:\n%s", want, msg)
	}

	// Not valid JSON: the value the author meant is not recoverable, so
	// nothing is echoed — a re-quoted echo would name a remedy that
	// renders the very same string.
	msg = c152Message(t, compileText(t, c152Fixture("json", `"{'k': 1}"`)))
	if !strings.Contains(msg, "not valid JSON") {
		t.Fatalf("json remedy for an invalid encoding does not say so:\n%s", msg)
	}
	if strings.Contains(msg, "prints `{\"v\": {'k': 1}}`") || strings.Contains(msg, "prints `{\"v\": \"{'k': 1}\"}`") {
		t.Fatalf("json remedy echoes an invalid encoding as a value to print:\n%s", msg)
	}
}

// TestC152ScalarRemedySpellsTheLiteralValue: the scalar arms echo the
// value the text spells as the expr literal to write (`v: "false"`,
// not a fixed example), normalised through the numeric parsers — and a
// float never re-formats into a digit-only literal the expr lexer
// would read as an overflowing integer (`1e21` suggests `1000000000000000000000.0`,
// which parses; the bare digits do not). When the text spells no value
// of the type — a word, a fraction on an `int`, an out-of-range
// magnitude — the remedy says so instead of suggesting a value that
// would silently replace the author's. A remedy that always prints an
// example reddens the false/1e3/2.50/1e21 rows; a `'g'` or
// precision-1 float format reddens the 1e21 and 3.14159 rows.
func TestC152ScalarRemedySpellsTheLiteralValue(t *testing.T) {
	cases := []struct{ typ, lit, want string }{
		{"bool", `"false"`, `v: "false"`},
		{"bool", `"0"`, "spells no value"},
		{"int", `"+07"`, `v: "7"`},
		{"int", `"1e3"`, `v: "1000"`},
		{"int", `"99999999999999999999"`, "spells no value"},
		{"float", `"2.50"`, `v: "2.5"`},
		{"float", `"3.14159"`, `v: "3.14159"`},
		{"float", `"1e21"`, `v: "1000000000000000000000.0"`},
		{"float", `"NaN"`, "spells no value"},
	}
	for _, tc := range cases {
		msg := c152Message(t, compileText(t, c152Fixture(tc.typ, tc.lit)))
		if strings.Contains(tc.want, "spells no value") {
			if !strings.Contains(msg, tc.want) {
				t.Errorf("%s field, literal %s: remedy does not admit the unspellable text:\n%s", tc.typ, tc.lit, msg)
			}
			continue
		}
		if !strings.Contains(msg, "(`"+tc.want+"`)") {
			t.Errorf("%s field, literal %s: remedy does not spell %s:\n%s", tc.typ, tc.lit, tc.want, msg)
		}
	}
}

// TestC152ScalarArrivalNamesTheConformFailure: the scalar arms quote
// the SCHEMA_VALIDATION wording a compute pass-through produces —
// `expected bool` / `expected integer` / `expected number` — so the
// author can grep the run failure back to the diagnostic.
func TestC152ScalarArrivalNamesTheConformFailure(t *testing.T) {
	cases := []struct{ typ, want string }{
		{"bool", "`expected bool, got string`"},
		{"int", "`expected integer, got string`"},
		{"float", "`expected number, got string`"},
	}
	for _, tc := range cases {
		msg := c152Message(t, compileText(t, c152Fixture(tc.typ, `"1"`)))
		if !strings.Contains(msg, tc.want) {
			t.Errorf("%s field: arrival does not quote %s:\n%s", tc.typ, tc.want, msg)
		}
	}
}

// TestC152SilentOnUnparseableTemplate: a mapping whose template failed
// to parse (`"x{{vars.ok}}{{"` — ParseRefs returns nothing) already
// carries C004 for the broken braces; C152 stays out of the way
// instead of double-reporting the remnant as a "literal".
func TestC152SilentOnUnparseableTemplate(t *testing.T) {
	got := compileText(t, c152Fixture("bool", `"x{{vars.ok}}{{"`))
	if !anyDiag(got, "C004") {
		t.Fatalf("expected C004 on the unterminated template, got:\n%v", got.Diagnostics)
	}
	if n := countByCode(got, "C152"); n != 0 {
		t.Fatalf("C152 double-reports a broken template C004 already flags (%d hits):\n%v", n, got.Diagnostics)
	}
}

// TestC152InterpolatedTemplateIntoTypedField: a template that
// interpolates a reference into prose — `{{vars.n}} ` with a trailing
// space, a word before a ref, brackets around a ref — renders the
// destination a STRING at run time, so the scalar and `string[]` arms
// fire exactly as they do on a ref-less literal. A mapping that is
// exactly one reference passes the value's type through and stays
// silent — the forbidden alternative the contrast fixture pins.
func TestC152InterpolatedTemplateIntoTypedField(t *testing.T) {
	src := `dsl: 2

vars:
  n: int = 42
  flag: bool = true
  xs: string[] = "[\"a\"]"
  j: json = "{\"k\": 1}"

schema kout:
  ok: bool

schema pin:
  n: int
  flag: bool
  xs: string[]
  j: json

compute pass:
  input: pin
  output: pin
  expr:
    n: "input.n"
    flag: "input.flag"
    xs: "input.xs"
    j: "input.j"

tool kick:
  command: ` + "`echo hi`" + `
  output: kout

workflow w:
  worktree: none
  sandbox: none
  entry: kick
  kick -> pass with {
    n: "{{vars.n}} "
    flag: "x{{vars.flag}}"
    xs: "[{{vars.xs}}]"
    j: "{{vars.j}} "
  }
  pass -> done
`
	got := compileText(t, src)
	if n := countByCode(got, "C152"); n != 4 {
		t.Fatalf("interpolated templates into typed fields: expected 4 C152, got %d:\n%v", n, got.Diagnostics)
	}
	// The delivered value on an interpolated mapping is the RENDERED
	// template, not the raw text — the message must not quote a byte
	// count of the raw (`{{vars.n}} ` is 11 bytes of source; the run
	// delivers "42 ", 3 bytes).
	for _, d := range got.Diagnostics {
		if d.Code == DiagWithLiteralTypeMismatch && strings.Contains(d.Message, "bytes") {
			t.Fatalf("an interpolated mapping's arrival quotes a byte count of the raw text, which the runtime does not deliver:\n%s", d.Message)
		}
	}

	// Contrast: the same mappings, each exactly one reference — the
	// engine passes the typed value through; C152 must stay silent.
	passthrough := strings.Replace(src, `with {
    n: "{{vars.n}} "
    flag: "x{{vars.flag}}"
    xs: "[{{vars.xs}}]"
    j: "{{vars.j}} "
  }`, `with {
    n: "{{vars.n}}"
    flag: "{{vars.flag}}"
    xs: "{{vars.xs}}"
    j: "{{vars.j}}"
  }`, 1)
	if passthrough == src {
		t.Fatal("contrast fixture did not apply")
	}
	ok := compileText(t, passthrough)
	if n := countByCode(ok, "C152"); n != 0 {
		t.Fatalf("C152 fires on a mapping that is exactly one reference (the typed passthrough):\n%v", ok.Diagnostics)
	}
}

package ir

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// listRoutingSrc builds a workflow with the two surfaces it names and no
// others: an edge `when` expression and a compute `expr:` field. The
// remaining surfaces C155 walks — a tool's shell and script bodies, a
// fallback `when:`, a loop cap in expression form — have their own fixture
// below, because a table whose cases all ride one arm cannot tell which arm
// is alive.
func listRoutingSrc(varsLine, whenExpr, computeExpr string) string {
	return fmt.Sprintf(`
dsl: 2

vars:
%s

schema note:
  value: string

schema tally:
  n: int

prompt sys:
  hi

agent a:
  backend: "claw"
  model: "anthropic/claude-sonnet-4-6"
  output: note
  system: sys

compute c:
  output: tally
  expr:
    n: "%s"

workflow w:
  entry: a
  a -> c when "%s"
  a -> done
  c -> done
`, varsLine, computeExpr, whenExpr)
}

// TestC155_FiresWhereTheDefaultStopsBeingText pins the one condition: the
// default's TYPE moves under the run's reading, AND a routing decision
// reads it.
func TestC155_FiresWhereTheDefaultStopsBeingText(t *testing.T) {
	cases := []struct {
		name     string
		varsLine string
		when     string
		compute  string
		want     int
	}{
		{
			name:     "string[] default read by a when and a compute",
			varsLine: `  tags: string[] = "a,b"`,
			when:     "vars.tags == 'a,b'",
			compute:  "0",
			want:     1,
		},
		{
			name:     "json default read by a compute",
			varsLine: `  invalid: json = "[]"`,
			when:     "true",
			compute:  "length(vars.invalid)",
			want:     1,
		},
		{
			name:     "both surfaces read it",
			varsLine: `  tags: string[] = "a,b"`,
			when:     "length(vars.tags) > 2",
			compute:  "length(vars.tags)",
			want:     2,
		},
		{
			// A `json` var whose text is not JSON still reads as that
			// text: nothing moved, nothing to warn about.
			name:     "json default that is not JSON",
			varsLine: `  note: json = "free text"`,
			when:     "vars.note == 'free text'",
			compute:  "0",
			want:     0,
		},
		{
			// No default: an override was always the only source, and it
			// was always coerced.
			name:     "no default",
			varsLine: `  tags: string[]`,
			when:     "length(vars.tags) > 0",
			compute:  "0",
			want:     0,
		},
		{
			// The type moved, but nothing ROUTES on it.
			name:     "shifted default nobody routes on",
			varsLine: `  tags: string[] = "a,b"`,
			when:     "true",
			compute:  "0",
			want:     0,
		},
		{
			name:     "scalar default",
			varsLine: `  mode: string = "fast"`,
			when:     "vars.mode == 'fast'",
			compute:  "0",
			want:     0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := compileFile(t, listRoutingSrc(tc.varsLine, tc.when, tc.compute))
			if got := countCode(r, DiagVarListDefaultRouting); got != tc.want {
				t.Errorf("C155 count = %d, want %d\ndiagnostics: %v", got, tc.want, r.Diagnostics)
			}
		})
	}
}

// The message must carry BOTH readings: a warning that only names the new
// one leaves the author guessing what their expression used to see.
func TestC155_MessageNamesBothReadings(t *testing.T) {
	r := compileFile(t, listRoutingSrc(`  tags: string[] = "a,b"`, "vars.tags == 'a,b'", "0"))
	var msg string
	for _, d := range r.Diagnostics {
		if d.Code == DiagVarListDefaultRouting {
			msg = d.Message
		}
	}
	if msg == "" {
		t.Fatalf("no C155 emitted: %v", r.Diagnostics)
	}
	for _, want := range []string{`"tags"`, `["a","b"]`, `"a,b"`, "string[]"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not carry %q", msg, want)
		}
	}
}

// The shift set is read with NO environment on purpose: a diagnostic that
// consulted the compiling host's env would say different things on two
// machines. Setting the var's env here must not change the verdict.
func TestC155_ShiftSetIgnoresTheHostEnvironment(t *testing.T) {
	t.Setenv("C155_LIST", "one,two,three")
	r := compileFile(t, listRoutingSrc(`  tags: string[] = "${C155_LIST:-a,b}"`, "vars.tags == 'a,b'", "0"))
	var msg string
	for _, d := range r.Diagnostics {
		if d.Code == DiagVarListDefaultRouting {
			msg = d.Message
		}
	}
	if msg == "" {
		t.Fatalf("no C155 emitted: %v", r.Diagnostics)
	}
	if strings.Contains(msg, "one") {
		t.Errorf("the diagnostic read the host environment: %q", msg)
	}
}

// ---------------------------------------------------------------------------
// ResolveVarText — the ONE reading (#1285)
// ---------------------------------------------------------------------------

func TestResolveVarText_ExpandsThenCoerces(t *testing.T) {
	noEnv := func(string) string { return "" }
	cases := []struct {
		name string
		in   any
		vt   VarType
		want any
	}{
		// The order that matters: coercing first split the unexpanded
		// text on its comma into ["${LIST:-a", "b}"].
		{"string[] through a defaulted env form", "${C3_LIST:-a,b}", VarStringArray, []any{"a", "b"}},
		{"string[] comma text", "a,b", VarStringArray, []any{"a", "b"}},
		{"string[] JSON form", `["a","b"]`, VarStringArray, []any{"a", "b"}},
		{"string[] empty", "", VarStringArray, []any{}},
		{"string through a defaulted env form", "${C3_MODE:-fast}", VarString, "fast"},
		{"int", "3", VarInt, int64(3)},
		{"bool", "yes", VarBool, true},
		// A document's `$` is data — but its `${…}` is still the DSL's
		// one universal idiom and must resolve.
		{"json keeps a literal dollar", `{"cost":"$5"}`, VarJSON, map[string]any{"cost": "$5"}},
		{"json keeps a shell program", `{"awk":"{print $1}"}`, VarJSON, map[string]any{"awk": "{print $1}"}},
		{"json resolves a braced reference", `{"dir":"${C3_DIR:-/wt}"}`, VarJSON, map[string]any{"dir": "/wt"}},
		{"json list with a braced reference", `["${C3_ONE:-a}","b"]`, VarJSON, []any{"a", "b"}},
		{"json list", "[1, 2]", VarJSON, []any{float64(1), float64(2)}},
		{"json empty list", "[]", VarJSON, []any{}},
		// Text that is not JSON stays text — and THEN expands, so a
		// `json` var still reaches ${…} the way every other field does.
		{"json non-JSON text expands", "${C3_MODE:-fast}", VarJSON, "fast"},
		// A value that is not text is typed already.
		{"typed value passes through", 7, VarInt, 7},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveVarText(tc.in, tc.vt, noEnv)
			if err != nil {
				t.Fatalf("ResolveVarText(%q, %s): %v", tc.in, tc.vt, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ResolveVarText(%q, %s) = %#v, want %#v", tc.in, tc.vt, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The expander's own bounds
// ---------------------------------------------------------------------------

// A document's `$` is data. The braced-only reading is what lets a `json`
// var carry `$5` and `{print $1}` while `${PROJECT_DIR}` still resolves.
func TestExpandBracedWithDefault_LeavesBareDollarAlone(t *testing.T) {
	lookup := func(k string) string {
		if k == "SET" {
			return "value"
		}
		return ""
	}
	cases := []struct{ in, braced, full string }{
		{"costs $5", "costs $5", "costs "},
		{"{print $1}", "{print $1}", "{print }"},
		{"$SET", "$SET", "value"},
		{"${SET}", "value", "value"},
		{"${UNSET:-fallback}", "fallback", "fallback"},
	}
	for _, c := range cases {
		if got := ExpandBracedWithDefault(c.in, lookup); got != c.braced {
			t.Errorf("ExpandBracedWithDefault(%q) = %q, want %q", c.in, got, c.braced)
		}
		if got := ExpandWithDefault(c.in, lookup); got != c.full {
			t.Errorf("ExpandWithDefault(%q) = %q, want %q", c.in, got, c.full)
		}
	}
}

// listSurfaceSrc builds the surfaces listRoutingSrc does not: a tool's
// `command:` and `script:` bodies, a bounded loop whose cap is an
// expression, and an agent's `fallbacks:` gate.
func listSurfaceSrc(varsLine, body string) string {
	return fmt.Sprintf(`
dsl: 2

vars:
%s

schema note:
  value: string

prompt sys:
  hi

agent a:
  backend: "claw"
  model: "anthropic/claude-sonnet-4-6"
  output: note
  system: sys
  fallbacks:
    cli:
      backend: "claude_code"
      model: "anthropic/claude-sonnet-4-6"
      when: "length(vars.%s) > 0"

%s

workflow w:
  entry: a
  a -> t
  t -> a as retry("length(vars.%s)")
  t -> done
`, varsLine, "tags", body, "tags")
}

// Every arm C155 declares, one fixture each: delete the arm, this test
// reddens. Without them `collectShiftedRefs` could be emptied entirely and
// the suite stayed green.
func TestC155_EachSurfaceIsWalked(t *testing.T) {
	const decl = `  tags: string[] = "a,b"`
	cases := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "tool shell body",
			body: "tool t:\n  command: `TAGS={{vars.tags}} run`",
			want: []string{`tool "t" shell body`},
		},
		{
			name: "tool postcondition",
			body: "tool t:\n  command: `run`\n  postcondition: `test -n {{vars.tags}}`",
			want: []string{`tool "t" shell body`},
		},
		{
			name: "tool script body",
			body: "tool t:\n  language: py\n  script: |\n    xs = {{vars.tags}}\n    print('{}')",
			want: []string{`tool "t" script`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := compileFile(t, listSurfaceSrc(decl, tc.body))
			var msgs []string
			for _, d := range r.Diagnostics {
				if d.Code == DiagVarListDefaultRouting {
					msgs = append(msgs, d.Message)
				}
			}
			// The fallback gate and the loop cap ride every case of this
			// table, so each run also witnesses those two arms.
			for _, want := range append(tc.want, `agent "a" fallback`, `edge t -> a`) {
				found := false
				for _, m := range msgs {
					if strings.Contains(m, want) {
						found = true
					}
				}
				if !found {
					t.Errorf("no C155 naming %q; got %v", want, msgs)
				}
			}
		})
	}
}

// A `json` var read through a MEMBER — `vars.cfg.enabled`, the canonical
// way to route on a document — is a read of the whole value, which is what
// moved. Recognising only the bare `vars.cfg` form let an edge flip sides
// in silence.
func TestC155_DrilledReadCounts(t *testing.T) {
	r := compileFile(t, listRoutingSrc(`  cfg: json = "{\"ok\":true}"`, "vars.cfg.ok", "0"))
	if got := countCode(r, DiagVarListDefaultRouting); got != 1 {
		t.Errorf("C155 count = %d, want 1 for a member read\ndiagnostics: %v", got, r.Diagnostics)
	}
}

// The shift set compares the VALUE, not its Go type. A `json` var whose
// text is a JSON string still reads as a string — and still changed, from
// the seven characters written to the five meant.
func TestC155_JSONStringDefaultCounts(t *testing.T) {
	r := compileFile(t, listRoutingSrc(`  label: json = "\"hello\""`, `vars.label == 'hello'`, "0"))
	if got := countCode(r, DiagVarListDefaultRouting); got != 1 {
		t.Errorf("C155 count = %d, want 1 for a JSON-string default\ndiagnostics: %v", got, r.Diagnostics)
	}
}

// A `json` value reaches a shell body as one token of its JSON text before
// and after — only the whitespace the author typed is gone. Warning there
// would be noise the author cannot act on.
func TestC155_QuietOnAJSONValueInAShellBody(t *testing.T) {
	r := compileFile(t, listSurfaceSrc(`  tags: string[] = "a,b"`+"\n"+`  doc: json = "[1, 2]"`,
		"tool t:\n  command: `DOC={{vars.doc}} run`"))
	for _, d := range r.Diagnostics {
		if d.Code == DiagVarListDefaultRouting && strings.Contains(d.Message, `"doc"`) {
			t.Errorf("C155 fired on a json value in a shell body: %s", d.Message)
		}
	}
}

// ---------------------------------------------------------------------------
// The contract view of a default
// ---------------------------------------------------------------------------

// The public contract advertises a var's default, and C300 refuses a port
// whose declared default disagrees with it. Reading the text any other way
// than a run does made the port advertise `["${LIST:-a","b}"]` for a var
// the run seeds as ["a","b"] — and then refused the author's correct
// default.
func TestSeededDefault_ReadsATextAsARunDoes(t *testing.T) {
	t.Setenv("C155_CONTRACT_LIST", "ignored,by,design")
	cases := []struct {
		v    *Var
		want string
	}{
		{&Var{Name: "tags", Type: VarStringArray, HasDefault: true, Default: "${C155_CONTRACT_LIST:-a,b}"}, `["a","b"]`},
		{&Var{Name: "tags", Type: VarStringArray, HasDefault: true, Default: "a,b"}, `["a","b"]`},
		{&Var{Name: "cfg", Type: VarJSON, HasDefault: true, Default: "[]"}, `[]`},
		{&Var{Name: "mode", Type: VarString, HasDefault: true, Default: "${C155_CONTRACT_MODE:-fast}"}, `"fast"`},
		{&Var{Name: "n", Type: VarInt, HasDefault: true, Default: int64(3)}, `3`},
	}
	for _, c := range cases {
		if got := string(varDefaultJSON(c.v)); got != c.want {
			t.Errorf("var %q (%s) default %v published as %s, want %s", c.v.Name, c.v.Type, c.v.Default, got, c.want)
		}
	}
}

// A `${…}` inside a JSON DOCUMENT resolves, and it resolves in the leaf
// rather than in the syntax: splicing the expansion into the text before
// parsing let a value carrying a `"` break the document, after which
// CoerceVarValue returned the corrupt text as a plain STRING — a `json`
// var silently arriving as a string, with no error and no diagnostic.
func TestResolveVarText_ExpandsTheLeavesOfADocument(t *testing.T) {
	lookup := func(k string) string {
		return map[string]string{
			"C155_DIR":   "/wt",
			"C155_QUOTE": `he said "hi"`,
			"C155_BS":    `C:\tmp`,
		}[k]
	}
	cases := []struct {
		in   string
		want any
	}{
		{`{"dir":"${C155_DIR}"}`, map[string]any{"dir": "/wt"}},
		{`{"a":{"b":["${C155_DIR}","$5"]}}`, map[string]any{"a": map[string]any{"b": []any{"/wt", "$5"}}}},
		// An expansion carrying the document's own syntax stays DATA.
		{`{"msg":"${C155_QUOTE}"}`, map[string]any{"msg": `he said "hi"`}},
		{`{"p":"${C155_BS}"}`, map[string]any{"p": `C:\tmp`}},
		// A key is a name, not a value.
		{`{"${C155_DIR}":1}`, map[string]any{"${C155_DIR}": float64(1)}},
	}
	for _, c := range cases {
		got, err := ResolveVarText(c.in, VarJSON, lookup)
		if err != nil {
			t.Fatalf("ResolveVarText(%q): %v", c.in, err)
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("ResolveVarText(%q) = %#v, want %#v", c.in, got, c.want)
		}
	}
}

// A compile-time view resolves what has an answer with no environment and
// keeps the rest AS WRITTEN. Resolving `${PROJECT_DIR}/audits` against an
// empty environment published `/audits` — a path at the filesystem root
// that is neither the source text nor any run's value — on 86 shipped var
// defaults, and made C300 refuse a port that mirrored its var exactly.
func TestSeededDefault_KeepsAnUnresolvableReferenceAsWritten(t *testing.T) {
	t.Setenv("C155_AS_WRITTEN", "from-the-host")
	cases := []struct {
		v    *Var
		want string
	}{
		{&Var{Name: "ws", Type: VarString, HasDefault: true, Default: "${PROJECT_DIR}"}, `"${PROJECT_DIR}"`},
		{&Var{Name: "dir", Type: VarString, HasDefault: true, Default: "${PROJECT_DIR}/audits"}, `"${PROJECT_DIR}/audits"`},
		{&Var{Name: "home", Type: VarString, HasDefault: true, Default: "$HOME/x"}, `"$HOME/x"`},
		{&Var{Name: "host", Type: VarString, HasDefault: true, Default: "${C155_AS_WRITTEN}"}, `"${C155_AS_WRITTEN}"`},
		{&Var{Name: "cfg", Type: VarJSON, HasDefault: true, Default: `{"dir":"${PROJECT_DIR}"}`}, `{"dir":"${PROJECT_DIR}"}`},
		{&Var{Name: "dirs", Type: VarStringArray, HasDefault: true, Default: "${SCAN_DIRS}"}, `["${SCAN_DIRS}"]`},
		// A `:-` fallback HAS an answer with no environment: it resolves,
		// which is the #1285 win the contract view is here for.
		{&Var{Name: "tags", Type: VarStringArray, HasDefault: true, Default: "${LIST:-a,b}"}, `["a","b"]`},
	}
	for _, c := range cases {
		if got := string(varDefaultJSON(c.v)); got != c.want {
			t.Errorf("var %q (%s) default %v published as %s, want %s", c.v.Name, c.v.Type, c.v.Default, got, c.want)
		}
	}
}

// The raw form emits the value's own text with no quoting at all, so it
// does not change the way the default form does — and must not be told
// the arity sentence, whose remedy does not apply to it.
func TestC155_RawFormGetsItsOwnConsequence(t *testing.T) {
	r := compileFile(t, listSurfaceSrc(`  tags: string[] = "a,b"`,
		"tool t:\n  command: `TAGS={{!vars.tags}} run`"))
	var msg string
	for _, d := range r.Diagnostics {
		if d.Code == DiagVarListDefaultRouting && strings.Contains(d.Message, `tool "t" shell body`) {
			msg = d.Message
		}
	}
	if msg == "" {
		t.Fatalf("no C155 on the raw form: %v", r.Diagnostics)
	}
	if !strings.Contains(msg, "raw form") {
		t.Errorf("the raw form got the quoted form's consequence: %s", msg)
	}
	if strings.Contains(msg, "arity moves") {
		t.Errorf("the raw form was told the arity sentence, which does not apply: %s", msg)
	}
}

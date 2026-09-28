package ir

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/expr"
)

// The fixture family for the collection-literal static checks (#1525): one
// compute whose expressions exercise C306 (a collection comparison is
// constant) and C307 (a collection literal the field's type cannot hold).
const literalHead = `schema src_out:
  tags: string[]
  other: string[]
  n: int
  j: json

schema lit_out:
  cmp: bool
  xs: string[]
  s: string
  n: int
  j: json

agent measure:
  model: "m"
  output: src_out

compute compute_lit:
  input: src_out
  output: lit_out
  expr:
`

const literalTail = `
workflow w:
  worktree: none
  sandbox: none
  entry: measure
  measure -> compute_lit
  compute_lit -> done
`

// TestC306CollectionCompareIsConstant: `==`/`!=` never walks into a slice
// or a map, so a comparison with a collection on either side is constant
// (`[1] == [1]` is false) — the warning fires on a literal either side, and
// on two statically-known string[] operands. A comparison C107 already owns
// (both operands known AND incompatible) is not double-reported.
func TestC306CollectionCompareIsConstant(t *testing.T) {
	for name, tc := range map[string]struct {
		expr    string
		want306 bool
	}{
		"two list literals":          {expr: `['a'] == ['a']`, want306: true},
		"two object literals":        {expr: `{a: 1} == {a: 1}`, want306: true},
		"literal vs empty literal":   {expr: `['a'] != []`, want306: true},
		"literal vs ref":             {expr: `input.tags == ['a']`, want306: true},
		"ref vs literal":             {expr: `{a: 1} != input.tags`, want306: true},
		"two known collections":      {expr: `input.tags == input.other`, want306: true},
		"keys == keys":               {expr: `keys(input.j) == keys(input.j)`, want306: true},        // LOW 1: the helper's result is a collection
		"values == values":           {expr: `values(input.j) != values(input.j)`, want306: true},    // collection of unknowable elements: still constant
		"sort == sort on string[]":   {expr: `sort(input.tags) == sort(input.other)`, want306: true}, // element-preserving mirror
		"map == map":                 {expr: `map(input.tags, x => x) == map(input.tags, x => x)`, want306: true},
		"scalar comparison":          {expr: `input.n == 1`, want306: false},
		"known collection vs scalar": {expr: `input.tags == 'x'`, want306: false},       // C107 owns it
		"helper vs scalar":           {expr: `sort(input.tags) == 'x'`, want306: false}, // C107 owns it (sort of string[] infers string[])
		"literal vs scalar":          {expr: `['a'] == 'x'`, want306: false},            // C107 owns it (the all-string literal infers string[])
		"mixed literal vs scalar":    {expr: `[1, 'a'] == 'x'`, want306: true},          // the literal infers no element type: no C107, still constant
	} {
		t.Run(name, func(t *testing.T) {
			src := literalHead + "    cmp: \"" + strings.ReplaceAll(tc.expr, `"`, `'`) + "\"\n" +
				"    xs: \"input.tags\"\n    s: \"'x'\"\n    n: \"input.n\"\n    j: \"input.tags\"\n" + literalTail
			cr := compileText(t, src)
			if got := countByCode(cr, "C306"); (got > 0) != tc.want306 {
				t.Errorf("C306 count = %d, want fired=%v\ndiagnostics: %v", got, tc.want306, cr.Diagnostics)
			}
		})
	}
}

// TestC306DoesNotDoubleReportC107: when both operands are statically known
// and incompatible, the comparison carries C107 alone — one finding per
// site, not two wordings of it.
func TestC306DoesNotDoubleReportC107(t *testing.T) {
	src := literalHead + "    cmp: \"input.tags == 'x'\"\n" +
		"    xs: \"input.tags\"\n    s: \"'x'\"\n    n: \"input.n\"\n    j: \"input.tags\"\n" + literalTail
	cr := compileText(t, src)
	if got := countByCode(cr, "C107"); got != 1 {
		t.Fatalf("C107 count = %d, want 1\ndiagnostics: %v", got, cr.Diagnostics)
	}
	if got := countByCode(cr, "C306"); got != 0 {
		t.Fatalf("C306 double-reports what C107 owns (%d hits)\ndiagnostics: %v", got, cr.Diagnostics)
	}
}

// TestC306OnAWhenEdge: the same check walks a quoted `when` — an edge whose
// guard is a collection comparison is constant there too, and constant
// routing is the worse surprise.
func TestC306OnAWhenEdge(t *testing.T) {
	src := `schema src_out:
  tags: string[]

agent measure:
  model: "m"
  output: src_out

agent next:
  model: "m"
  output: src_out

workflow w:
  worktree: none
  sandbox: none
  entry: measure
  measure -> next when "input.tags == ['a']"
  measure -> done else
  next -> done
`
	cr := compileText(t, src)
	if got := countByCode(cr, "C306"); got != 1 {
		t.Fatalf("C306 count on a when edge = %d, want 1\ndiagnostics: %v", got, cr.Diagnostics)
	}
}

// TestC307CollectionLiteralConformance: a compute field fed by a collection
// literal its declared type cannot hold fails SCHEMA_VALIDATION at run time,
// and only there — the warning names the shape at compile time. An element
// the compiler cannot type (a ref, a call) is not held against the author;
// a json field accepts any of it.
func TestC307CollectionLiteralConformance(t *testing.T) {
	for name, tc := range map[string]struct {
		field string // the lit_out field the expression feeds
		expr  string
		want  bool
	}{
		"non-string element":        {"xs", `['a', 1]`, true},
		"bool element":              {"xs", `[true]`, true},
		"bool expression element":   {"xs", `[input.n == 1]`, true},  // a ==/!= infers bool (LOW 2)
		"known int ref element":     {"xs", `[input.n]`, true},       // a typed ref, known from the source
		"collection helper element": {"xs", `[keys(input.j)]`, true}, // keys() is a string[] — a nested collection
		"nested collection":         {"xs", `[['a']]`, true},
		"all strings":               {"xs", `['a', 'b']`, false},
		"empty list":                {"xs", `[]`, false},
		"untypable element":         {"xs", `[input.j]`, false},               // a json field: no opinion
		"string-typed expression":   {"xs", `[join(input.tags, '-')]`, false}, // join() infers string
		"object into string[]":      {"xs", `{a: 1}`, true},
		"list into a string field":  {"s", `['a']`, true},
		"object into an int field":  {"n", `{a: 1}`, true},
		"scalar into string[]? no — a plain string is fine as an expression": {"xs", `input.tags`, false},
		"json takes an object":     {"j", `{a: 1}`, false},
		"json takes a mixed list":  {"j", `['a', 1]`, false},
		"scalar into scalar field": {"n", `1 + 1`, false},
	} {
		t.Run(name, func(t *testing.T) {
			exprs := map[string]string{
				"cmp": "true", "xs": "input.tags", "s": "'x'", "n": "input.n", "j": "input.tags",
			}
			exprs[tc.field] = tc.expr
			var b strings.Builder
			b.WriteString(literalHead)
			for _, k := range []string{"cmp", "xs", "s", "n", "j"} {
				b.WriteString("    " + k + ": \"" + exprs[k] + "\"\n")
			}
			b.WriteString(literalTail)
			cr := compileText(t, b.String())
			if got := countByCode(cr, "C307"); (got > 0) != tc.want {
				t.Errorf("C307 count = %d, want fired=%v for %s: %q\ndiagnostics: %v", got, tc.want, tc.field, tc.expr, cr.Diagnostics)
			}
		})
	}
}

// TestC307NamesTheElement: the message points at the offending element, so
// the author finds it without re-reading the whole literal.
func TestC307NamesTheElement(t *testing.T) {
	src := literalHead + "    cmp: \"true\"\n    xs: \"['a', 1]\"\n    s: \"'x'\"\n    n: \"input.n\"\n    j: \"input.tags\"\n" + literalTail
	cr := compileText(t, src)
	for _, d := range cr.Diagnostics {
		if d.Code == DiagCollectionLiteralConformance {
			if !strings.Contains(d.Message, "element 1") || d.Severity != SeverityWarning {
				t.Fatalf("C307 does not name element 1 as a warning: %+v", d)
			}
			return
		}
	}
	t.Fatalf("no C307 emitted: %v", cr.Diagnostics)
}

// TestC152StringArrayRemedyOffersTheComputeLiteral: with the literal in the
// language (#1525), the string[] arm offers the rich form first — the
// constant emitted from a compute's `expr:` — next to the producer form it
// always named.
func TestC152StringArrayRemedyOffersTheComputeLiteral(t *testing.T) {
	msg := c152Message(t, compileText(t, c152Fixture("string[]", `"a"`)))
	if want := "(`v: \"['a']\"`)"; !strings.Contains(msg, want) {
		t.Fatalf("string[] remedy does not offer the compute literal %s:\n%s", want, msg)
	}
	if want := "prints `{\"v\": [\"a\"]}`"; !strings.Contains(msg, want) {
		t.Fatalf("string[] remedy dropped the producer form %s:\n%s", want, msg)
	}
	msg = c152Message(t, compileText(t, c152Fixture("string[]", `"[\"a\", \"b\"]"`)))
	if want := "(`v: \"['a', 'b']\"`)"; !strings.Contains(msg, want) {
		t.Fatalf("string[] remedy for a spelled list does not re-spell it as a literal %s:\n%s", want, msg)
	}
}

// TestC152UnspellableElementKeepsTheProducerForm: an element carrying a
// quote, a backslash or a control character has no one-line spelling that
// reads the same in both profiles — the remedy says so and keeps the
// producer form rather than suggest a line that reads otherwise.
func TestC152UnspellableElementKeepsTheProducerForm(t *testing.T) {
	msg := c152Message(t, compileText(t, c152Fixture("string[]", `"it's"`)))
	if !strings.Contains(msg, "cannot spell this text") {
		t.Fatalf("remedy for an unspellable element does not say why no literal is offered:\n%s", msg)
	}
	if strings.Contains(msg, "(`v: \"['it's']\"`)") {
		t.Fatalf("remedy offered a literal whose single quote would close the expr string:\n%s", msg)
	}
	if want := "prints `{\"v\": [\"it's\"]}`"; !strings.Contains(msg, want) {
		t.Fatalf("remedy dropped the producer form %s:\n%s", want, msg)
	}
}

// TestC152JSONRemedyOffersTheComputeLiteral: a `json` field whose text
// decodes to a value the literal spells gets the compute form — objects,
// lists, scalars — while JSON null and a non-identifier key keep the
// producer form with the reason named.
func TestC152JSONRemedyOffersTheComputeLiteral(t *testing.T) {
	cases := []struct {
		lit     string
		want    string // substring the remedy must carry
		notWant string // substring it must not
	}{
		{`"{\"k\": 1}"`, "(`v: \"{k: 1}\"`)", ""},
		{`"[1, \"a\"]"`, "(`v: \"[1, 'a']\"`)", ""},
		{`"true"`, "(`v: \"true\"`)", ""},
		{`"{}"`, "(`v: \"{}\"`)", ""},
		{`"[]"`, "(`v: \"[]\"`)", ""},
		{`"null"`, "JSON null has no expr spelling", "(`v: \""},
		{`"{\"a-b\": 1}"`, "a bare identifier only", "(`v: \""},
		{`"{\"true\": 1}"`, "a bare identifier only", "(`v: \""}, // a keyword key: the suggestion stays conservative
	}
	for _, tc := range cases {
		msg := c152Message(t, compileText(t, c152Fixture("json", tc.lit)))
		if !strings.Contains(msg, tc.want) {
			t.Errorf("json remedy for %s does not carry %q:\n%s", tc.lit, tc.want, msg)
		}
		if tc.notWant != "" && strings.Contains(msg, tc.notWant) {
			t.Errorf("json remedy for %s must not carry %q:\n%s", tc.lit, tc.notWant, msg)
		}
	}
}

// TestSpellExprJSONTextParsesBack: the suggestion the remedy prints is only
// worth making if the author can paste it — every value the helper spells
// must parse as an expr literal and evaluate to the value it came from
// (numbers modulo the int/float representation), and every value it refuses
// must be one the parser would reject anyway (a keyword key, a
// non-identifier key, JSON null).
func TestSpellExprJSONTextParsesBack(t *testing.T) {
	for _, s := range []string{
		`{"k":1}`, `[1,"a"]`, "true", "false", "[]", "{}", "1", "1.5", "-2",
		`{"a":{"b":["c"]}}`, "1e3", `""`, `"plain"`, `[true,false]`,
	} {
		sp, ok := spellExprJSONText(s)
		if !ok {
			t.Errorf("spellExprJSONText(%s) refused a spellable value", s)
			continue
		}
		ast, err := expr.Parse(sp)
		if err != nil {
			t.Errorf("spellExprJSONText(%s) = %q does not parse: %v", s, sp, err)
			continue
		}
		got, err := ast.Eval(nil)
		if err != nil {
			t.Errorf("spellExprJSONText(%s) = %q does not evaluate: %v", s, sp, err)
			continue
		}
		var want any
		if err := json.Unmarshal([]byte(s), &want); err != nil {
			t.Fatalf("fixture %s is not valid JSON", s)
		}
		if !reflect.DeepEqual(got, normalizeJSONNumbers(want)) {
			t.Errorf("spellExprJSONText(%s) = %q evaluates to %#v, want %#v", s, sp, got, want)
		}
	}
	for _, s := range []string{`{"true":1}`, `{"and":1}`, `{"a-b":1}`, "null", `{"k":null}`, `["it's"]`, `{"sp ace":1}`, "1e21"} {
		if sp, ok := spellExprJSONText(s); ok {
			t.Errorf("spellExprJSONText(%s) = %q — that spelling must be refused (the parser or a profile would not read it back)", s, sp)
		}
	}
}

// normalizeJSONNumbers turns a JSON-decoded value's float64s into the int64s
// an expr literal evaluates to, so the two compare.
func normalizeJSONNumbers(v any) any {
	switch t := v.(type) {
	case float64:
		if t == math.Trunc(t) && t >= math.MinInt64 && t < 9223372036854775808.0 {
			return int64(t)
		}
		return t
	case []any:
		out := make([]any, len(t))
		for i, el := range t {
			out[i] = normalizeJSONNumbers(el)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, el := range t {
			out[k] = normalizeJSONNumbers(el)
		}
		return out
	}
	return v
}

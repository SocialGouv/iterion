package unparse_test

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/expr"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unparse"
)

// TestCollectionLiteralExprRoundTrip: the unparser re-emits a compute's
// expr text verbatim (it is a quoted DSL string, quoted by str()), so a
// collection literal — braces, colons, single quotes, and the
// double-quoted-string form included — survives parse → unparse → parse in
// both profiles and still compiles to the same expression.
func TestCollectionLiteralExprRoundTrip(t *testing.T) {
	exprs := []struct {
		text string // the expr source
		dsl  string // how the DSL value quotes it (a profile-1 "…" cannot carry a double quote)
	}{
		{"[]", "\""},
		{"['a', 'b']", "\""},
		{`{a: 1, b: [true, 'x']}`, "\""},
		{`{a: "x"}`, "`"}, // the raw string carries the inner double quote in both profiles
		{"[input.tags, ['const']]", "\""},
	}
	for _, header := range []string{"", "dsl: 2\n\n"} {
		for _, e := range exprs {
			src := header + `schema lit:
  xs: string[]
  j: json

compute shape:
  output: lit
  expr:
    xs: ` + e.dsl + e.text + e.dsl + `
    j: ` + e.dsl + e.text + e.dsl + `

workflow w:
  worktree: none
  sandbox: none
  entry: shape
  shape -> done
`
			pr1 := parser.Parse("lit.bot", src)
			for _, d := range pr1.Diagnostics {
				if d.Severity == parser.SeverityError {
					t.Fatalf("header %q expr %q: original parse error: %s", header, e.text, d.Error())
				}
			}
			unparsed := unparse.Unparse(pr1.File)
			pr2 := parser.Parse("lit.roundtrip.bot", unparsed)
			for _, d := range pr2.Diagnostics {
				if d.Severity == parser.SeverityError {
					t.Fatalf("header %q expr %q: re-parse error: %s\nUnparsed:\n%s", header, e.text, d.Error(), unparsed)
				}
			}
			var got []string
			for _, c := range pr2.File.Computes {
				for _, ce := range c.Expr {
					got = append(got, ce.Expr)
				}
			}
			if len(got) != 2 || got[0] != e.text || got[1] != e.text {
				t.Fatalf("header %q expr %q: round-trip changed the text to %q\nUnparsed:\n%s", header, e.text, got, unparsed)
			}
			// And the re-read text still parses as the same expression.
			if _, err := expr.Parse(got[0]); err != nil {
				t.Fatalf("header %q expr %q: re-read text does not parse: %v", header, e.text, err)
			}
		}
	}
}

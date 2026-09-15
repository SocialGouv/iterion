package unparse

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// A JSON string inside a contract value — a default, a parameter, an
// object key — survives the writer whatever it holds: a quote, a
// backslash, a newline (a raw literal spans lines as one token, so the
// one-value-per-line rule holds), a carriage return or a backtick (which
// turn the file strict). The written text parses, the document reads back
// equal, and Verify agrees — the fill test's identifier-shaped strings
// never meet these.
func TestAJSONStringValueSurvivesTheWriter(t *testing.T) {
	for name, s := range map[string]string{
		"quote":     `a"b`,
		"backslash": `a\b`,
		"newline":   "a\nb",
		"cr":        "a\rb",
		"backtick":  "a`b",
		"all":       "a\"b\\c\nd`e",
	} {
		t.Run(name, func(t *testing.T) {
			def, err := json.Marshal(s)
			if err != nil {
				t.Fatal(err)
			}
			params, err := json.Marshal(map[string]any{"pattern": s, s: 1})
			if err != nil {
				t.Fatal(err)
			}
			optional := false
			f := &ast.File{Contracts: []*ast.ContractDecl{{Name: "c",
				Inputs:   []*ast.PortDecl{{Name: "goal", Type: "string", Required: &optional, Default: def}},
				Criteria: []*ast.CriterionDecl{{Name: "k", Kind: "pattern", Port: "input.goal", Params: params}},
			}}}
			out := Unparse(f)
			back := parser.Parse("x.bot", out)
			for _, d := range back.Diagnostics {
				if d.Severity == parser.SeverityError {
					t.Fatalf("the written text does not parse: %s\n%s", d.Error(), out)
				}
			}
			want, err := ast.MarshalFile(&ast.File{Contracts: f.Contracts})
			if err != nil {
				t.Fatal(err)
			}
			got, err := ast.MarshalFile(&ast.File{Contracts: back.File.Contracts})
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(want, got) {
				t.Fatalf("the value did not survive the writer:\nwant %s\ngot  %s\n%s", want, got, out)
			}
			if err := Verify(f, out); err != nil {
				t.Fatalf("Verify: %v\n%s", err, out)
			}
		})
	}
}

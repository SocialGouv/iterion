package unparse_test

import (
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unparse"
)

const literalWorkflow = "tool t:\n  command: \"true\"\nworkflow w:\n  entry: t\n  t -> done\n"

func TestFloatLiteralKeepsItsDecimalSpellingAndKind(t *testing.T) {
	for _, raw := range []string{"0.0000001", "1000000000000000000000.0", "8.0", "8.00", "0.0", "0008.00"} {
		for _, typ := range []string{"float", "int"} {
			t.Run(raw+"/"+typ, func(t *testing.T) {
				src := "vars:\n  x: " + typ + " = " + raw + "\npresets:\n  sample:\n    x: " + raw + "\n" + literalWorkflow
				p := parser.Parse("float.bot", src)
				if len(p.Diagnostics) != 0 {
					t.Fatal(p.Diagnostics)
				}
				out := unparse.Unparse(p.File)
				if !strings.Contains(out, " = "+raw+"\n") || !strings.Contains(out, "    x: "+raw+"\n") {
					t.Fatalf("spelling changed:\n%s", out)
				}
				if err := unparse.Verify(p.File, out); err != nil {
					t.Fatal(err)
				}
				back := parser.Parse("float.bot", out)
				if back.File.Vars.Fields[0].Default.Kind != ast.LitFloat || back.File.Presets.Entries[0].Values[0].Value.Kind != ast.LitFloat {
					t.Fatal("float came back as another literal kind")
				}
			})
		}
	}
}

func TestFloatLiteralFromEditedASTUsesCurrentValue(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		value     float64
	}{
		{"missing tiny", "", 1e-7}, {"missing huge", "", 1e21}, {"missing integer", "", 8},
		{"smallest", "", math.SmallestNonzeroFloat64}, {"largest", "", math.MaxFloat64},
		{"stale", "1.0", 8}, {"exponent", "1e-7", 1e-7}, {"integer raw", "8", 8},
		{"underscore raw", "1_0.0", 10}, {"source fragment", "8.0\n  y: float = 9.0", 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := parser.Parse("float.bot", "vars:\n  x: float = 1.0\n"+literalWorkflow).File
			f.Vars.Fields[0].Default = &ast.Literal{Kind: ast.LitFloat, Raw: tc.raw, FloatVal: tc.value}
			data, err := ast.MarshalFile(f)
			if err != nil {
				t.Fatal(err)
			}
			f, err = ast.UnmarshalFile(data)
			if err != nil {
				t.Fatal(err)
			}
			out := unparse.Unparse(f)
			if err = unparse.Verify(f, out); err != nil {
				t.Fatal(err)
			}
			p := parser.Parse("float.bot", out)
			if len(p.Diagnostics) != 0 {
				t.Fatal(p.Diagnostics)
			}
			lit := p.File.Vars.Fields[0].Default
			if lit.Kind != ast.LitFloat || lit.FloatVal != tc.value {
				t.Fatalf("got %#v, want float %s", lit, fmt.Sprint(tc.value))
			}
			if len(p.File.Vars.Fields) != 1 {
				t.Fatal("raw text injected an extra field")
			}
		})
	}
}

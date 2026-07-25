package unparse

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

const varsEnumRoundtripSrc = `vars:
  mode: string [enum: "autonomous", "interview"] = "autonomous"
  effort: string [enum: "low", "high"]
  plain: string = "x"

prompt sys:
  hi

agent a:
  model: "test"
  system: sys

workflow w:
  entry: a
  a -> done
`

// TestVarsEnumRoundtrip verifies parse → unparse → parse is stable for
// var enum constraints and that the canonical form matches the
// schema-field enum syntax.
func TestVarsEnumRoundtrip(t *testing.T) {
	pr1 := parser.Parse("t.bot", varsEnumRoundtripSrc)
	if len(pr1.Diagnostics) > 0 {
		for _, d := range pr1.Diagnostics {
			t.Logf("first parse diag: %+v", d)
		}
		t.Fatalf("first parse produced diagnostics")
	}
	out1 := Unparse(pr1.File)

	pr2 := parser.Parse("t.bot", out1)
	if len(pr2.Diagnostics) > 0 {
		for _, d := range pr2.Diagnostics {
			t.Logf("second parse diag: %+v", d)
		}
		t.Fatalf("re-parse of unparsed source produced diagnostics:\n%s", out1)
	}
	out2 := Unparse(pr2.File)

	if out1 != out2 {
		t.Fatalf("round-trip drift:\n--- pass 1 ---\n%s\n--- pass 2 ---\n%s", out1, out2)
	}

	// Canonical emission: constraint between type and default.
	expectedSubstrings := []string{
		`mode: string [enum: "autonomous", "interview"] = "autonomous"`,
		`effort: string [enum: "low", "high"]`,
		`plain: string = "x"`,
	}
	for _, want := range expectedSubstrings {
		if !strings.Contains(out1, want) {
			t.Errorf("unparsed source missing %q\n---\n%s", want, out1)
		}
	}

	// The re-parsed AST carries the same enum values.
	vb := pr2.File.Vars
	if vb == nil || len(vb.Fields) != 3 {
		t.Fatalf("re-parsed vars block = %v, want 3 fields", vb)
	}
	if len(vb.Fields[0].EnumValues) != 2 || vb.Fields[0].EnumValues[0] != "autonomous" {
		t.Errorf("re-parsed enum values = %v", vb.Fields[0].EnumValues)
	}
}

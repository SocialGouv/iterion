package ir

import (
	"strings"
	"testing"
)

const enumHead = `schema out:
  s: string [enum: "a", "b"]
  xs: string[] [enum: "a", "b"]
  t: string
  n: int

compute pick:
  output: out
  expr:
`

// A compute field with an enum constraint fed by a static string literal
// outside the enum is named (C183) — the runtime enum arm of checkFieldType
// refuses the value at the node (SCHEMA_VALIDATION). A member literal, an
// expression the compiler cannot fully evaluate, and a shape another check
// owns (a scalar literal into a string[] field fails on the type, not the
// enum) stay silent.
func TestAComputeLiteralOutsideTheEnumIsAWarning(t *testing.T) {
	tail := "\nworkflow w:\n  entry: pick\n  pick -> done\n"
	for name, tc := range map[string]struct {
		exprs string
		warn  bool
	}{
		"literal outside the enum":                 {"    s: \"'zzz'\"\n    t: \"'anything'\"\n    n: \"1\"\n", true},
		"literal inside the enum":                  {"    s: \"'a'\"\n    t: \"'anything'\"\n    n: \"1\"\n", false},
		"the other member":                         {"    s: \"'b'\"\n    t: \"'anything'\"\n    n: \"1\"\n", false},
		"an unconstrained field":                   {"    s: \"'a'\"\n    t: \"'zzz'\"\n    n: \"1\"\n", false},
		"an if() the compiler cannot fold":         {"    s: \"if(true, 'a', 'zzz')\"\n    t: \"'anything'\"\n    n: \"1\"\n", false},
		"a call the compiler cannot fold":          {"    s: \"join(xs, ',')\"\n    t: \"'anything'\"\n    n: \"1\"\n", false},
		"a scalar literal into the string[] field": {"    s: \"'a'\"\n    xs: \"'zzz'\"\n    t: \"'anything'\"\n    n: \"1\"\n", false},
		"a list literal with a non-member":         {"    s: \"'a'\"\n    xs: \"['a', 'zzz']\"\n    t: \"'anything'\"\n    n: \"1\"\n", true},
		"a list literal of members":                {"    s: \"'a'\"\n    xs: \"['a', 'b']\"\n    t: \"'anything'\"\n    n: \"1\"\n", false},
		"a list literal with a non-string element": {"    s: \"'a'\"\n    xs: \"['a', 1]\"\n    t: \"'anything'\"\n    n: \"1\"\n", false},
	} {
		t.Run(name, func(t *testing.T) {
			cr := compileText(t, enumHead+tc.exprs+tail)
			var got *Diagnostic
			for i := range cr.Diagnostics {
				if cr.Diagnostics[i].Code == DiagComputeEnumLiteral {
					got = &cr.Diagnostics[i]
				}
			}
			if tc.warn && (got == nil || got.Severity != SeverityWarning || !strings.Contains(got.Message, `"zzz"`) || got.NodeID != "pick") {
				t.Fatalf("no C183 warning at pick: %+v\n%v", got, cr.Diagnostics)
			}
			if !tc.warn && got != nil {
				t.Fatalf("C183 on a literal that is fine: %s", got.Message)
			}
		})
	}
}

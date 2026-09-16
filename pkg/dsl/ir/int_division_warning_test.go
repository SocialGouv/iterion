package ir

import (
	"strings"
	"testing"
)

const divisionHead = `schema pair:
  a: float
  b: int
  n: int

schema stats:
  ratio: int
  share: float

agent measure:
  model: "m"
  output: pair

compute summarize:
  input: pair
  output: stats
  expr:
`

// A compute field typed int fed by a division that may carry a fraction is
// named (C146) — unless floor()/round() wraps it, both operands are ints,
// or the field is a float.
func TestADivisionIntoAnIntFieldIsAWarning(t *testing.T) {
	tail := "\nworkflow w:\n  worktree: none\n  sandbox: none\n  entry: measure\n  measure -> summarize\n  summarize -> done\n"
	for name, tc := range map[string]struct {
		exprs string
		warn  bool
	}{
		"float over int into an int":     {"    ratio: \"input.a / input.b\"\n    share: \"input.a\"\n", true},
		"wrapped in floor":               {"    ratio: \"floor(input.a / input.b)\"\n    share: \"input.a\"\n", false},
		"wrapped in round":               {"    ratio: \"round(input.a / input.b)\"\n    share: \"input.a\"\n", false},
		"int over int":                   {"    ratio: \"input.n / input.b\"\n    share: \"input.a\"\n", false},
		"division into the float field":  {"    ratio: \"input.n\"\n    share: \"input.a / input.b\"\n", false},
		"division nested in an addition": {"    ratio: \"1 + input.a / input.b\"\n    share: \"input.a\"\n", true},
	} {
		t.Run(name, func(t *testing.T) {
			cr := compileText(t, divisionHead+tc.exprs+tail)
			var got *Diagnostic
			for i := range cr.Diagnostics {
				if cr.Diagnostics[i].Code == DiagIntDivisionUnrounded {
					got = &cr.Diagnostics[i]
				}
			}
			if tc.warn && (got == nil || got.Severity != SeverityWarning || !strings.Contains(got.Message, "floor(") || got.NodeID != "summarize") {
				t.Fatalf("no C146 warning at summarize: %+v\n%v", got, cr.Diagnostics)
			}
			if !tc.warn && got != nil {
				t.Fatalf("C146 on a division that is fine: %s", got.Message)
			}
		})
	}
}

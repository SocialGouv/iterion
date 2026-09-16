package ir

import (
	"strings"
	"testing"
)

const divisionHead = `schema pair:
  a: float
  b: int
  n: int
  ok: bool

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

// A compute field typed int fed by a division with a float operand is
// named (C146) — unless floor()/round() wraps it; ints divide to an int, and
// an operand the compiler cannot type (a function's result, an arithmetic)
// is not held against the author — the warning is true when it speaks.
func TestADivisionIntoAnIntFieldIsAWarning(t *testing.T) {
	tail := "\nworkflow w:\n  worktree: none\n  sandbox: none\n  entry: measure\n  measure -> summarize\n  summarize -> done\n"
	for name, tc := range map[string]struct {
		exprs string
		warn  bool
	}{
		"float over int into an int":       {"    ratio: \"input.a / input.b\"\n    share: \"input.a\"\n", true},
		"wrapped in floor":                 {"    ratio: \"floor(input.a / input.b)\"\n    share: \"input.a\"\n", false},
		"wrapped in round":                 {"    ratio: \"round(input.a / input.b)\"\n    share: \"input.a\"\n", false},
		"int over int":                     {"    ratio: \"input.n / input.b\"\n    share: \"input.a\"\n", false},
		"division into the float field":    {"    ratio: \"input.n\"\n    share: \"input.a / input.b\"\n", false},
		"division nested in an addition":   {"    ratio: \"1 + input.a / input.b\"\n    share: \"input.a\"\n", true},
		"a float literal":                  {"    ratio: \"input.n / 2.5\"\n    share: \"input.a\"\n", true},
		"floor() as an operand":            {"    ratio: \"floor(input.a) / input.b\"\n    share: \"input.a\"\n", false},
		"an int arithmetic as an operand":  {"    ratio: \"(input.n + input.b) / input.b\"\n    share: \"input.a\"\n", false},
		"an if() the compiler cannot type": {"    ratio: \"if(input.ok, input.n, input.b) / input.b\"\n    share: \"input.a\"\n", false},
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

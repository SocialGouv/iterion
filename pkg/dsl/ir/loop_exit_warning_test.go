package ir

import (
	"strings"
	"testing"
)

const loopHead = `schema verdict:
  ok: bool
  n: int
  items: string[]

agent check:
  model: "m"
  output: verdict

agent survey:
  model: "m"
  output: verdict

judge assess:
  model: "m"
  output: verdict

workflow w:
  worktree: none
  sandbox: none
  entry: check
  budget:
    max_iterations: 20
  check -> assess
`

// A bounded loop edge with no exit once the loop is spent is named (C145):
// the runtime declines the back-edge at its cap whatever its condition, and
// the edges left must cover every outcome — a bare edge, an else, or
// conditionals exhaustive on their own. An unbounded loop is not named: its
// fuel is its ceiling.
func TestALoopWithNoExitAtItsCapIsAWarning(t *testing.T) {
	for name, tc := range map[string]struct {
		edges string
		warn  bool
	}{
		"conditional back-edge, only the other polarity left": {"  assess -> check when not ok as retry(2)\n  assess -> done when ok\n", true},
		"bare back-edge alone":                                {"  assess -> check as retry(2)\n", true},
		"bare back-edge and a bare exit":                      {"  assess -> check as retry(2)\n  assess -> done\n", false},
		"conditional back-edge and a bare exit":               {"  assess -> check when not ok as retry(2)\n  assess -> done when ok\n  assess -> done\n", false},
		"conditional back-edge and an else":                   {"  assess -> check when not ok as retry(2)\n  assess -> done else\n", false},
		"no loop at all":                                      {"  assess -> done\n", false},
		"unbounded loop, its fuel the ceiling":                {"  assess -> check when not ok as retry(unbounded 5)\n  assess -> done when ok\n", false},
		"unbounded loop declared first, bounded second":       {"  assess -> check when ok as slow(unbounded 5)\n  assess -> check when not ok as retry(2)\n", true},
		"foreach edge beside the bounded loop":                {"  assess -> check when not ok as retry(2)\n  assess -> survey as foreach scan(item in \"{{outputs.check.items}}\")\n  survey -> done\n", true},
		"an expression edge the compiler cannot evaluate":     {"  assess -> check when \"outputs.assess.n < 3\" as retry(3)\n  assess -> done when \"outputs.assess.n >= 3\"\n", false},
	} {
		t.Run(name, func(t *testing.T) {
			cr := compileText(t, loopHead+tc.edges)
			var got *Diagnostic
			for i := range cr.Diagnostics {
				if cr.Diagnostics[i].Code == DiagLoopNoExit {
					got = &cr.Diagnostics[i]
				}
			}
			if tc.warn && (got == nil || got.Severity != SeverityWarning || !strings.Contains(got.Message, "LOOP_EXHAUSTED") || got.NodeID != "assess") {
				t.Fatalf("no C145 warning at assess: %+v\n%v", got, cr.Diagnostics)
			}
			// The warning names the BOUNDED loop — never the unbounded one
			// declared beside it, whose fuel is its ceiling.
			if tc.warn && !strings.Contains(got.Message, `loop "retry"`) {
				t.Fatalf("C145 does not name the bounded loop: %s", got.Message)
			}
			if !tc.warn && got != nil {
				t.Fatalf("C145 on a loop that has its exit: %s", got.Message)
			}
		})
	}
}

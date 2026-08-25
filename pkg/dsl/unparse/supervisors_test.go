package unparse

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

const supervisorsRoundtripSrc = `supervisor watchdog:
  watches: [implement, fix]
  model: "anthropic/claude-opus-4-8"
  system: watchdog_policy
  cooldown: "45s"
  max_evals: 12
  monitors: ["event_type=tool_error,tool_name=Bash", "text_contains=impossible"]

prompt watchdog_policy:
  Intervene if the implementer edits files outside src/.

agent implement:
  model: "anthropic/claude-opus-4-8"

agent fix:
  model: "anthropic/claude-opus-4-8"

workflow w:
  entry: implement
  implement -> fix
  fix -> done
`

func TestSupervisorsRoundtrip(t *testing.T) {
	pr1 := parser.Parse("t.bot", supervisorsRoundtripSrc)
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

	for _, want := range []string{
		"supervisor watchdog:",
		"watches: [implement, fix]",
		`model: "anthropic/claude-opus-4-8"`,
		"system: watchdog_policy",
		`cooldown: "45s"`,
		"max_evals: 12",
		`monitors: ["event_type=tool_error,tool_name=Bash", "text_contains=impossible"]`,
	} {
		if !strings.Contains(out1, want) {
			t.Errorf("unparsed source missing %q\n---\n%s", want, out1)
		}
	}
}

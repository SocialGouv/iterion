package unparse

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

const declaredToolsBot = `prompt sys:
  """s"""

prompt usr:
  """u"""

judge reviewer:
  model: "openai/gpt-5.5"
  backend: "claw"
  system: sys
  user: usr
  tools: []

workflow w:
  entry: reviewer
  reviewer -> done
`

// `iterion fmt` used to DELETE the `tools: []` line, which is how the rule
// bots/evolve's gpt reviewer documented stopped being carried by any file at
// all. Reddens on the mutation that writes the line only for a non-empty
// list.
func TestFmtWritesTheEmptyToolDeclarationBackInsteadOfDroppingIt(t *testing.T) {
	pr := parser.Parse("b.bot", declaredToolsBot)
	out := Unparse(pr.File)
	if !strings.Contains(out, "tools: []") {
		t.Fatalf("fmt dropped the declaration:\n%s", out)
	}
	// …and it is a FIXED POINT: formatting the formatted text changes nothing.
	again := Unparse(parser.Parse("b.bot", out).File)
	if again != out {
		t.Fatalf("fmt is not idempotent on `tools: []`:\nfirst:\n%s\nsecond:\n%s", out, again)
	}
	// An undeclared list still writes nothing.
	without := strings.Replace(declaredToolsBot, "  tools: []\n", "", 1)
	if got := Unparse(parser.Parse("b.bot", without).File); strings.Contains(got, "tools:") {
		t.Fatalf("fmt invented a tools: line on an undeclared node:\n%s", got)
	}
}

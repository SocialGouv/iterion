package unparse

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser/parsertest"
)

// The subagents reserve (CM-13) round-trips: parse → unparse keeps every
// key verbatim, and the re-parse re-compiles to the same reserve. A dropped
// key would silently keep the agent tool withheld from a node whose author
// sized it — a reserve is opt-in BY its declaration, so losing one line
// loses the capability.
func TestTheSubagentsReserveRoundTrips(t *testing.T) {
	src := `
agent a:
  model: "m"
  backend: "claw"
  codemode:
    subagents_tokens: 100000
    subagents_tokens_per_agent: 40000
    subagents_concurrent: 2
    subagents_steps_per_agent: 8

workflow w:
  entry: a
  a -> done
`
	pr1 := parsertest.Parse("sub.bot", src)
	for _, d := range pr1.Diagnostics {
		if d.Severity.String() == "error" {
			t.Fatalf("original parse error: %s", d.Error())
		}
	}
	un := Unparse(pr1.File)
	for _, want := range []string{
		"subagents_tokens: 100000",
		"subagents_tokens_per_agent: 40000",
		"subagents_concurrent: 2",
		"subagents_steps_per_agent: 8",
	} {
		if !strings.Contains(un, want) {
			t.Fatalf("unparse lost %q:\n%s", want, un)
		}
	}
	pr2 := parsertest.Parse("sub.bot.roundtrip", un)
	for _, d := range pr2.Diagnostics {
		if d.Severity.String() == "error" {
			t.Fatalf("re-parse error: %s\nUnparsed:\n%s", d.Error(), un)
		}
	}
	res := ir.Compile(pr2.File)
	for _, d := range res.Diagnostics {
		if d.Severity == ir.SeverityError {
			t.Fatalf("re-compile error: %s\nUnparsed:\n%s", d.Error(), un)
		}
	}
	agent := res.Workflow.Nodes["a"].(*ir.AgentNode)
	if agent.CodemodeSubagents == nil ||
		agent.CodemodeSubagents.Tokens != 100000 ||
		agent.CodemodeSubagents.TokensPerAgent != 40000 ||
		agent.CodemodeSubagents.Concurrent != 2 ||
		agent.CodemodeSubagents.StepsPerAgent != 8 {
		t.Fatalf("the re-parsed reserve differs: %+v", agent.CodemodeSubagents)
	}
}

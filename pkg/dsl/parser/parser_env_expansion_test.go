package parser_test

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// `env_expansion:` is a plain identifier property of a tool node (ADR-130):
// both values parse, and a tool that does not write the line carries the
// empty string, which the engine reads as `braced` — today's behaviour.
func TestParseToolEnvExpansion(t *testing.T) {
	for _, want := range []string{"braced", "none"} {
		src := "tool emit:\n  command: \"echo ${NAME:-d}\"\n  env_expansion: " + want + "\n"
		res := parser.Parse("test.bot", src)
		assertNoDiags(t, res)
		if len(res.File.Tools) != 1 {
			t.Fatalf("expected 1 tool, got %d", len(res.File.Tools))
		}
		assertEq(t, "EnvExpansion", res.File.Tools[0].EnvExpansion, want)
	}

	plain := parser.Parse("test.bot", "tool plain:\n  command: \"echo ok\"\n")
	assertNoDiags(t, plain)
	assertEq(t, "EnvExpansion of a plain tool", plain.File.Tools[0].EnvExpansion, "")
}

// The word is not reserved: `env_expansion` reads as a property only inside a
// tool node, so a bot that already names a node or a schema field with it
// keeps parsing. Reserving it as a lexer keyword is what would break them
// (the ADR-044 / ADR-098 convention for the same reason).
func TestEnvExpansionIsNotAReservedWord(t *testing.T) {
	src := `schema env_expansion:
  env_expansion: string

tool env_expansion:
  command: "echo ok"
  output: env_expansion
`
	res := parser.Parse("test.bot", src)
	assertNoDiags(t, res)
	if len(res.File.Schemas) != 1 || len(res.File.Tools) != 1 {
		t.Fatalf("expected the schema and the tool to parse, got %d schema(s) and %d tool(s)", len(res.File.Schemas), len(res.File.Tools))
	}
	assertEq(t, "the tool's name", res.File.Tools[0].Name, "env_expansion")
	assertEq(t, "the tool's output schema", res.File.Tools[0].Output, "env_expansion")
}

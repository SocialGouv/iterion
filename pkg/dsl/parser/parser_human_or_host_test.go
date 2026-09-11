package parser_test

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unparse"
)

const humanOrHostSrc = `schema chat_out:
  message: string
  host_event: json

human chat:
  output: chat_out
  interaction: human_or_host

workflow t:
  entry: chat

  chat -> done
`

// human_or_host must survive .bot → AST → .bot. A mode the unparser dropped
// would silently turn a declared standby gate back into a plain human pause
// on the next studio save — and a gate that stops declaring its second input
// source is exactly the invisibility this mode exists to remove.
func TestInteractionHumanOrHost_ParsesAndSurvivesUnparse(t *testing.T) {
	res := parser.Parse("test.bot", humanOrHostSrc)
	assertNoDiags(t, res)
	var found bool
	for _, n := range res.File.Humans {
		found = true
		if n.Interaction != ast.InteractionHumanOrHost {
			t.Fatalf("interaction = %v, want human_or_host", n.Interaction)
		}
	}
	if !found {
		t.Fatal("no human node parsed")
	}

	out := unparse.Unparse(res.File)
	if !strings.Contains(out, "interaction: human_or_host") {
		t.Fatalf("unparse dropped the mode:\n%s", out)
	}
}

// The parser's error message enumerates the accepted modes; a new mode that
// is accepted but unlisted sends the next author hunting.
func TestInteractionModeError_ListsHumanOrHost(t *testing.T) {
	res := parser.Parse("test.bot", strings.Replace(humanOrHostSrc, "human_or_host", "nonsense", 1))
	var msg string
	for _, d := range res.Diagnostics {
		msg += d.Message + "\n"
	}
	if !strings.Contains(msg, "human_or_host") {
		t.Fatalf("mode error does not list human_or_host: %q", msg)
	}
}

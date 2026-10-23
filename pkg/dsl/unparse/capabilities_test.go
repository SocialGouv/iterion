package unparse_test

import (
	"github.com/SocialGouv/iterion/pkg/dsl/parser/parsertest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/unparse"
)

func TestUnparseCapabilities_RoundTrip(t *testing.T) {
	src := `agent po:
  model: "gpt-4"
  capabilities: [board.create, board.move, board.read]

judge qa:
  model: "gpt-4"
  capabilities: [board.read]

workflow w:
  entry: po
  capabilities: [board.read]

  po -> qa
  qa -> done
`
	res := parsertest.Parse("test.bot", src)
	if len(res.Diagnostics) != 0 {
		t.Fatalf("unexpected diagnostics: %+v", res.Diagnostics)
	}

	out := unparse.Unparse(res.File)

	for _, want := range []string{
		"capabilities: [board.create, board.move, board.read]",
		"capabilities: [board.read]",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("unparse output missing %q:\n%s", want, out)
		}
	}

	// Reparse the unparsed output; must produce equivalent capability lists.
	res2 := parsertest.Parse("test.bot", out)
	if len(res2.Diagnostics) != 0 {
		t.Fatalf("reparse diagnostics: %+v\nsource:\n%s", res2.Diagnostics, out)
	}
	a := res2.File.Agents[0]
	if got := strings.Join(a.Capabilities, ","); got != "board.create,board.move,board.read" {
		t.Fatalf("agent Capabilities after round-trip = %q", got)
	}
	j := res2.File.Judges[0]
	if got := strings.Join(j.Capabilities, ","); got != "board.read" {
		t.Fatalf("judge Capabilities after round-trip = %q", got)
	}
	wf := res2.File.Workflows[0]
	if got := strings.Join(wf.Capabilities, ","); got != "board.read" {
		t.Fatalf("workflow Capabilities after round-trip = %q", got)
	}
}

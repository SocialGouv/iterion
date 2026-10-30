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

// `capabilities: []` is a DECLARED empty grant (#1801): the author saying
// "this node holds no capability" — overriding the workflow's list. Dropping
// the line on unparse would turn the declared empty grant back into an
// inheritance. Only an absent (nil) list writes nothing.
func TestUnparseCapabilities_DeclaredEmptyRoundTrip(t *testing.T) {
	src := `agent po:
  model: "gpt-4"
  capabilities: []

agent dev:
  model: "gpt-4"

workflow w:
  entry: po
  capabilities: []

  po -> dev
  dev -> done
`
	res := parsertest.Parse("test.bot", src)
	if len(res.Diagnostics) != 0 {
		t.Fatalf("unexpected diagnostics: %+v", res.Diagnostics)
	}
	if res.File.Agents[0].Capabilities == nil || res.File.Agents[1].Capabilities != nil {
		t.Fatalf("precondition: declared = %#v, absent = %#v", res.File.Agents[0].Capabilities, res.File.Agents[1].Capabilities)
	}

	out := unparse.Unparse(res.File)

	if n := strings.Count(out, "capabilities: []"); n != 2 {
		t.Fatalf("unparse must write the two declared empty lists back, found %d:\n%s", n, out)
	}
	if strings.Contains(out, "capabilities: [") && !strings.Contains(out, "capabilities: []") {
		t.Fatalf("unparse invented a list:\n%s", out)
	}

	// Reparse: the declared empty lists survive, the absent one stays nil.
	res2 := parsertest.Parse("test.bot", out)
	if len(res2.Diagnostics) != 0 {
		t.Fatalf("reparse diagnostics: %+v\nsource:\n%s", res2.Diagnostics, out)
	}
	if got := res2.File.Agents[0].Capabilities; got == nil || len(got) != 0 {
		t.Fatalf("declared empty agent Capabilities after round-trip = %#v", got)
	}
	if got := res2.File.Agents[1].Capabilities; got != nil {
		t.Fatalf("absent agent Capabilities after round-trip = %#v, want nil", got)
	}
	if got := res2.File.Workflows[0].Capabilities; got == nil || len(got) != 0 {
		t.Fatalf("declared empty workflow Capabilities after round-trip = %#v", got)
	}
}

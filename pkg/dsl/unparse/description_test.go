package unparse_test

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unparse"
)

// parse → unparse → re-parse must preserve the optional node
// `description:` on every node kind.
func TestUnparseRoundtripNodeDescriptions(t *testing.T) {
	src := `agent a:
  description: "Agent label"
  model: "m"

judge j:
  description: "Judge label"
  model: "m"

router r:
  description: "Router label"
  mode: fan_out_all

human h:
  description: "Human label"

tool tl:
  description: "Tool label"
  command: "true"

compute c:
  description: "Compute label"
  expr:
    ok: "true"

subbot sb:
  description: "Subbot label"
  source: "child.bot"

emit e:
  description: "Emit label"
  event: "ready"

wait w:
  description: "Wait label"
  event: "ready"
  timeout: "30s"
`
	pr1 := parser.Parse("desc.bot", src)
	if pr1.File == nil {
		t.Fatal("parse returned nil File")
	}
	for _, d := range pr1.Diagnostics {
		if d.Severity == parser.SeverityError {
			t.Fatalf("parse error: %s", d.Error())
		}
	}

	unparsed := unparse.Unparse(pr1.File)
	pr2 := parser.Parse("desc.roundtrip.bot", unparsed)
	if pr2.File == nil {
		t.Fatalf("re-parse returned nil File\nUnparsed:\n%s", unparsed)
	}
	for _, d := range pr2.Diagnostics {
		if d.Severity == parser.SeverityError {
			t.Fatalf("re-parse error: %s\nUnparsed:\n%s", d.Error(), unparsed)
		}
	}

	f := pr2.File
	checks := []struct {
		kind, got, want string
	}{
		{"agent", f.Agents[0].Description, "Agent label"},
		{"judge", f.Judges[0].Description, "Judge label"},
		{"router", f.Routers[0].Description, "Router label"},
		{"human", f.Humans[0].Description, "Human label"},
		{"tool", f.Tools[0].Description, "Tool label"},
		{"compute", f.Computes[0].Description, "Compute label"},
		{"subbot", f.Subbots[0].Description, "Subbot label"},
		{"emit", f.Emits[0].Description, "Emit label"},
		{"wait", f.Waits[0].Description, "Wait label"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s Description = %q, want %q\nUnparsed:\n%s", c.kind, c.got, c.want, unparsed)
		}
	}
}

package parser_test

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// The optional `description:` field is accepted on every node kind and
// lands on the matching decl (a human-readable label for the run console).
func TestNodeDescriptions(t *testing.T) {
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
	res := parser.Parse("test.bot", src)
	assertNoDiags(t, res)

	f := res.File
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
			t.Errorf("%s Description = %q, want %q", c.kind, c.got, c.want)
		}
	}
}

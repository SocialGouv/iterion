package ir_test

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// The optional node `description:` must survive compilation onto the IR
// node (BaseNode.Description, exposed via NodeDescription) for every
// node kind in the graph.
func TestCompileCarriesNodeDescriptions(t *testing.T) {
	src := `schema flags:
  ok: bool

agent a:
  description: "Agent label"
  model: "m"

tool tl:
  description: "Tool label"
  command: "true"

compute c:
  description: "Compute label"
  output: flags
  expr:
    ok: "true"

human h:
  description: "Human label"

emit e:
  description: "Emit label"
  event: "ready"

wait w:
  description: "Wait label"
  event: "ready"
  timeout: "30s"

workflow main:
  entry: a
  a -> tl
  tl -> c
  c -> h
  h -> e
  e -> w
  w -> done
`
	pr := parser.Parse("desc.bot", src)
	if pr.File == nil {
		t.Fatal("parse returned nil File")
	}
	for _, d := range pr.Diagnostics {
		if d.Severity == parser.SeverityError {
			t.Fatalf("parse error: %s", d.Error())
		}
	}
	cr := ir.Compile(pr.File)
	if cr.HasErrors() {
		for _, d := range cr.Diagnostics {
			if d.Severity == ir.SeverityError {
				t.Fatalf("compile error: %s", d.Error())
			}
		}
	}

	want := map[string]string{
		"a":  "Agent label",
		"tl": "Tool label",
		"c":  "Compute label",
		"h":  "Human label",
		"e":  "Emit label",
		"w":  "Wait label",
	}
	for id, wantDesc := range want {
		n, ok := cr.Workflow.Nodes[id]
		if !ok {
			t.Errorf("node %q missing from workflow", id)
			continue
		}
		d, ok := n.(interface{ NodeDescription() string })
		if !ok {
			t.Errorf("node %q (%T) does not expose NodeDescription", id, n)
			continue
		}
		if got := d.NodeDescription(); got != wantDesc {
			t.Errorf("node %q Description = %q, want %q", id, got, wantDesc)
		}
	}
	// Terminal nodes carry no description.
	if d, ok := cr.Workflow.Nodes["done"].(interface{ NodeDescription() string }); ok && d.NodeDescription() != "" {
		t.Errorf("done node Description = %q, want empty", d.NodeDescription())
	}
}

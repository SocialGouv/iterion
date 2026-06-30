package unparse_test

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unparse"
)

// TestUnparseForeach_RoundTrip pins parse → unparse → re-parse stability for the
// `as foreach <name>(item in "<collection>")` edge clause.
func TestUnparseForeach_RoundTrip(t *testing.T) {
	src := `tool start:
  command: "true"

tool proc:
  command: "true"

workflow w:
  entry: start

  start -> proc
  proc -> proc as foreach scan(item in "{{outputs.start.items}}")
  proc -> done
`
	res := parser.Parse("test.bot", src)
	if len(res.Diagnostics) != 0 {
		t.Fatalf("unexpected diagnostics: %+v", res.Diagnostics)
	}
	out := unparse.Unparse(res.File)
	if !strings.Contains(out, `as foreach scan(item in "{{outputs.start.items}}")`) {
		t.Fatalf("unparse lost the foreach clause:\n%s", out)
	}
	res2 := parser.Parse("test.bot", out)
	if len(res2.Diagnostics) != 0 {
		t.Fatalf("reparse diagnostics: %+v\nsource:\n%s", res2.Diagnostics, out)
	}
	var found bool
	for _, e := range res2.File.Workflows[0].Edges {
		if e.Foreach != nil && e.Foreach.Name == "scan" && e.Foreach.Item == "item" &&
			e.Foreach.Collection == "{{outputs.start.items}}" {
			found = true
		}
	}
	if !found {
		t.Fatalf("foreach clause not preserved through round-trip:\n%s", out)
	}
}

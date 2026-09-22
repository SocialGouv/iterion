package parser

import (
	"strconv"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)

// An edge line read on its own reads as it reads inside a workflow body:
// the same edges, the same clauses. Its strings are read with the standard
// escapes in every profile — the author document's form — which a
// profile-1 body opts into by its directive.
func TestParseEdgeLineReadsWhatAWorkflowBodyReads(t *testing.T) {
	for _, line := range []string{
		"a -> b",
		"r1.look -> done when ok",
		"a -> b when not ok as fix(3)",
		`a -> b when "loop.fix.iteration > 2"`,
		`a -> b when "a\nb"`,
		"a -> b else",
		"a -> b as scan(unbounded 5)",
		`a -> b as foreach scan(item in "{{outputs.list.items}}")`,
		`a -> b with { k: "v", n: 3, ok: true }`,
		"a -> b -> c when ok",
		`a -> b with { note: "a: \"b\"" }`,
		"a -> b # a trailing comment is a comment",
	} {
		for _, profile := range []struct {
			n    int
			head string
		}{
			{2, "dsl: 2\n\n"},
			{1, "## strict-escape: on\n\n"},
		} {
			t.Run(line+" in profile "+strconv.Itoa(profile.n), func(t *testing.T) {
				edges, diags := ParseEdgeLine(profile.n, line)
				if len(diags) > 0 {
					t.Fatalf("diagnostics on a valid line: %v", diags)
				}
				pr := Parse("w.bot", profile.head+"workflow w:\n  entry: a\n  "+line+"\n")
				if len(pr.Diagnostics) > 0 {
					t.Fatalf("the workflow body refuses the line: %v", pr.Diagnostics)
				}
				want := pr.File.Workflows[0].Edges
				if len(edges) != len(want) {
					t.Fatalf("%d edges alone, %d in the body", len(edges), len(want))
				}
				for i := range edges {
					if edgeSummary(edges[i]) != edgeSummary(want[i]) {
						t.Errorf("edge %d differs:\nalone: %s\nbody:  %s", i, edgeSummary(edges[i]), edgeSummary(want[i]))
					}
				}
			})
		}
	}
}

// A line that is not exactly one edge line is refused by name, and nothing
// past the line is read.
func TestParseEdgeLineRefusesWhatIsNotOneEdgeLine(t *testing.T) {
	for _, tc := range []struct{ line, want string }{
		{"entry: a", "expected an edge line"},
		{"a", "expected an edge line"},
		{"-> b", "expected an edge line"},
		{"", "empty line"},
		{"   ", "empty line"},
		{"a -> b\nb -> c", "one line"},
		{"a -> b when ok when done", "duplicate 'when'"},
		{"a -> b when x -> c", "clause before a further arrow"},
		{"a -> b extra", "ends after its clauses"},
		{"  a -> b", "not with indentation"},
		{"a -> b when x && y", "quoted expression"},
	} {
		t.Run(tc.line, func(t *testing.T) {
			_, diags := ParseEdgeLine(2, tc.line)
			if len(diags) == 0 {
				t.Fatalf("no diagnostic for %q", tc.line)
			}
			var msgs []string
			for _, d := range diags {
				msgs = append(msgs, d.Message)
			}
			if !strings.Contains(strings.Join(msgs, " | "), tc.want) {
				t.Errorf("diagnostics %q do not say %q", msgs, tc.want)
			}
		})
	}
}

// edgeSummary renders an edge without its positions, for a comparison.
func edgeSummary(e *ast.Edge) string {
	var b strings.Builder
	b.WriteString(e.From + " -> " + e.To)
	if e.IsElse {
		b.WriteString(" else")
	}
	if e.When != nil {
		b.WriteString(" when{" + e.When.Condition + "|" + e.When.Expr + "}")
		if e.When.Negated {
			b.WriteString("!")
		}
	}
	if e.Loop != nil {
		b.WriteString(" loop{" + e.Loop.Name + "|" + e.Loop.MaxIterationsExpr + "}")
		if e.Loop.Unbounded {
			b.WriteString("u")
		}
		b.WriteString(strings.Repeat("i", e.Loop.MaxIterations) + strings.Repeat("f", e.Loop.FuelCap))
	}
	if e.Foreach != nil {
		b.WriteString(" foreach{" + e.Foreach.Name + "|" + e.Foreach.Item + "|" + e.Foreach.Collection + "}")
	}
	for _, w := range e.With {
		b.WriteString(" with{" + w.Key + "=" + w.Value + "}")
	}
	return b.String()
}

package unparse_test

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unparse"
)

// Under profile 2 every value has a quoted form — standard escapes, no
// directive, no switch of the whole file — so each string shape the v1
// writer had to choose a form for round-trips through one quoting.
func TestUnparseKeepsEveryStringShapeInProfileTwo(t *testing.T) {
	values := []string{
		"plain",
		"with 'single' quotes",
		`a literal \n backslash sequence`,
		`a trailing backslash \`,
		`say "hi"`,
		"two\nlines",
		"tab\there",
		"printf %s \"line1\nline2\"\necho second",
		"uses `backticks` only",
		"backticks `and` \"quotes\"",
		"backticks `and`\nnewlines",
		"backticks `and` a \\ backslash",
		"unicode — é ✓",
		"trailing newline\n",
		"\nleading newline",
		"  leading spaces",
	}
	for _, v := range values {
		t.Run(v, func(t *testing.T) {
			f := &ast.File{
				Profile: 2,
				Tools:   []*ast.ToolNodeDecl{{Name: "t", Command: v, Description: v}},
				Workflows: []*ast.WorkflowDecl{{
					Name: "w", Entry: "t",
					Edges: []*ast.Edge{{From: "t", To: "done", With: []*ast.WithEntry{{Key: "note", Value: v}}}},
				}},
			}
			text := unparse.Unparse(f)
			if !strings.HasPrefix(text, "dsl: 2\n") {
				t.Fatalf("no header:\n%s", text)
			}
			if strings.Contains(text, "strict-escape") {
				t.Fatalf("a profile-2 file carries no directive:\n%s", text)
			}
			pr := parser.Parse("shape.bot", text)
			for _, d := range pr.Diagnostics {
				if d.Severity == parser.SeverityError {
					t.Fatalf("does not parse back: %s\n%s", d.Error(), text)
				}
			}
			if got := pr.File.Tools[0].Command; got != v {
				t.Errorf("command: %q came back as %q\n%s", v, got, text)
			}
			if got := pr.File.Tools[0].Description; got != v {
				t.Errorf("description: %q came back as %q\n%s", v, got, text)
			}
			if got := pr.File.Workflows[0].Edges[0].With[0].Value; got != v {
				t.Errorf("with value: %q came back as %q\n%s", v, got, text)
			}
			if err := unparse.Verify(f, text); err != nil {
				t.Errorf("Verify: %v\n%s", err, text)
			}
		})
	}
}

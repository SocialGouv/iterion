package unparse

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

func TestScratchKebabArtifactLabel(t *testing.T) {
	f := &ast.File{
		Computes: []*ast.ComputeDecl{{
			Name:           "c",
			ArtifactLabels: []string{"review-ledger"},
			Publish:        "led",
			Expr:           []*ast.ComputeExpr{{Key: "x", Expr: `"a"`}},
		}},
		Workflows: []*ast.WorkflowDecl{{Name: "w", Entry: "c", Edges: []*ast.Edge{{From: "c", To: "done"}}}},
	}
	src := Unparse(f)
	t.Logf("SRC:\n%s", src)
	if err := Verify(f, src); err != nil {
		t.Logf("VERIFY FAILED: %v", err)
	} else {
		t.Logf("VERIFY OK")
	}
	res := parser.Parse("x.bot", src)
	t.Logf("diags=%v", res.Diagnostics)
	if res.File != nil && len(res.File.Computes) > 0 {
		t.Logf("reparsed labels=%v", res.File.Computes[0].ArtifactLabels)
	}
}

func TestScratchEmptyWorkflow(t *testing.T) {
	f := &ast.File{Workflows: []*ast.WorkflowDecl{{Name: "w"}}}
	src := Unparse(f)
	t.Logf("SRC:%q", src)
	if err := Verify(f, src); err != nil {
		t.Logf("VERIFY FAILED: %v", err)
	} else {
		t.Logf("VERIFY OK")
	}
}

func TestScratchCommentAfterEmptyDecl(t *testing.T) {
	src := "schema s:\n\n## keep me\nworkflow w:\n  entry: a\n"
	res := parser.Parse("x.bot", src)
	t.Logf("diags=%v", res.Diagnostics)
	if res.File != nil {
		t.Logf("schemas=%d comments=%d", len(res.File.Schemas), len(res.File.Comments))
		for _, c := range res.File.Comments {
			t.Logf("  comment=%q", c.Text)
		}
	}
}

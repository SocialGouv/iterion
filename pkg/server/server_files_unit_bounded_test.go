package server

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/expr"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
)

// citeStoredMainWorkflow rewrites a diagnostic the loader recorded — after
// the recording doors — so the rewrite is bounded where it lands (#2801): a
// workflow name of a megabyte must not ride the rewritten message whole.
func TestCiteStoredMainWorkflowBoundsTheRewrite(t *testing.T) {
	long := strings.Repeat("w", 1<<20)
	stored := &unit.Unit{
		Main: "main.bot",
		Files: []unit.File{{
			Rel: "main.bot",
			AST: &ast.File{Workflows: []*ast.WorkflowDecl{{Name: long, Span: ast.Span{Start: ast.Pos{File: "main.bot", Line: 3, Column: 1}}}}},
		}},
	}
	probe := &unit.Unit{
		Merged: &ast.File{Workflows: []*ast.WorkflowDecl{{Name: long}, {Name: long}}},
		Diagnostics: []parser.Diagnostic{{
			Code: parser.DiagDuplicateDecl, Severity: parser.SeverityError,
			File: "main.bot", Line: 1, Column: 1,
			Message: "a unit has one workflow: \"stub\" here and \"stub\" at lib/f.bot:1",
		}},
	}
	citeStoredMainWorkflow(stored, probe)
	d := probe.Diagnostics[0]
	if len(d.Message) > expr.MaxDiagnosticText+len("…") {
		t.Fatalf("the rewrite carried the name whole: %d bytes, want bounded to %d of content", len(d.Message), expr.MaxDiagnosticText)
	}
	if !strings.Contains(d.Message, "a unit has one workflow:") {
		t.Errorf("the rewrite still says what it rewrites: %.120s", d.Message)
	}
	if !strings.Contains(d.Message, "main.bot:3") {
		t.Errorf("the rewrite still cites the stored main's position: %.120s", d.Message[len(d.Message)-120:])
	}
}

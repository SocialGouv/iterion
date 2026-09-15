package ir

import (
	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"strings"
	"testing"
)

const loopCapExpressionSource = `vars:
  max_passes: int = 3
schema out:
  ok: bool
compute work:
  output: out
  expr:
    ok: "true"
workflow w:
  worktree: none
  entry: work
  work -> work as retry("vars.max_passes - 1")
  work -> done
`

func TestLoopCapExpressionCompilesThroughTransport(t *testing.T) {
	pr := parser.Parse("loop-cap.bot", loopCapExpressionSource)
	for _, d := range pr.Diagnostics {
		if d.Severity == parser.SeverityError {
			t.Fatal(d.Error())
		}
	}
	raw, err := ast.MarshalFile(pr.File)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := ast.UnmarshalFile(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []*ast.File{pr.File, restored} {
		cr := Compile(f)
		if cr.HasErrors() {
			t.Fatal(cr.Diagnostics)
		}
		if got := cr.Workflow.Loops["retry"].MaxIterationsExpr; got != "vars.max_passes - 1" {
			t.Fatalf("loop cap=%q", got)
		}
	}
}

func TestLoopCapRejectsDefiniteTypeAndReferenceErrors(t *testing.T) {
	for _, cap := range []string{"vars.missing", "outputs.missing.cap", "outputs.work.missing", "'three'", "true", "1.5", "loop.retry.max", "{{vars.missing}}"} {
		t.Run(cap, func(t *testing.T) {
			cr := compileFile(t, strings.Replace(loopCapExpressionSource, "vars.max_passes - 1", cap, 1))
			if !cr.HasErrors() {
				t.Fatal("invalid cap compiled")
			}
			found := false
			for _, d := range cr.Diagnostics {
				if d.Severity == SeverityError && strings.Contains(d.Message, `loop "retry" cap`) {
					found = true
				}
			}
			if !found {
				t.Fatalf("diagnostic does not name the loop and cap: %+v", cr.Diagnostics)
			}
		})
	}
	src := strings.Replace(loopCapExpressionSource, "max_passes: int = 3", `max_passes: string = "3"`, 1)
	if cr := compileFile(t, src); !cr.HasErrors() {
		t.Fatal("string variable in arithmetic cap compiled")
	}
}

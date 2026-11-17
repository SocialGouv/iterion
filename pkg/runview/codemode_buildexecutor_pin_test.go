package runview

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"testing"
)

// The pod's second carrier of the launch-time codemode override (CM-5e,
// #3345): BuildExecutor's option list, where ExecutorSpec.Codemode /
// CodemodeBounds become the model executor's override fields. This is a
// SOURCE guard, not a behavioural one, because the behavioural reach is the
// executor's private fields — no seam exposes them. The two options missing
// here compile fine and leave every launch override dead at the door: the
// operator's `off` runs the authored value, the direction the v33 bump
// exists to reject (found by the adversarial round — a mutant emptying both
// lines stayed green across every suite).
func TestBuildExecutorForwardsTheCodemodeOverride(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("executor.go")
	if err != nil {
		t.Fatalf("read executor.go: %v", err)
	}
	f, err := parser.ParseFile(token.NewFileSet(), "executor.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var mode, bounds bool
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel == nil {
			return true
		}
		switch sel.Sel.Name {
		case "WithCodemodeOverride":
			if len(call.Args) == 1 && fieldName(call.Args[0]) == "Codemode" {
				mode = true
			}
		case "WithCodemodeBoundsOverride":
			if len(call.Args) == 1 && fieldName(call.Args[0]) == "CodemodeBounds" {
				bounds = true
			}
		}
		return true
	})
	if !mode || !bounds {
		t.Errorf("BuildExecutor's options carry WithCodemodeOverride=%v WithCodemodeBoundsOverride=%v — a dropped line kills the launch override at the door", mode, bounds)
	}
}

// fieldName names the struct field a selector argument reads, or "" — the
// pin wants the SPEC's field, not a literal (a literal there forwards a
// constant and the override dies all the same).
func fieldName(arg ast.Expr) string {
	sel, ok := arg.(*ast.SelectorExpr)
	if !ok || sel.Sel == nil {
		return ""
	}
	return sel.Sel.Name
}

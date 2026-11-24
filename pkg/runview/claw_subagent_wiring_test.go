package runview

// The production host wires the subagent runner (#1940). BuildExecutor keeps
// the registry it fills private — there is no reader between this file and
// what a run calls — so the wiring is asserted on the SOURCE of the one site
// that builds tool.ClawDefaults, and the behaviour of what it wires is
// asserted where it can be driven (pkg/backend/model's subagent witnesses,
// pkg/backend/tool's registry ones).

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// TestTheProductionHostWiresASubagentRunner: with nothing assigned to
// ClawDefaults.Subagent, claw's own executor answers {"status": "running"}
// and starts nothing — a node that delegated would report on work nobody did.
// Red when the assignment is dropped from executor.go.
func TestTheProductionHostWiresASubagentRunner(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "executor.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse executor.go: %v", err)
	}

	var call *ast.CallExpr
	ast.Inspect(file, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
			return true
		}
		sel, ok := assign.Lhs[0].(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Subagent" {
			return true
		}
		if ident, ok := sel.X.(*ast.Ident); !ok || ident.Name != "clawDefaults" {
			return true
		}
		if c, ok := assign.Rhs[0].(*ast.CallExpr); ok {
			call = c
		}
		return false
	})
	if call == nil {
		t.Fatal("executor.go assigns nothing to clawDefaults.Subagent — the `agent` tool registers and refuses every call, " +
			"and claw's own executor would report a subagent nobody launched")
	}
	fn, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || fn.Sel.Name != "NewSubagentRunner" {
		t.Fatalf("clawDefaults.Subagent is built by something other than model.NewSubagentRunner: %T", call.Fun)
	}
	if n := len(call.Args); n != 5 {
		t.Fatalf("model.NewSubagentRunner called with %d arguments, want 5", n)
	}
	// The host names NO default model: the model is a property of the NODE,
	// this closure is built once per run, and the runner resolves the parent
	// node's model per call (model.ParentLoop). A hard-coded id here would
	// bill every delegation of every workflow to one model.
	lit, ok := call.Args[4].(*ast.BasicLit)
	if !ok || strings.Trim(lit.Value, `"`) != "" {
		t.Errorf("the host passes %s as the default model; want the empty string so the parent node's model serves",
			types(call.Args[4]))
	}
}

// types renders an expression for an error message without importing a
// printer: the node kind is enough to tell an identifier from a literal.
func types(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.BasicLit:
		return v.Value
	case *ast.Ident:
		return v.Name
	default:
		return "a non-literal expression"
	}
}

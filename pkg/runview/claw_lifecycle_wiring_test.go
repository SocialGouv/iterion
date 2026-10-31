package runview

// The runview half of integrations.claw-lifecycle-hooks. BuildExecutor keeps
// the backend registry it fills private — claw_subagent_wiring_test.go reads
// the same seam — so the handoff is asserted on the SOURCE of the one site
// that builds the claw backend: the default lifecycle runner (the PreToolUse
// safety hook that refuses dangerous commands) must be built HERE and ride
// BOTH the claw backend's options (WithBackendLifecycleHooks → the loop's
// opts.Hooks, where a Block short-circuits the tool's Execute) and the
// executor's own (WithLifecycleHooks). The behaviour of what is wired is
// witnessed where it can be driven
// (TestClawBackend_DefaultLifecycleHookBlocksInsideTheLoop,
// pkg/backend/model/claw_backend_lifecycle_hooks_test.go); this witness
// carries the production handoff alone.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

func TestTheProductionHostHandsTheDefaultLifecycleHooksToTheClawBackend(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "executor.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse executor.go: %v", err)
	}

	// qualified renders pkg.Sel for a selector, a slice type as its element's
	// rendering prefixed with [], and an identifier as its name. Recursive,
	// hence declared before it is assigned.
	var qualified func(ast.Expr) string
	qualified = func(e ast.Expr) string {
		switch v := e.(type) {
		case *ast.SelectorExpr:
			if x, ok := v.X.(*ast.Ident); ok {
				return x.Name + "." + v.Sel.Name
			}
		case *ast.ArrayType:
			return "[]" + qualified(v.Elt)
		case *ast.Ident:
			return v.Name
		}
		return ""
	}

	// The default lifecycle runner is built into a local variable.
	built := ""
	ast.Inspect(file, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
			return true
		}
		name, ok := assign.Lhs[0].(*ast.Ident)
		if !ok {
			return true
		}
		call, ok := assign.Rhs[0].(*ast.CallExpr)
		if ok && qualified(call.Fun) == "model.NewDefaultLifecycleHooks" {
			built = name.Name
			return false
		}
		return true
	})
	if built == "" {
		t.Fatal("executor.go never builds model.NewDefaultLifecycleHooks — " +
			"the PreToolUse safety hook disappears from every in-process claw run")
	}

	// Only the option slice NewClawBackend consumes reaches the loop's
	// opts.Hooks — not the executor's own option list, not a sibling slice.
	// optionCalls remembers, per slice variable, every option call written
	// into it (the composite literal AND later appends), so moving the entry
	// between the two shapes of the same slice cannot slip past.
	optionCalls := map[string][]*ast.CallExpr{}
	var backendArgs []ast.Expr
	ast.Inspect(file, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.AssignStmt:
			if len(v.Lhs) != 1 || len(v.Rhs) != 1 {
				return true
			}
			name, ok := v.Lhs[0].(*ast.Ident)
			if !ok {
				return true
			}
			lit, ok := v.Rhs[0].(*ast.CompositeLit)
			if !ok || qualified(lit.Type) != "[]model.ClawBackendOption" {
				return true
			}
			for _, elt := range lit.Elts {
				if call, ok := elt.(*ast.CallExpr); ok {
					optionCalls[name.Name] = append(optionCalls[name.Name], call)
				}
			}
		case *ast.CallExpr:
			if qualified(v.Fun) == "model.NewClawBackend" {
				backendArgs = v.Args
			}
			if id, ok := v.Fun.(*ast.Ident); ok && id.Name == "append" && len(v.Args) > 0 {
				if base, ok := v.Args[0].(*ast.Ident); ok {
					for _, a := range v.Args[1:] {
						if call, ok := a.(*ast.CallExpr); ok {
							optionCalls[base.Name] = append(optionCalls[base.Name], call)
						}
					}
				}
			}
		}
		return true
	})

	handedToBackend := false
	for _, arg := range backendArgs {
		id, ok := arg.(*ast.Ident)
		if !ok {
			continue
		}
		for _, call := range optionCalls[id.Name] {
			if qualified(call.Fun) != "model.WithBackendLifecycleHooks" || len(call.Args) != 1 {
				continue
			}
			if id, ok := call.Args[0].(*ast.Ident); ok && id.Name == built {
				handedToBackend = true
			}
		}
	}
	if !handedToBackend {
		t.Fatalf("the backend options NewClawBackend consumes carry no model.WithBackendLifecycleHooks(%s) — "+
			"the default lifecycle runner never reaches the claw loop, and a dangerous tool call runs instead of being refused", built)
	}

	handedToExecutor := false
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || qualified(call.Fun) != "model.WithLifecycleHooks" || len(call.Args) != 1 {
			return true
		}
		if id, ok := call.Args[0].(*ast.Ident); ok && id.Name == built {
			handedToExecutor = true
		}
		return true
	})
	if !handedToExecutor {
		t.Fatalf("executor.go never hands %s to model.WithLifecycleHooks — "+
			"the executor's own half of the lifecycle wiring disappears", built)
	}
}

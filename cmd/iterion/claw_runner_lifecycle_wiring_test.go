package main

// The sandboxed half of integrations.claw-lifecycle-hooks. The in-container
// runner builds the ONE ClawBackend a sandboxed node executes on, and the
// default lifecycle runner — the PreToolUse safety hook that refuses
// dangerous commands — must ride its options the way it rides the host
// backend's (runview/executor.go, guarded there by
// claw_lifecycle_wiring_test.go). Without it a workspace whose settings
// carry no hooks executes with opts.Hooks nil and the refusal the row
// promises never fires in the container. The runner's provider registry
// resolves from the environment, so the backend cannot be driven here; the
// wiring is asserted on the source, and the refusal behaviour of what is
// wired is witnessed on the real backend
// (TestClawBackend_DefaultLifecycleHookBlocksInsideTheLoop,
// pkg/backend/model/claw_backend_lifecycle_hooks_test.go).

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

func TestTheSandboxedRunnerHandsTheDefaultLifecycleHooksToItsBackend(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "claw_runner.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse claw_runner.go: %v", err)
	}

	// qualified renders pkg.Sel for a qualified call, an identifier's name
	// for a bare one.
	qualified := func(e ast.Expr) string {
		if sel, ok := e.(*ast.SelectorExpr); ok {
			if x, ok := sel.X.(*ast.Ident); ok {
				return x.Name + "." + sel.Sel.Name
			}
		}
		if id, ok := e.(*ast.Ident); ok {
			return id.Name
		}
		return ""
	}

	var backend *ast.CallExpr
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if ok && qualified(call.Fun) == "model.NewClawBackend" {
			backend = call
		}
		return true
	})
	if backend == nil {
		t.Fatal("claw_runner.go never builds model.NewClawBackend — the sandboxed runner has no backend at all")
	}

	for _, arg := range backend.Args {
		call, ok := arg.(*ast.CallExpr)
		if !ok || qualified(call.Fun) != "model.WithBackendLifecycleHooks" || len(call.Args) != 1 {
			continue
		}
		if inner, ok := call.Args[0].(*ast.CallExpr); ok && qualified(inner.Fun) == "model.NewDefaultLifecycleHooks" {
			return
		}
	}
	t.Fatal("the sandboxed runner's ClawBackend carries no model.NewDefaultLifecycleHooks through " +
		"model.WithBackendLifecycleHooks — a workspace without settings hooks executes with opts.Hooks nil, " +
		"and a dangerous tool call runs inside the container instead of being refused")
}

package serversurface

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/internal/sweepwalk"
)

// The cloud bootstrap needs Mongo/NATS; pin its actual server.New argument
// separately from the constructor/API integration tests
// (pkg/server/maintenance_regression_test.go). Removing the production
// MaintenanceSettings field must fail even if Config-based tests still pass.
// It reads cmd/iterion/server.go, so it lives here and not in pkg/server.
func TestFenceCloudBootstrapWiresMaintenance(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(sweepwalk.ModuleRoot(t), "cmd", "iterion", "server.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	wired := false
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		fn, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || fn.Sel.Name != "New" {
			return true
		}
		pkg, ok := fn.X.(*ast.Ident)
		if !ok || pkg.Name != "server" {
			return true
		}
		cfg, ok := call.Args[0].(*ast.CompositeLit)
		if !ok {
			return true
		}
		for _, elt := range cfg.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok || key.Name != "MaintenanceSettings" {
				continue
			}
			v, ok := kv.Value.(*ast.SelectorExpr)
			if !ok || v.Sel.Name != "maintenance" {
				continue
			}
			obj, ok := v.X.(*ast.Ident)
			wired = ok && obj.Name == "stores"
		}
		return true
	})
	if !wired {
		t.Fatal("cloud server.New must receive MaintenanceSettings: stores.maintenance")
	}
}

package tool

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// buildFullClawRegistry hand-mirrors the host's wiring: RegisterClawAll plus
// the families a host registers beside it. A hand-mirror of a list that lives
// somewhere else drifts, and this one drifting is invisible — the
// exhaustiveness guard would simply stop covering a family, stay green, and
// the first unclassified execution-capable tool would reach a sandboxed node
// as a silent PlacementRefused.
//
// So the list is derived instead: every RegisterClaw* family this package
// exports that RegisterClawAll does NOT call is a family the host wires
// itself, and therefore one this helper must wire too.
func TestTheFullRegistryWiresEveryFamilyTheHostHasTo(t *testing.T) {
	exempt := map[string]string{
		// A thin wrapper over RegisterClawBuiltinsWithEnv, which
		// RegisterClawAll calls: the same tools, no extra names.
		"RegisterClawBuiltins": "wrapper over RegisterClawBuiltinsWithEnv",
		// Registers ONE tool the caller supplies. There is no fixed name for
		// a placement table to classify, and the classification of whatever
		// is passed is the caller's to make.
		"RegisterClawTool": "registers a single caller-supplied tool, not a family",
	}

	exported := map[string]bool{}
	bodies := map[string]*ast.FuncDecl{}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		file, perr := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", name, perr)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil {
				continue
			}
			if strings.HasSuffix(name, "_test.go") {
				bodies[fn.Name.Name] = fn
				continue
			}
			if strings.HasPrefix(fn.Name.Name, "RegisterClaw") {
				exported[fn.Name.Name] = true
				bodies[fn.Name.Name] = fn
			}
		}
	}

	// The premise: if these two disappear or get renamed, the guard below is
	// vacuous and would stay green forever.
	for _, required := range []string{"RegisterClawAll", "buildFullClawRegistry"} {
		if bodies[required] == nil {
			t.Fatalf("premise broken: %s was not found in this package, so this guard proves nothing", required)
		}
	}

	calledByAll := registerClawCallsIn(bodies["RegisterClawAll"])
	calledByHelper := registerClawCallsIn(bodies["buildFullClawRegistry"])

	var missing []string
	for name := range exported {
		if name == "RegisterClawAll" || calledByAll[name] || calledByHelper[name] {
			continue
		}
		if _, ok := exempt[name]; ok {
			continue
		}
		missing = append(missing, name)
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("RegisterClawAll does not call %v, so a host wires them itself — and buildFullClawRegistry does "+
			"not either, so TestSandboxPlacementCoversEveryRegisteredClawTool never sees their tools. Wire them "+
			"in the helper, or add them to this test's exempt map with a reason.", missing)
	}
}

// registerClawCallsIn returns every RegisterClaw* function the body calls,
// in either spelling (bare, or through a package selector).
func registerClawCallsIn(fn *ast.FuncDecl) map[string]bool {
	out := map[string]bool{}
	if fn == nil || fn.Body == nil {
		return out
	}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch f := call.Fun.(type) {
		case *ast.Ident:
			if strings.HasPrefix(f.Name, "RegisterClaw") {
				out[f.Name] = true
			}
		case *ast.SelectorExpr:
			if strings.HasPrefix(f.Sel.Name, "RegisterClaw") {
				out[f.Sel.Name] = true
			}
		}
		return true
	})
	return out
}

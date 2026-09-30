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
		// Registers whatever ToolDef the caller hands it; no fixed name for
		// a placement table to classify.
		"RegisterBuiltin": "registers a single caller-supplied tool",
		"RegisterMCP":     "registers a discovered MCP tool; the shape rule classifies those",
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
			if isToolRegistrar(fn) {
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

	calledByAll := registrarCallsIn(bodies["RegisterClawAll"], exported)
	calledByHelper := registrarCallsIn(bodies["buildFullClawRegistry"], exported)

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

// isToolRegistrar reports whether fn is one of this package's tool-family
// registrars, by SIGNATURE: exported, first parameter `*Registry`, single
// `error` result.
//
// Not by name prefix. "RegisterClaw*" is a convention this package already
// breaks twice — RegisterAskUser and RegisterAsyncAsk — so a guard keyed on
// it goes blind to the next registrar that does not adopt it, which is
// exactly how a new execution-capable tool reaches a sandboxed node with no
// placement and gets silently refused.
func isToolRegistrar(fn *ast.FuncDecl) bool {
	if fn.Recv != nil || !fn.Name.IsExported() || fn.Type.Params == nil {
		return false
	}
	if len(fn.Type.Params.List) == 0 {
		return false
	}
	star, ok := fn.Type.Params.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	id, ok := star.X.(*ast.Ident)
	if !ok || id.Name != "Registry" {
		return false
	}
	res := fn.Type.Results
	if res == nil || len(res.List) != 1 {
		return false
	}
	errID, ok := res.List[0].Type.(*ast.Ident)
	return ok && errID.Name == "error"
}

// registrarCallsIn returns every function the body calls whose name this
// package exports as a registrar.
func registrarCallsIn(fn *ast.FuncDecl, registrars map[string]bool) map[string]bool {
	out := map[string]bool{}
	if fn == nil || fn.Body == nil {
		return out
	}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		var name string
		switch f := call.Fun.(type) {
		case *ast.Ident:
			name = f.Name
		case *ast.SelectorExpr:
			name = f.Sel.Name
		}
		if registrars[name] {
			out[name] = true
		}
		return true
	})
	return out
}

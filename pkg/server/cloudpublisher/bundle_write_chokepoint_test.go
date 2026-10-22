package cloudpublisher

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// bundleKeyMaps are the RunBundle fields only setAPIKey/clearAPIKey may
// write: a key written anywhere else travels without the URL and the
// fingerprint it is one credential with.
var bundleKeyMaps = map[string]bool{"APIKeys": true, "PinnedAPIKeys": true, "APIBaseURLs": true, "PinnedAPIBaseURLs": true, "APIKeyFingerprints": true}

// readOnlyHelpers take a key map as an argument and only read it.
var readOnlyHelpers = map[string]bool{
	"len": true, "providersWithoutKey": true, "withoutFundedProviders": true,
	// derivePoolWants/already-filled audit: acquireFromPool reads the
	// already-filled pinned keys to derive the pool's wants and never
	// writes them (its writes go through Acquire's lease, not the map).
	"acquireFromPool": true, "fundedAPIKeySlots": true, "doorServes": true, "familyHolderName": true,
}

// Every write into the bundle's key maps goes through the chokepoint
// (bundle_keys.go). The scan refuses, anywhere else in the package, every
// use of these fields that is not a READ: an index assignment, a delete, a
// whole-map assignment, an alias (`m := b.APIKeys`), an address taken, a
// map builtin or helper call (`clear`, `maps.Copy`, …), a composite-literal
// value, a range key/value target. A read is: `len(...)`, an index READ
// (`b.APIKeys[p]` on the right-hand side or in a condition), `range` over
// the map, an argument to a read-only helper named in readOnlyHelpers.
// Blind spot, documented: a `RunBundle{...}` literal that puts keys in the
// maps (lane 0 initialises empty ones; a test fixture may fill them).
func TestEveryBundleKeyWriteGoesThroughTheChokepoint(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var offenders []string
	files := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || name == "bundle_keys.go" {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		files++
		offenders = append(offenders, keyMapWrites(fset, f)...)
	}
	if files < 10 {
		t.Fatalf("scanned %d files — the scan is not reading the package", files)
	}
	if len(offenders) > 0 {
		t.Fatalf("uses of the bundle's key maps outside bundle_keys.go that are not reads — route writes through setAPIKey/clearAPIKey:\n  %s", strings.Join(offenders, "\n  "))
	}
}

// keyMapWrites walks the file with its parent chain and reports every
// occurrence of a key-map selector whose context is not a known read.
func keyMapWrites(fset *token.FileSet, f *ast.File) []string {
	var out []string
	var stack []ast.Node
	ast.Inspect(f, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, n)
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || !bundleKeyMaps[sel.Sel.Name] {
			return true
		}
		// The selector itself must name a field of a bundle value, not a
		// package-level identifier that happens to share the name.
		if _, isIdent := sel.X.(*ast.Ident); !isIdent {
			if _, isSel := sel.X.(*ast.SelectorExpr); !isSel {
				if _, isStar := sel.X.(*ast.StarExpr); !isStar {
					if _, isParen := sel.X.(*ast.ParenExpr); !isParen {
						return true
					}
				}
			}
		}
		parent := stack[len(stack)-2]
		var grand ast.Node
		if len(stack) >= 3 {
			grand = stack[len(stack)-3]
		}
		if !isKeyMapRead(sel, parent, grand) {
			out = append(out, fset.Position(sel.Pos()).String()+" uses "+sel.Sel.Name+" as a write")
		}
		return true
	})
	return out
}

// isKeyMapRead decides whether sel, under parent (and grand), is a read.
func isKeyMapRead(sel *ast.SelectorExpr, parent, grand ast.Node) bool {
	switch p := parent.(type) {
	case *ast.IndexExpr:
		if p.X != sel {
			return true // the map appears in the index position: a read
		}
		// b.M[k] — a read unless it is an assignment target.
		if as, ok := grand.(*ast.AssignStmt); ok {
			for _, lhs := range as.Lhs {
				if lhs == p {
					return false
				}
			}
		}
		if rs, ok := grand.(*ast.RangeStmt); ok && (rs.Key == p || rs.Value == p) {
			return false
		}
		if u, ok := grand.(*ast.UnaryExpr); ok && u.Op == token.AND {
			return false
		}
		if inc, ok := grand.(*ast.IncDecStmt); ok && inc.X == p {
			return false
		}
		return true
	case *ast.CallExpr:
		switch fn := p.Fun.(type) {
		case *ast.Ident:
			return readOnlyHelpers[fn.Name]
		case *ast.SelectorExpr:
			return readOnlyHelpers[fn.Sel.Name] // a method read (p.acquireFromPool, …)
		}
		return false // delete, clear, maps.Copy, any other call
	case *ast.RangeStmt:
		return p.X == sel
	case *ast.BinaryExpr:
		return true // comparisons read
	case *ast.AssignStmt, *ast.ValueSpec, *ast.KeyValueExpr, *ast.CompositeLit, *ast.UnaryExpr, *ast.ReturnStmt:
		return false // alias, address, literal value, handed out
	}
	return false
}

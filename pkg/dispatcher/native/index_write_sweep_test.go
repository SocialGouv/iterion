package native

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// chokePointFile holds setIndexLocked and dropIndexLocked themselves, so
// it is the one file allowed to touch the map's entries directly.
const chokePointFile = "store_index.go"

// The dirty-set merge of Reconcile rests on one invariant: every write to
// s.index goes through setIndexLocked or dropIndexLocked, which mark the
// id while a scan is in flight. A mutator that wrote the map directly
// would be reverted by the next overflow rebuild without a test noticing,
// so the invariant is held by this sweep over the package's source, the
// way bot_resolver_sweep_test.go holds the role-bot constants.
//
// The sweep reads the package's SYNTAX, not its spelling. A regexp over
// the text got this wrong in both directions, and both were reachable:
//
//	`s\.index\[[^\]]+\]\s*=` swallows the first `=` of `==`, so
//	`if s.index[id] == nil {` — a pure READ — failed the build with
//	"index written outside setIndexLocked", which is exactly wrong; and
//
//	it anchors on a receiver spelled `s`, so `x.index[id] = iss` or
//	`delete(g.s.index, id)` walked straight past it — and the package
//	already reads through a second chain (store_issues.go's g.s.index),
//	so that rename is one method away.
//
// Walking for an IndexExpr over a `.index` selector has neither problem,
// and it closes the third hole for free: an ALIAS of the whole map
// (`idx := s.index; idx[k] = v`) writes it with no dirty mark at all.
// The one deliberate whole-map write — swapIndexLocked's `s.index =
// fresh` — is not an entry write and not an alias; the swap IS the choke
// point for that operation, and it holds mu.
func TestEveryIndexWriteGoesThroughTheChokePoint(t *testing.T) {
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var offenders []string
	report := func(n ast.Node, what string) {
		offenders = append(offenders, fset.Position(n.Pos()).String()+": "+what)
	}
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") || name == chokePointFile {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.AssignStmt:
				for _, lhs := range x.Lhs {
					if isIndexEntry(lhs) {
						report(lhs, "writes an index entry directly — call setIndexLocked")
					}
				}
				for _, rhs := range x.Rhs {
					if isIndexMap(rhs) {
						report(rhs, "aliases the index map — the alias can be written without a dirty mark")
					}
				}
			case *ast.CallExpr:
				if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "delete" &&
					len(x.Args) == 2 && isIndexMap(x.Args[0]) {
					report(x, "deletes an index entry directly — call dropIndexLocked")
				}
			}
			return true
		})
	}
	if len(offenders) > 0 {
		t.Fatalf("the index was written outside setIndexLocked/dropIndexLocked (the write would escape the rebuild's dirty set and be reverted by the next scan):\n  %s", strings.Join(offenders, "\n  "))
	}
}

// isIndexMap reports whether e is a `….index` selector — the map itself.
func isIndexMap(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "index"
}

// isIndexEntry reports whether e is `….index[…]` — one entry of it.
func isIndexEntry(e ast.Expr) bool {
	ix, ok := e.(*ast.IndexExpr)
	return ok && isIndexMap(ix.X)
}

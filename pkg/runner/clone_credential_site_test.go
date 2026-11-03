package runner

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// The grant gate guards one call site, in prepareRepoWorkspace, that no
// test can drive: the clone fetches a public repository through the SSRF
// guard. Deleting the three lines of the call leaves every behaviour test
// green — so the call is pinned here, in the source: it sits in the
// function that materialises the credential, AFTER the install that
// writes it (the gate judges the store that exists), its verdict is the
// if's WHOLE condition (an `err != nil && <never>` leaves the gate dead
// code at its one site), and its error is RETURNED, bare or wrapped (a
// judged-and-ignored refusal is the silent no-op this guards against).
func TestTheCloneCredentialGateIsCalledWhereTheCredentialIsInstalled(t *testing.T) {
	const file = "loop_gitws.go"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	var body *ast.BlockStmt
	for _, decl := range f.Decls {
		if fd, ok := decl.(*ast.FuncDecl); ok && fd.Name.Name == "prepareRepoWorkspace" && fd.Body != nil {
			body = fd.Body
		}
	}
	if body == nil {
		t.Fatalf("%s declares no prepareRepoWorkspace: the clone credential is installed elsewhere now, move this pin with it", file)
	}
	installed, gated, whole, returned := 0, 0, false, false
	ast.Inspect(body, func(n ast.Node) bool {
		ifStmt, ok := n.(*ast.IfStmt)
		if !ok {
			return true
		}
		switch methodCalledIn(ifStmt.Init) {
		case "installGitCredentialStore":
			installed = fset.Position(ifStmt.Pos()).Line
		case "refuseUngrantedCloneCredential":
			gated = fset.Position(ifStmt.Pos()).Line
			whole = isErrNotNil(ifStmt.Cond)
			returned = bodyReturns(ifStmt.Body)
		}
		return true
	})
	if installed == 0 {
		t.Fatalf("prepareRepoWorkspace no longer installs the credential store through `if err := r.installGitCredentialStore(…)`: move this pin to where the credential is written")
	}
	if gated == 0 {
		t.Fatalf("prepareRepoWorkspace installs the clone credential (%s:%d) and never judges its grants: the `if err := r.refuseUngrantedCloneCredential(…); err != nil { return … err }` call is gone, and a `sinks:` declaration that withholds `file` is served again", file, installed)
	}
	if gated < installed {
		t.Errorf("the grant gate (%s:%d) runs before the credential store is installed (%s:%d): it judges a file that does not exist yet", file, gated, file, installed)
	}
	if !whole {
		t.Errorf("the grant gate's verdict (%s:%d) is not the if's whole condition `err != nil`: a conjunct that never holds leaves the gate dead code at its only call site", file, gated)
	}
	if !returned {
		t.Errorf("the grant gate's refusal (%s:%d) is not returned: a judged-and-ignored refusal serves the credential anyway", file, gated)
	}
}

// isErrNotNil reports whether cond is exactly `err != nil`.
func isErrNotNil(cond ast.Expr) bool {
	bin, ok := cond.(*ast.BinaryExpr)
	if !ok || bin.Op != token.NEQ {
		return false
	}
	x, xok := bin.X.(*ast.Ident)
	y, yok := bin.Y.(*ast.Ident)
	return xok && yok && x.Name == "err" && y.Name == "nil"
}

// methodCalledIn names the method of `err := r.<method>(…)` when init is
// that shape, "" otherwise.
func methodCalledIn(init ast.Stmt) string {
	as, ok := init.(*ast.AssignStmt)
	if !ok || len(as.Rhs) != 1 {
		return ""
	}
	call, ok := as.Rhs[0].(*ast.CallExpr)
	if !ok {
		return ""
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	return sel.Sel.Name
}

// bodyReturns reports whether the block's statements include a return
// that carries the judged error — bare, or wrapped the way the install's
// own refusal is (`fmt.Errorf("…: %w", err)` keeps the typed cause
// reachable through errors.As, which is how the delivery path reads it).
func bodyReturns(block *ast.BlockStmt) bool {
	for _, st := range block.List {
		ret, ok := st.(*ast.ReturnStmt)
		if !ok {
			continue
		}
		for _, res := range ret.Results {
			if carriesErr(res) {
				return true
			}
		}
	}
	return false
}

// carriesErr reports whether the identifier `err` appears anywhere in the
// expression.
func carriesErr(expr ast.Expr) bool {
	found := false
	ast.Inspect(expr, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Name == "err" {
			found = true
		}
		return !found
	})
	return found
}

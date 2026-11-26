package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// The forge refresh worker only refreshes tokens expiring within Lead, on a
// fixed ticker: with Lead at or below the tick period, a 1h installation
// token can expire between two ticks, the refresh phase-locks onto the
// expiry instant, and every run launched just before that instant is sealed
// a token with seconds of life (see the constants' comment for the incident
// this encodes).
func TestForgeRefreshLeadExceedsTickPeriod(t *testing.T) {
	if forgeRefreshLead <= forgeRefreshTick {
		t.Fatalf("forgeRefreshLead (%v) must exceed forgeRefreshTick (%v): a lead at or below the tick period lets a token expire between two sweeps and phase-locks its refresh onto the expiry instant", forgeRefreshLead, forgeRefreshTick)
	}
}

// assertArmedAtBoot fails unless (*Server).ListenAndServe calls the
// receiver's method, with no argument, as a statement of its own body,
// ahead of any return at that level: the boot path then reaches it on every
// start past the superseded and recovery-passive returns, which sit inside
// their own branches. Read through go/parser rather than as text, so the
// shapes a text search accepts and boot never runs fail here — the call in a
// block comment, behind an `if false`, in a closure, in another function of
// the file, or after the body has returned. A boot-time arming moved into a
// helper must move this witness with it.
func assertArmedAtBoot(t *testing.T, method string) {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "server_lifecycle.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse server_lifecycle.go: %v", err)
	}
	var listen *ast.FuncDecl
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Name.Name != "ListenAndServe" || fd.Recv == nil || len(fd.Recv.List) != 1 || len(fd.Recv.List[0].Names) != 1 || fd.Body == nil {
			continue
		}
		if star, ok := fd.Recv.List[0].Type.(*ast.StarExpr); ok {
			if id, ok := star.X.(*ast.Ident); ok && id.Name == "Server" {
				listen = fd
			}
		}
	}
	if listen == nil {
		t.Fatal("(*Server).ListenAndServe was not found in server_lifecycle.go: this witness would pass on a function it never read")
	}
	recv := listen.Recv.List[0].Names[0].Name
	for _, stmt := range listen.Body.List {
		if _, ok := stmt.(*ast.ReturnStmt); ok {
			break
		}
		expr, ok := stmt.(*ast.ExprStmt)
		if !ok {
			continue
		}
		call, ok := expr.X.(*ast.CallExpr)
		if !ok || len(call.Args) != 0 {
			continue
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != method {
			continue
		}
		if id, ok := sel.X.(*ast.Ident); ok && id.Name == recv {
			return
		}
	}
	t.Errorf("ListenAndServe does not call %s.%s() as a statement of its own body before it returns — the boot path never arms it", recv, method)
}

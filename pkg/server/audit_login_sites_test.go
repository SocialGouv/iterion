package server

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

// loginMintingMethods lists the exported methods of *auth.Service that
// resolve a sign-in: those whose results carry a LoginResult (a session) or
// a DesktopLogin (a session deferred to the ticket's redemption). Read from
// the source of pkg/auth, so a new login path joins the list by existing.
// The value says whether the method mints a LoginResult.
func loginMintingMethods(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, decl := range parsedFuncDecls(t, filepath.Join("..", "auth")) {
		if decl.Recv == nil || !decl.Name.IsExported() || decl.Type.Results == nil {
			continue
		}
		if recv := receiverTypeName(decl); recv != "Service" {
			continue
		}
		for _, res := range decl.Type.Results.List {
			switch astTypeName(res.Type) {
			case "LoginResult":
				out[decl.Name.Name] = true
			case "DesktopLogin":
				if _, ok := out[decl.Name.Name]; !ok {
					out[decl.Name.Name] = false
				}
			}
		}
	}
	return out
}

// loginSitesWithoutAudit is the one login path that mints a session and is
// NOT audited, by decision (ADR-157): a refresh runs at every access-token
// expiry, and the session lineage already records it.
var loginSitesWithoutAudit = map[string]string{
	"Refresh": "a refresh is not a sign-in; the session lineage records it",
}

// TestEverySessionMintingHandlerIsAudited is the choke point of the sign-in
// audit: every function of this package that calls a session-minting method
// of the auth service writes auth.login.failed on the refusal and — where
// the call mints the session itself — auth.login.succeeded on the success.
// A ninth handler that mints a session without the two calls turns this red.
func TestEverySessionMintingHandlerIsAudited(t *testing.T) {
	minting := loginMintingMethods(t)
	for _, want := range []string{
		"Login", "Register", "ChangePasswordPending", "ChangePassword", "ConfirmPasswordReset",
		"RedeemDesktopLogin", "LoginWithExternal", "LoginWithExternalForOrg",
		"LoginWithExternalDesktop", "LoginWithExternalForOrgDesktop", "Refresh",
	} {
		if _, ok := minting[want]; !ok {
			t.Fatalf("auth.Service.%s is not read as a session-minting method: the source scan is broken", want)
		}
	}
	for name := range loginSitesWithoutAudit {
		if _, ok := minting[name]; !ok {
			t.Fatalf("the exemption of auth.Service.%s names a method that mints no session", name)
		}
		delete(minting, name)
	}

	var scanned []string
	var problems []string
	for _, decl := range parsedFuncDecls(t, ".") {
		if decl.Body == nil {
			continue
		}
		calls := map[string]bool{}
		audits := map[string]bool{}
		ast.Inspect(decl.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch sel.Sel.Name {
			case "auditLoginSucceeded", "auditLoginFailed":
				audits[sel.Sel.Name] = true
			}
			if inner, ok := sel.X.(*ast.SelectorExpr); ok && inner.Sel.Name == "authSvc" {
				calls[sel.Sel.Name] = true
			}
			return true
		})
		mintsSession, resolvesLogin := false, false
		for name := range calls {
			if mints, ok := minting[name]; ok {
				resolvesLogin = true
				mintsSession = mintsSession || mints
			}
		}
		if !resolvesLogin {
			continue
		}
		scanned = append(scanned, decl.Name.Name)
		if !audits["auditLoginFailed"] {
			problems = append(problems, decl.Name.Name+" resolves a sign-in and never writes auth.login.failed")
		}
		if mintsSession && !audits["auditLoginSucceeded"] {
			problems = append(problems, decl.Name.Name+" mints a session and never writes auth.login.succeeded")
		}
	}
	sort.Strings(scanned)
	for _, want := range []string{
		"handleLogin", "handleChangePassword", "handleRegister", "handleOIDCCallback",
		"handleDesktopExchange", "handlePasswordResetConfirm", "handleChangeMyPassword",
	} {
		if sort.SearchStrings(scanned, want) >= len(scanned) || scanned[sort.SearchStrings(scanned, want)] != want {
			t.Errorf("%s was not read as a sign-in site: the source scan is broken (scanned %v)", want, scanned)
		}
	}
	for _, p := range problems {
		t.Error(p)
	}
}

// parsedFuncDecls parses the non-test Go files of dir and answers their
// function declarations.
func parsedFuncDecls(t *testing.T, dir string) []*ast.FuncDecl {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	var out []*ast.FuncDecl
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok {
				out = append(out, fd)
			}
		}
	}
	return out
}

// receiverTypeName is the bare type name of a method's receiver.
func receiverTypeName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return ""
	}
	return astTypeName(fd.Recv.List[0].Type)
}

// astTypeName is the bare identifier of a type expression (pointer stripped).
func astTypeName(expr ast.Expr) string {
	switch x := expr.(type) {
	case *ast.StarExpr:
		return astTypeName(x.X)
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return x.Sel.Name
	}
	return ""
}

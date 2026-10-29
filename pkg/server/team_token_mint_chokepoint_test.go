package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The class the derivation chokepoint closes is defined by the VERB, not by a
// route's name: a team-scoped self-authenticating token exists because one of
// three primitives fabricated it. These tests enumerate the callers of those
// primitives from the SOURCE — pkg/server and pkg/forge, where the
// orchestrator mints the repo's hook token two calls away from the handler —
// and confront that set with what the mux really registers and with the table
// the OpenAPI spec is generated from. A new mint site, or a gate removed from
// an old one, fails here rather than in a review.
//
// The sibling classes keep their own gates and are out of this one: a personal
// access token is MintPAT (pat.MintToken, gated in handleCreatePAT), a
// WebSocket ticket MintWSTicket, a forge installation token is minted from the
// GitHub App's key rather than from the presented credential.
var teamTokenMintVerbs = []string{
	"webhooks.MintToken",
	"configshare.MintToken",
	"CreateInvitation",
}

// mintReachingFuncs answers, keyed by function name (a method's name without
// its receiver), every function of pkg/server and pkg/forge whose body
// reaches one of teamTokenMintVerbs, with the chain that proves it. The call
// graph is name-keyed and therefore an over-approximation: it demands a gate
// on more than the minimum, never on less, which is the direction a guard
// must err in.
func mintReachingFuncs(t *testing.T) map[string]string {
	t.Helper()
	calls := map[string]map[string]bool{}
	for _, dir := range []string{".", "../forge"} {
		for name, body := range parsedFuncCalls(t, dir) {
			if calls[name] == nil {
				calls[name] = map[string]bool{}
			}
			for callee := range body {
				calls[name][callee] = true
			}
		}
	}
	reaching := map[string]string{}
	for _, verb := range teamTokenMintVerbs {
		for name, callees := range calls {
			if callees[verb] {
				reaching[name] = verb
			}
		}
	}
	for grew := true; grew; {
		grew = false
		for name, callees := range calls {
			if _, done := reaching[name]; done {
				continue
			}
			for callee := range callees {
				if via, ok := reaching[callee]; ok {
					reaching[name] = callee + " → " + via
					grew = true
					break
				}
			}
		}
	}
	if len(reaching) == 0 {
		t.Fatalf("no caller of %v found: the guard is reading nothing", teamTokenMintVerbs)
	}
	return reaching
}

// parsedFuncCalls parses every non-test .go file of dir and answers, per
// top-level function, the names it calls: a bare identifier, a selector's
// final name, and "pkg.Name" where the selector's receiver is a plain
// identifier (so a package-qualified primitive is matched exactly).
func parsedFuncCalls(t *testing.T, dir string) map[string]map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	out := map[string]map[string]bool{}
	fset := token.NewFileSet()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, 0)
		if err != nil {
			t.Fatalf("parse %s/%s: %v", dir, e.Name(), err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			if out[fn.Name.Name] == nil {
				out[fn.Name.Name] = map[string]bool{}
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				for _, name := range calleeNames(call.Fun) {
					out[fn.Name.Name][name] = true
				}
				return true
			})
		}
	}
	return out
}

// calleeNames answers the names a call expression's function part is known
// by: "Name" for an identifier, "Sel" and "x.Sel" for a selector.
func calleeNames(fun ast.Expr) []string {
	switch f := fun.(type) {
	case *ast.Ident:
		return []string{f.Name}
	case *ast.SelectorExpr:
		names := []string{f.Sel.Name}
		if x, ok := f.X.(*ast.Ident); ok {
			names = append(names, x.Name+"."+f.Sel.Name)
		}
		return names
	}
	return nil
}

// routeRegistration is one s.mux.Handle("METHOD /pattern", …) of pkg/server:
// the handler names its expression mentions and the mint its gate names
// (empty when no gateDerivation wraps it).
type routeRegistration struct {
	key      string
	handlers []string
	gateMint string
	file     string
}

// parsedRouteRegistrations reads every s.mux.Handle / s.mux.HandleFunc call of
// pkg/server whose pattern is a string literal. A registration this cannot
// read is a registration the guard cannot judge, which is why
// TestEveryTeamTokenMintSitsBehindTheChokepoint fails on a mint-reaching
// handler it finds in none of them.
func parsedRouteRegistrations(t *testing.T) []routeRegistration {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package dir: %v", err)
	}
	fset := token.NewFileSet()
	var out []routeRegistration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, e.Name(), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) != 2 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (sel.Sel.Name != "Handle" && sel.Sel.Name != "HandleFunc") {
				return true
			}
			if !strings.HasSuffix(exprText(sel.X), "mux") {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			pattern, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			reg := routeRegistration{key: pattern, file: e.Name()}
			seen := map[string]bool{}
			ast.Inspect(call.Args[1], func(inner ast.Node) bool {
				switch v := inner.(type) {
				case *ast.Ident:
					if strings.HasPrefix(v.Name, "handle") && !seen[v.Name] {
						seen[v.Name] = true
						reg.handlers = append(reg.handlers, v.Name)
					}
				case *ast.SelectorExpr:
					if strings.HasPrefix(v.Sel.Name, "handle") && !seen[v.Sel.Name] {
						seen[v.Sel.Name] = true
						reg.handlers = append(reg.handlers, v.Sel.Name)
					}
				case *ast.CallExpr:
					gsel, ok := v.Fun.(*ast.SelectorExpr)
					if ok && gsel.Sel.Name == "gateDerivation" && len(v.Args) == 2 {
						reg.gateMint = exprText(v.Args[0])
					}
				}
				return true
			})
			out = append(out, reg)
			return true
		})
	}
	if len(out) < 100 {
		t.Fatalf("only %d route registrations read: the guard is reading nothing", len(out))
	}
	return out
}

// exprText renders an expression the way the source spells it, for the few
// shapes this guard compares ("auth.MintTeamToken", "s.mux").
func exprText(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return exprText(v.X) + "." + v.Sel.Name
	}
	return ""
}

// TestEveryTeamTokenMintSitsBehindTheChokepoint is the class guard of #2687:
// every handler that reaches a team-token mint — including the three the
// forge orchestrator mints for, two calls deep — is registered through
// gateDerivation(auth.MintTeamToken, …). The route that mints while asking
// nothing is the hole this closes; a mint-reaching handler registered in a
// shape this guard cannot read fails too, rather than passing unseen.
func TestEveryTeamTokenMintSitsBehindTheChokepoint(t *testing.T) {
	reaching := mintReachingFuncs(t)
	registrations := parsedRouteRegistrations(t)

	registered := map[string][]routeRegistration{}
	for _, reg := range registrations {
		for _, h := range reg.handlers {
			registered[h] = append(registered[h], reg)
		}
	}
	var mintHandlers []string
	for name := range reaching {
		if strings.HasPrefix(name, "handle") {
			mintHandlers = append(mintHandlers, name)
		}
	}
	sort.Strings(mintHandlers)
	if len(mintHandlers) == 0 {
		t.Fatal("no mint-reaching handler found: the guard is reading nothing")
	}
	for _, h := range mintHandlers {
		regs, ok := registered[h]
		if !ok {
			t.Errorf("%s reaches a team-token mint (%s) but no s.mux registration this guard can read names it", h, reaching[h])
			continue
		}
		for _, reg := range regs {
			if reg.gateMint != "auth.MintTeamToken" {
				t.Errorf("%q (%s, %s) reaches a team-token mint (%s) with gate %q, want gateDerivation(auth.MintTeamToken, …)",
					reg.key, reg.file, h, reaching[h], reg.gateMint)
			}
		}
	}
	t.Logf("mint-reaching handlers behind the chokepoint: %v", mintHandlers)
}

// TestTheChokepointTableMatchesTheRegistrations confronts the hand-written
// table the OpenAPI spec is generated from (gatedRouteChokepointCodes) with
// the registrations themselves: a route gated by gateDerivation states its
// 401 and 403 in the published spec, and the table claims no gate the mux
// does not have. The table is the only place the refusals are declared, so
// nothing but this comparison keeps it true.
func TestTheChokepointTableMatchesTheRegistrations(t *testing.T) {
	// The routes gated INSIDE their handler (checkDerivation at the top,
	// where the handler owns the refusal's shape) rather than by the
	// middleware: each names the mint it asks for.
	gatedInHandler := map[string]string{
		"POST /api/auth/me/team/{team_id}":          "MintAccessToken",
		"POST /api/auth/me/org/{org_id}":            "MintAccessToken",
		"POST /api/me/tokens":                       "MintPAT",
		"POST /api/ws/ticket":                       "MintWSTicket",
		"GET /api/me/sso/{provider}/link/start":     "MintSSOLink",
		"POST /api/admin/users/{id}/reset-password": "MintPassword",
	}
	byMiddleware := map[string]bool{}
	for _, reg := range parsedRouteRegistrations(t) {
		if reg.gateMint == "" {
			continue
		}
		byMiddleware[reg.key] = true
		codes, ok := gatedRouteChokepointCodes[reg.key]
		if !ok {
			t.Errorf("%q is registered through gateDerivation(%s, …) but states no refusal in gatedRouteChokepointCodes: the OpenAPI spec hides it", reg.key, reg.gateMint)
			continue
		}
		var has401, has403 bool
		for _, c := range codes {
			has401 = has401 || c == http.StatusUnauthorized
			has403 = has403 || c == http.StatusForbidden
		}
		if !has401 || !has403 {
			t.Errorf("%q is gated but its table entry %v misses 401 or 403", reg.key, codes)
		}
	}
	for key := range gatedRouteChokepointCodes {
		if byMiddleware[key] {
			continue
		}
		if _, ok := gatedInHandler[key]; !ok {
			t.Errorf("%q claims the chokepoint's refusals but no gateDerivation registration carries it, and it is not declared as gated in its handler", key)
		}
	}
}

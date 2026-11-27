package server

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/auth"
)

// The derivation chokepoint's module-wide guards — the primitives' demand,
// the verdict's stampers, the forge's ask before its mint and the caller
// inventory — live in internal/sweep/mintchokepoint, which reads every
// non-test .go file of the module. What stays here reads this package alone:
// the forge orchestrator built with its mint seam, and the OpenAPI table of
// gated routes confronted with the registrations.

// exprText renders an expression the way the source spells it, for the few
// shapes this file compares ("auth.MintTeamToken", "forge.Orchestrator").
func exprText(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return exprText(v.X) + "." + v.Sel.Name
	case *ast.StarExpr:
		return "*" + exprText(v.X)
	}
	return ""
}

// routeRegistration is one s.mux.Handle("METHOD /pattern", …) of pkg/server,
// reduced to what the OpenAPI confrontation reads: the pattern, the file,
// and the mint its gateDerivation wrapper names (empty when none wraps it).
type routeRegistration struct {
	key      string
	file     string
	gateMint string
}

// gateMintOf answers the mint a registration's handler value is wrapped in
// when gateDerivation names one, wherever the wrapper sits on the spine the
// value flows through — s.requireAuth(s.gateDerivation(auth.MintTeamToken,
// …)) names it through the requireAuth around it.
func gateMintOf(e ast.Expr) string {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return ""
	}
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "gateDerivation" && len(call.Args) == 2 {
		return exprText(call.Args[0])
	}
	// gateInvitationMint asks gateDerivation for the mint the presented
	// credential may make (invitationMint): the team token for a session, the
	// emailed invitation for a personal access token.
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "gateInvitationMint" && len(call.Args) == 1 {
		return "invitationMint(id)"
	}
	if m := gateMintOf(call.Fun); m != "" {
		return m
	}
	for _, a := range call.Args {
		if m := gateMintOf(a); m != "" {
			return m
		}
	}
	return ""
}

// parsedRouteRegistrations reads every Handle / HandleFunc call of
// pkg/server taking two arguments. It does NOT require the pattern to be a
// string literal (studio_legacy_redirect.go builds its patterns).
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
		file, perr := parser.ParseFile(fset, e.Name(), nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", e.Name(), perr)
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
			out = append(out, routeRegistration{
				key:      patternText(call.Args[0]),
				file:     e.Name(),
				gateMint: gateMintOf(call.Args[1]),
			})
			return true
		})
	}
	if len(out) < 100 {
		t.Fatalf("only %d route registrations read: the guard is reading nothing", len(out))
	}
	return out
}

// patternText renders the route pattern for the message: the literal when it
// is one, the source's own spelling otherwise ("GET /"+seg), so a
// registration whose pattern is computed is still named.
func patternText(e ast.Expr) string {
	if lit, ok := e.(*ast.BasicLit); ok && lit.Kind == token.STRING {
		if s, serr := strconv.Unquote(lit.Value); serr == nil {
			return s
		}
	}
	if b, ok := e.(*ast.BinaryExpr); ok {
		return patternText(b.X) + "+" + patternText(b.Y)
	}
	if t := exprText(e); t != "" {
		return t
	}
	return "«a pattern this guard cannot render»"
}

// TestTheForgeOrchestratorIsBuiltWithTheMintChokepoint closes the one hole
// the verdict's construction cannot see from inside the forge: the approval
// asked at the mint is asked through an injected seam, and a seam left nil
// REFUSES (a server that cannot mint is down, not open) — so wherever this
// package builds a forge.Orchestrator, ApproveTeamTokenMint is wired.
func TestTheForgeOrchestratorIsBuiltWithTheMintChokepoint(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package dir: %v", err)
	}
	fset := token.NewFileSet()
	built := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		file, perr := parser.ParseFile(fset, e.Name(), nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", e.Name(), perr)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok || exprText(lit.Type) != "forge.Orchestrator" {
				return true
			}
			built++
			wired := ""
			for _, el := range lit.Elts {
				kv, ok := el.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if exprText(kv.Key) == "ApproveTeamTokenMint" {
					wired = exprText(kv.Value)
				}
			}
			if wired == "" || wired == "nil" {
				t.Errorf("%s builds a forge.Orchestrator with ApproveTeamTokenMint %q: the approval asked at the mint is a seam, and an unwired seam refuses every provisioning that mints",
					e.Name(), wired)
			}
			return true
		})
	}
	if built == 0 {
		t.Fatal("no forge.Orchestrator is built in this package: the guard is reading nothing")
	}
}

// TestTheChokepointTableMatchesTheRegistrations confronts the hand-written
// table the OpenAPI spec is generated from (gatedRouteChokepointCodes) with
// the registrations themselves: a route gated by gateDerivation states its
// 401 and 403 in the published spec, and the table claims no gate the mux
// does not have. The table is the only place the refusals are declared, so
// nothing but this comparison keeps it true.
func TestTheChokepointTableMatchesTheRegistrations(t *testing.T) {
	// The routes whose refusal comes from BENEATH the registration: the
	// handler asks checkDerivation itself, or the mint it reaches asks at
	// the mint (the repo-bot provisioning, whose question is only put when
	// a hook identity is really reserved). Each names the mint it asks for.
	gatedBeneathTheRegistration := map[string]string{
		"POST /api/auth/me/team/{team_id}":          "MintAccessToken",
		"POST /api/auth/me/org/{org_id}":            "MintAccessToken",
		"POST /api/me/tokens":                       "MintPAT",
		"POST /api/ws/ticket":                       "MintWSTicket",
		"GET /api/me/sso/{provider}/link/start":     "MintSSOLink",
		"POST /api/admin/users/{id}/reset-password": "MintPassword",
		// forge.Orchestrator.ApproveTeamTokenMint, asked at the
		// reservation of a NEW hook identity: a re-provision that reuses
		// the integration's sealed secret mints nothing and answers 200.
		"POST /api/teams/{id}/forge/repo-bots":                          "MintTeamToken",
		"PATCH /api/teams/{id}/forge/repo-bots/{integration_id}":        "MintTeamToken",
		"POST /api/orgs/{id}/provision-approvals/{approval_id}/approve": "MintTeamToken",
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
		if _, ok := gatedBeneathTheRegistration[key]; !ok {
			t.Errorf("%q claims the chokepoint's refusals but no gateDerivation registration carries it, and it is not declared as gated beneath its registration", key)
		}
	}
	for key := range gatedBeneathTheRegistration {
		if byMiddleware[key] {
			t.Errorf("%q is declared as gated beneath its registration AND registered through gateDerivation: one of the two is stale", key)
		}
		if _, ok := gatedRouteChokepointCodes[key]; !ok {
			t.Errorf("%q is declared as gated beneath its registration but states no refusal in gatedRouteChokepointCodes: the OpenAPI spec hides a 401/403 the route really answers", key)
		}
	}
}

// testMintVerdictContext carries the verdict the derivation gate stamps
// after its ask grants — issued through the same constructor the gate uses
// (auth.NewMintVerdict), so a test mints under exactly the context a gated
// handler runs under. The mint primitives this package reaches
// (webhooks.MintToken, configshare.MintToken, auth.Service.CreateInvitation)
// refuse a context without one.
func testMintVerdictContext(ctx context.Context) context.Context {
	return auth.ContextWithMintVerdict(ctx, auth.NewMintVerdict(auth.MintTeamToken, "test"))
}

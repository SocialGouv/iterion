package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// teamRouteGateDepth bounds how many calls deep, from a route's handler or
// wrapper, the scan looks for a team gate.
const teamRouteGateDepth = 4

// notATeamGate are the wrappers that reference a team gate for their own
// purpose — requireAuth asks canManageTeam to canonicalize a team path — and
// authorize nothing for the route they wrap.
var notATeamGate = map[string]bool{"requireAuth": true}

// TestEveryTeamRouteReachesATeamPinGate: TestEveryAuthzGateAnswersTheCredentialPin
// proves each GATE answers the pin; this proves each team-addressed ROUTE
// goes through one. A route under /api/teams/{id} authorized on the org
// standing alone (canManageOrg answers the ORG pin only) let a token pinned
// to one team delete a sibling team, and rewrite the org's pool through a
// sibling team's path. Every registration under /api/teams/{id} must reach,
// from its handler or a wrapper, a gate the patPinGates table resolves to
// PinRefusesTeam (or PinRefusesTeam itself) within teamRouteGateDepth calls.
//
// What it does not prove, stated: a gate reached on another path of the
// handler counts as reached; the behavioural census
// (TestPinnedTokenTeamRoutes_NoSiblingTeamAnswers2xx) measures the answer on
// the routes the test server registers.
func TestEveryTeamRouteReachesATeamPinGate(t *testing.T) {
	gates := teamPinGates(t)
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	bodies := map[string]*ast.BlockStmt{}
	type registration struct {
		pattern  string
		handlers []string
	}
	var regs []registration
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		for _, decl := range f.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Body != nil && fn.Recv != nil {
				bodies[fn.Name.Name] = fn.Body
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) != 2 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (sel.Sel.Name != "Handle" && sel.Sel.Name != "HandleFunc") {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			pattern, err := strconv.Unquote(lit.Value)
			if err != nil || !strings.Contains(pattern, " /api/teams/{id}") {
				return true
			}
			reg := registration{pattern: pattern}
			ast.Inspect(call.Args[1], func(m ast.Node) bool {
				if se, ok := m.(*ast.SelectorExpr); ok {
					if recv, ok := se.X.(*ast.Ident); ok && recv.Name == "s" && !notATeamGate[se.Sel.Name] {
						reg.handlers = append(reg.handlers, se.Sel.Name)
					}
				}
				return true
			})
			regs = append(regs, reg)
			return true
		})
	}
	if len(regs) < 100 {
		t.Fatalf("the scan found %d registrations under /api/teams/{id}; the registration shape changed and the census no longer sees them", len(regs))
	}

	var reaches func(name string, depth int, seen map[string]bool) bool
	reaches = func(name string, depth int, seen map[string]bool) bool {
		if gates[name] {
			return true
		}
		body := bodies[name]
		if body == nil || depth == 0 || seen[name] {
			return false
		}
		seen[name] = true
		found := false
		ast.Inspect(body, func(n ast.Node) bool {
			if found {
				return false
			}
			if se, ok := n.(*ast.SelectorExpr); ok && !notATeamGate[se.Sel.Name] && reaches(se.Sel.Name, depth-1, seen) {
				found = true
			}
			return !found
		})
		return found
	}

	var missing []string
	for _, reg := range regs {
		ok := false
		for _, h := range reg.handlers {
			if reaches(h, teamRouteGateDepth, map[string]bool{}) {
				ok = true
				break
			}
		}
		if !ok {
			missing = append(missing, reg.pattern+" -> "+strings.Join(reg.handlers, ", "))
		}
	}
	sort.Strings(missing)
	for _, m := range missing {
		t.Errorf("team-addressed route reaches no gate answering the team pin within %d calls: %s — authorize it with canViewTeam/canManageTeam/orgAdminOfTeam, or refuse auth.Identity.PinRefusesTeam before an org-level gate", teamRouteGateDepth, m)
	}
}

// teamPinGates resolves the patPinGates table to the gates whose chain ends
// at PinRefusesTeam: the ones a team-addressed route may rely on.
func teamPinGates(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{"PinRefusesTeam": true}
	for name := range patPinGates {
		next, hops := name, 0
		for next != "PinRefusesTeam" && next != "PinRefusesOrg" {
			n, ok := patPinGates[next]
			if !ok || hops > len(patPinGates) {
				t.Fatalf("patPinGates: %s's chain ends at %q, which is no gate of the table", name, next)
			}
			next, hops = n, hops+1
		}
		if next == "PinRefusesTeam" {
			out[name] = true
		}
	}
	if !out["canViewTeam"] || !out["canManageTeam"] || out["canManageOrg"] {
		t.Fatalf("patPinGates no longer resolves the team gates as this census expects: %v", out)
	}
	return out
}

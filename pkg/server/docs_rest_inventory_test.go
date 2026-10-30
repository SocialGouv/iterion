package server

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// docs/cloud-rest-api-generated.md is the GENERATED annex of the curated
// reference: every route this binary registers under /api with a literal
// pattern, its outer auth rung, and whether the published spec carries it.
// Two gates hold it here:
//
//   - FRESHNESS: the committed bytes are what the generator renders from
//     this tree — a route added without a regen reddens, which is the
//     direction a curated page always lost.
//   - BOTH WAYS: the annex's rows and the source-derived registration
//     table are the same set — every registered route is in the annex, and
//     every annex row is registered. Freshness makes the two nearly the
//     same check; this one pays when somebody edits the annex by hand in
//     exactly the shape the generator would write (a copied row, a renamed
//     pattern): bytes still differ from a regen, but the ROWS are what a
//     reader trusts, so they get their own verdict.
//
// The third leg — the source scan covers what a running server records —
// is cli_route_reachability_test.go's
// TestTheSourceDerivedTableCoversTheRunningOne.

// restAnnexPairFloor is the number of rows the annex must still yield.
// Measured at 460 on 2026-10-30; the floor catches a parse that stops
// reading the tables and would otherwise judge nothing.
const restAnnexPairFloor = 400

var restAnnexRow = regexp.MustCompile("^\\| `([A-Z]+)` \\| `(/api[^`]*)` \\| ([^|]+) \\| (yes|no) \\|$")

// annexRoutes reads every (method, path) row of the generated annex.
func annexRoutes(t *testing.T, body string) []RouteInfo {
	t.Helper()
	var out []RouteInfo
	seen := map[RouteInfo]bool{}
	for i, line := range strings.Split(body, "\n") {
		m := restAnnexRow.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		method := m[1]
		if method == "ANY" {
			method = ""
		}
		r := RouteInfo{Method: method, Pattern: m[2]}
		if seen[r] {
			continue
		}
		seen[r] = true
		if strings.TrimSpace(m[3]) == "" {
			t.Errorf("annex row %d carries no auth rung: %q", i+1, line)
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Pattern != out[j].Pattern {
			return out[i].Pattern < out[j].Pattern
		}
		return out[i].Method < out[j].Method
	})
	return out
}

// diffAnnexAgainstRegistered names the rows where the annex and the
// source-derived registration table disagree, in both directions.
func diffAnnexAgainstRegistered(annex, registered []RouteInfo) []string {
	inAnnex := map[RouteInfo]bool{}
	for _, r := range annex {
		inAnnex[r] = true
	}
	inRegistered := map[RouteInfo]bool{}
	for _, r := range registered {
		inRegistered[r] = true
	}
	var problems []string
	for _, r := range registered {
		if !inAnnex[r] {
			problems = append(problems, routeInfoString(r)+" is registered and absent from the annex")
		}
	}
	for _, r := range annex {
		if !inRegistered[r] {
			problems = append(problems, routeInfoString(r)+" is an annex row no pkg/server registration owns")
		}
	}
	sort.Strings(problems)
	return problems
}

func routeInfoString(r RouteInfo) string {
	if r.Method == "" {
		return "ANY " + r.Pattern
	}
	return r.Method + " " + r.Pattern
}

// registeredAPIRoutes is the annex's expected set: the source-derived
// registration table, scoped to the /api tree the annex covers.
func registeredAPIRoutes(t *testing.T) []RouteInfo {
	t.Helper()
	var out []RouteInfo
	for _, r := range serverSourceRoutes(t) {
		if strings.HasPrefix(r.Pattern, "/api/") || r.Pattern == "/api" {
			out = append(out, r)
		}
	}
	return out
}

func TestTheGeneratedRESTInventoryIsFresh(t *testing.T) {
	rendered, err := GenerateRESTInventoryAnnex("../..")
	if err != nil {
		t.Fatal(err)
	}
	committed, err := os.ReadFile(filepath.Join("..", "..", RESTInventoryAnnexPath))
	if err != nil {
		t.Fatalf("read the committed annex: %v — run `task restapi:gen` and commit", err)
	}
	if string(committed) != string(rendered) {
		t.Fatalf("%s is stale — run `task restapi:gen` and commit", RESTInventoryAnnexPath)
	}
}

func TestTheAnnexMatchesTheRegisteredSurfaceBothWays(t *testing.T) {
	committed, err := os.ReadFile(filepath.Join("..", "..", RESTInventoryAnnexPath))
	if err != nil {
		t.Fatalf("read the committed annex: %v — run `task restapi:gen` and commit", err)
	}
	annex := annexRoutes(t, string(committed))
	if len(annex) < restAnnexPairFloor {
		t.Fatalf("read %d rows from %s, floor %d — the parse went blind and this gate would judge nothing", len(annex), RESTInventoryAnnexPath, restAnnexPairFloor)
	}
	for _, problem := range diffAnnexAgainstRegistered(annex, registeredAPIRoutes(t)) {
		t.Error(problem)
	}
}

// A gate that cannot redden is not a gate: the comparator must name a row
// missing from the annex, a row the registration table does not own, and
// stay silent on an exact match.
func TestTheAnnexGateBites(t *testing.T) {
	registered := []RouteInfo{
		{Method: "GET", Pattern: "/api/orgs"},
		{Method: "POST", Pattern: "/api/orgs"},
		{Method: "", Pattern: "/api/anything"},
	}
	if problems := diffAnnexAgainstRegistered(registered, registered); len(problems) > 0 {
		t.Fatalf("an exact match reddened: %v", problems)
	}
	missing := diffAnnexAgainstRegistered(registered[:1], registered)
	if len(missing) != 2 || !strings.Contains(strings.Join(missing, "\n"), "POST /api/orgs is registered and absent from the annex") {
		t.Fatalf("a route missing from the annex was not named both ways it should be: %v", missing)
	}
	extra := diffAnnexAgainstRegistered(append(append([]RouteInfo{}, registered...), RouteInfo{Method: "GET", Pattern: "/api/ghost"}), registered)
	if len(extra) != 1 || !strings.Contains(extra[0], "/api/ghost") || !strings.Contains(extra[0], "no pkg/server registration owns") {
		t.Fatalf("an annex row without a registration was not named: %v", extra)
	}
}

// The rung is the gate a caller meets, so the classifier owes a case per
// rung — and per trap: a marketplace WRITE is a session though the family's
// reads are open, the preflight responder is open without a public path,
// and a bare registration is a session, not an opening.
func TestRegistrationRungClassifiesTheOuterGate(t *testing.T) {
	cases := []struct {
		head, method, pattern, want string
	}{
		{"s.requireSuperAdmin", "GET", "/api/admin/bots", "platform admin"},
		{"s.boardWriteAuth", "POST", "/api/v1/boards/cards", "session + write rung"},
		{"s.triggerWriteAuth", "GET", "/api/v1/triggers", "session + write rung"},
		{"s.requireAuth", "GET", "/api/runs", "session"},
		{"loginLimit", "POST", "/api/auth/login", "open"},
		{"s.webhookAuth", "POST", "/api/webhooks/github/{id}", "open"},
		{"s.configShareAuth", "GET", "/api/config-share/{id}/config", "open"},
		{"func", "GET", "/api/server/info", "open"},
		{"func", "OPTIONS", "/api/", "open"},
		{"func", "GET", "/api/v1/marketplace/bots", "open"},
		{"func", "POST", "/api/v1/marketplace/bots/{slug}/install", "session"},
		{"func", "GET", "/api/runs", "session"},
	}
	for _, c := range cases {
		if got := registrationRung(c.head, c.method, c.pattern); got != c.want {
			t.Errorf("registrationRung(%q, %q, %q) = %q, want %q", c.head, c.method, c.pattern, got, c.want)
		}
	}
}

// The ghost check refuses a spec row no registration owns — the one
// direction that is a defect — and accepts a registration the spec lacks
// (a gated family), which the annex's Spec column carries instead.
func TestTheSpecGhostCheckBites(t *testing.T) {
	routes := []InventoryRoute{
		{Method: "GET", Pattern: "/api/orgs"},
		{Method: "", Pattern: "/api/anything"},
		{Method: "GET", Pattern: "/api/examples/{name...}"},
	}
	spec := map[specPair]bool{
		{"GET", "/api/orgs"}:            true,
		{"GET", "/api/anything"}:        true,
		{"POST", "/api/anything"}:       true,
		{"GET", "/api/examples/{name}"}: true,
		{"GET", "/api/ghost"}:           true,
	}
	problems := specGhostCheck(routes, spec)
	if len(problems) != 1 || !strings.Contains(problems[0], "/api/ghost") {
		t.Fatalf("a spec row without a registration was not refused: %v", problems)
	}
	if !inSpec(routes[0], spec) || !inSpec(routes[1], spec) || !inSpec(routes[2], spec) {
		t.Fatalf("published registrations read as out of spec: %+v", routes)
	}
	delete(spec, specPair{"POST", "/api/anything"})
	if inSpec(routes[1], spec) {
		t.Fatal("a method-less registration whose POST the spec lacks must read as out of spec")
	}
}

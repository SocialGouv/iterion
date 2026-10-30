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

// annexRow is one parsed row of the generated annex, rung included — the
// rung is the cell the rung cross-check exists to judge.
type annexRow struct {
	RouteInfo
	Rung string
	Spec string
}

// annexRows reads every row of the generated annex.
func annexRows(t *testing.T, body string) []annexRow {
	t.Helper()
	var out []annexRow
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
		out = append(out, annexRow{RouteInfo: r, Rung: strings.TrimSpace(m[3]), Spec: m[4]})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Pattern != out[j].Pattern {
			return out[i].Pattern < out[j].Pattern
		}
		return out[i].Method < out[j].Method
	})
	return out
}

// annexRoutes reads every (method, path) row of the generated annex.
func annexRoutes(t *testing.T, body string) []RouteInfo {
	t.Helper()
	rows := annexRows(t, body)
	out := make([]RouteInfo, len(rows))
	for i, r := range rows {
		out[i] = r.RouteInfo
	}
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

// The rung column is the annex's one SECURITY-relevant cell, and freshness
// alone cannot hold it: the generator classifying wrong still renders bytes
// equal to its own output (a platform-admin route registered through a
// local variable — `h := s.requireSuperAdmin(…)` — read as "session" was
// the reviewer probe of #2821's first round). So the rung gets its own
// witness, independent of the classifier: this scan resolves the aliases
// for ITSELF and holds the annex to two truths —
//
//   - every annex row under /api/admin/ reads "platform admin" (the admin
//     prefix family is uniformly requireSuperAdmin by convention, so the
//     prefix alone is a verdict no classifier bug can talk its way out of);
//     the platform-admin family OUTSIDE the prefix gets the same verdict
//     where a rule already codifies it: every non-GET /api/v1/plugins
//     route, the rule chokepointResponses (openapi.go) applies;
//   - every registration whose resolved head IS requireSuperAdmin — inline
//     or followed through a one-hop local alias — reads "platform admin" in
//     the annex.
var (
	witnessRegistrationRe = regexp.MustCompile(`(?i:mux)\.Handle(?:Func)?\("(?:([A-Z]+) )?(/[^"]*)",\s*([A-Za-z_.]+)`)
	witnessAliasRe        = regexp.MustCompile(`(?m)^[ \t]*([a-z]\w*) :=\s*([A-Za-z_][\w.]*)\b`)
	witnessLineComments   = regexp.MustCompile(`(?m)//.*$`)
)

// platformAdminRegistrations is the witness's own read of the source: every
// /api registration whose handler head — resolved through the file's local
// aliases — is requireSuperAdmin.
func platformAdminRegistrations(t *testing.T) []RouteInfo {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the server package: %v", err)
	}
	var out []RouteInfo
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(name) // #nosec G304 -- the package's own directory listing
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		code := witnessLineComments.ReplaceAllString(string(raw), "")
		aliases := map[string]string{}
		for _, m := range witnessAliasRe.FindAllStringSubmatch(code, -1) {
			aliases[m[1]] = m[2]
		}
		for _, m := range witnessRegistrationRe.FindAllStringSubmatch(code, -1) {
			head := m[3]
			for hops := 0; hops < 4; hops++ {
				next, ok := aliases[head]
				if !ok {
					break
				}
				head = next
			}
			if !strings.Contains(head, "requireSuperAdmin") {
				continue
			}
			if !strings.HasPrefix(m[2], "/api/") {
				continue
			}
			out = append(out, RouteInfo{Method: m[1], Pattern: m[2]})
		}
	}
	// The floor is the witness's blindness net: the family counts in the
	// sixties, so a regex that stops reading it would pass an empty check.
	if len(out) < 50 {
		t.Fatalf("the witness found %d requireSuperAdmin registration(s), floor 50 — it went blind and the rung cross-check would judge nothing", len(out))
	}
	return out
}

func TestTheAdminRungCannotBeMislabeled(t *testing.T) {
	committed, err := os.ReadFile(filepath.Join("..", "..", RESTInventoryAnnexPath))
	if err != nil {
		t.Fatalf("read the committed annex: %v — run `task restapi:gen` and commit", err)
	}
	rows := annexRows(t, string(committed))
	byRoute := map[RouteInfo]string{}
	for _, r := range rows {
		byRoute[r.RouteInfo] = r.Rung
	}
	adminPrefixRows := 0
	pluginWriteRows := 0
	for _, r := range rows {
		if strings.HasPrefix(r.Pattern, "/api/admin/") {
			adminPrefixRows++
			if r.Rung != "platform admin" {
				t.Errorf("annex row %s reads rung %q — every /api/admin/ route is platform admin", routeInfoString(r.RouteInfo), r.Rung)
			}
		}
		// The platform-admin family outside the /api/admin/ prefix, where a
		// rule already codifies it: the non-GET plugin routes
		// (chokepointResponses, openapi.go).
		if strings.HasPrefix(r.Pattern, "/api/v1/plugins") && r.Method != "GET" {
			pluginWriteRows++
			if r.Rung != "platform admin" {
				t.Errorf("annex row %s reads rung %q — a non-GET /api/v1/plugins route is platform admin (the chokepointResponses rule)", routeInfoString(r.RouteInfo), r.Rung)
			}
		}
	}
	if adminPrefixRows < 50 {
		t.Fatalf("only %d /api/admin/ row(s) in the annex, floor 50 — the prefix leg of this witness is judging nothing", adminPrefixRows)
	}
	if pluginWriteRows < 5 {
		t.Fatalf("only %d non-GET /api/v1/plugins row(s) in the annex, floor 5 — the plugin leg of this witness is judging nothing", pluginWriteRows)
	}
	for _, reg := range platformAdminRegistrations(t) {
		if rung, ok := byRoute[reg]; !ok || rung != "platform admin" {
			t.Errorf("%s is registered with requireSuperAdmin and the annex reads rung %q — a platform-admin route documented lower is the lie this witness exists to refuse", routeInfoString(reg), byRoute[reg])
		}
	}
}

// The scan reads LITERAL registrations only; a platform-admin family
// mounted as a sub-tree on the raw mux under /api/admin would be invisible
// to it AND to the prefix leg above — the one hole left. The raw mounts all
// live in server_routes.go, so the guard is one sentence long: no sub-tree
// mount prefix there starts with /api/admin.
func TestNoAdminFamilyMountsOutsideTheScan(t *testing.T) {
	raw, err := os.ReadFile("server_routes.go")
	if err != nil {
		t.Fatalf("read server_routes.go: %v", err)
	}
	code := witnessLineComments.ReplaceAllString(string(raw), "")
	mounts := regexp.MustCompile(`RegisterRoutesWithMiddleware\(s\.mux\.ServeMux, "([^"]+)"`).FindAllStringSubmatch(code, -1)
	if len(mounts) < 2 {
		t.Fatalf("found %d sub-tree mount(s) in server_routes.go, floor 2 — the witness no longer reads the mounts it guards", len(mounts))
	}
	for _, m := range mounts {
		if strings.HasPrefix(m[1], "/api/admin") {
			t.Errorf("%s mounts a whole sub-tree on the raw mux under /api/admin — its routes escape the annex's scan AND its prefix leg; register it with literal patterns or move the prefix", m[1])
		}
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
		{"s.limitRoute", "POST", "/api/auth/login", "open"}, // the rate limiters are auth-neutral; the public-path rule decides
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

// resolveHead is where a hidden rung is refused: an alias to a rate limiter
// resolves and classifies, an alias to an AUTH wrapper refuses the
// generation (the annex would otherwise print "session" for a
// platform-admin route with every freshness gate green), and a bare head
// naming nothing declared refuses too.
func TestResolveHeadFollowsAliasesAndRefusesHiddenRungs(t *testing.T) {
	funcs := map[string]bool{"SPAHandler": true}
	aliases := map[string]string{
		"loginLimit":  "s.limitRoute",
		"adminProbe":  "s.requireSuperAdmin",
		"hiddenAdmin": "func",
		"loop":        "loop",
	}
	if got, err := resolveHead("loginLimit", aliases, funcs); err != nil || got != "s.limitRoute" {
		t.Errorf("loginLimit resolves to s.limitRoute, got %q err %v", got, err)
	}
	if _, err := resolveHead("adminProbe", aliases, funcs); err == nil || !strings.Contains(err.Error(), "hides behind the local variable") {
		t.Errorf("an alias hiding requireSuperAdmin must refuse, got %v", err)
	}
	if _, err := resolveHead("adminProbe", aliases, funcs); err == nil || !strings.Contains(err.Error(), "adminProbe") {
		t.Errorf("the refusal must name the variable, got %v", err)
	}
	// A closure behind a variable is refused whatever it wraps — the body is
	// unreadable, so "no admin wrapper inside" is not a verdict the
	// generator can give.
	if _, err := resolveHead("hiddenAdmin", aliases, funcs); err == nil || !strings.Contains(err.Error(), "function value") {
		t.Errorf("an alias to a function value must refuse, got %v", err)
	}
	if got, err := resolveHead("s.requireAuth", aliases, funcs); err != nil || got != "s.requireAuth" {
		t.Errorf("a dotted head passes through, got %q err %v", got, err)
	}
	if got, err := resolveHead("SPAHandler", aliases, funcs); err != nil || got != "SPAHandler" {
		t.Errorf("a declared package func passes, got %q err %v", got, err)
	}
	if got, err := resolveHead("func", aliases, funcs); err != nil || got != "func" {
		t.Errorf("an inline literal passes, got %q err %v", got, err)
	}
	if _, err := resolveHead("mystery", aliases, funcs); err == nil || !strings.Contains(err.Error(), "cannot resolve") {
		t.Errorf("an unresolvable bare head must refuse, got %v", err)
	}
	if _, err := resolveHead("loop", aliases, funcs); err == nil {
		t.Errorf("an alias cycle must refuse, got %v", err)
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

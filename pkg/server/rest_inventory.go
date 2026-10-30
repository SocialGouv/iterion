package server

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// This file is the GENERATOR of docs/cloud-rest-api-generated.md, the
// exhaustive annex that stands next to the curated cloud-rest-api.md: one
// row per route this binary registers under /api with a literal pattern,
// with the outer auth rung a caller meets. The curated page keeps the prose
// a generator cannot write (per-route authorization detail, refusal
// reasons, pitfalls); the annex keeps the inventory a hand-written page
// cannot hold (hundreds of registrations, drifting at every route
// addition).
//
// Two sources, both in the tree, never a running server:
//
//   - the REGISTRATION TABLE, read off this package's own source — a
//     registrar self-gates on the stores its family needs, so a live
//     instance's table is a DEPLOYMENT's surface, while the annex owes the
//     binary's whole one. The scan also carries the registration's auth
//     wrapper, which the mux never records (it sees the wrapped handler).
//   - the committed openapi.json, cross-checked against the scan: a spec
//     operation no registration owns is a ghost and fails the generation.
//     The other direction is a COLUMN, not a refusal — families gated off
//     in the offline spec build (the local-studio registrars, the DLQ, the
//     cloud schedules) and the literal raw-mux registrations are served
//     routes of the binary the published spec legitimately lacks, and the
//     annex is exactly where a reader learns that.
//
// The freshness and two-way gates live in docs_rest_inventory_test.go; the
// source-scan ↔ live-table cross-check is cli_route_reachability_test.go's.

// RESTInventoryAnnexPath is the annex's path relative to the repository root.
const RESTInventoryAnnexPath = "docs/cloud-rest-api-generated.md"

// InventoryRoute is one annex row: the verb ("" when the registration
// matches any method) and pattern as registered, the outer auth rung, the
// registrar file that carries the route, and whether the committed spec
// publishes the route.
type InventoryRoute struct {
	Method  string
	Pattern string
	Rung    string
	File    string
	InSpec  bool
}

// routeRegistrationRe reads one literal registration: the receiver is
// anything ending in "mux" (s.mux, a local alias, the raw s.mux.ServeMux),
// the pattern a single string literal, and the handler's head token the
// auth wrapper's name when there is one — digits included, so `hop2` does
// not truncate to `hop` (the alias regex below reads \w). A registration
// spelled over several lines still matches — the run between the pattern
// and the handler crosses newlines. A route registered through a
// non-literal pattern (the dispatcher and native-board sub-trees,
// "METHOD "+p+"<tail>") is out of this scan by design: those families mount
// on the raw mux, and the annex's scope sentence says so.
var routeRegistrationRe = regexp.MustCompile(`(?i:mux)\.Handle(?:Func)?\("(?:([A-Z]+) )?(/[^"]*)",\s*([A-Za-z_.0-9]+)`)

// localAliasRe reads a one-hop local alias of a wrapper —
// `loginLimit := s.limitRoute(...)` — so the classifier can follow it. The
// paren is OPTIONAL: `h := s.requireSuperAdmin` (a method VALUE, no call)
// is the same hiding with another spelling. A registration whose rung is
// spelled at the call site is the rule this enforces by construction: see
// resolveHead.
var localAliasRe = regexp.MustCompile(`(?m)^[ \t]*([a-z]\w*) :=\s*([A-Za-z_][\w.]*)\b`)

// inventoryLineComments strips line comments before the scan, so a retired
// route quoted in a comment is not a registration.
var inventoryLineComments = regexp.MustCompile(`(?m)//.*$`)

// authWrapperHeads are the wrapper heads that carry an auth rung. A
// registration hiding one of them behind a local variable is refused, not
// classified: the rung is the one fact of a registration this annex exists
// to publish, so it must be readable on the registration line itself.
var authWrapperHeads = []string{
	"requireSuperAdmin", "requireAuth",
	"boardWriteAuth", "triggerWriteAuth",
	"webhookAuth", "configShareAuth",
}

func isAuthWrapperHead(head string) bool {
	for _, w := range authWrapperHeads {
		if strings.HasSuffix(head, w) {
			return true
		}
	}
	return false
}

// packageFuncRe reads a package-level func declaration, so a bare head that
// names one (handleBrandAsset, SPAHandler) is a handler the generator can
// classify, while one that names nothing declared (a local variable hiding
// a wrapper) is refused.
var packageFuncRe = regexp.MustCompile(`(?m)^func ([A-Za-z_]\w*)\(`)

// scanRouteInventory reads every literal mux registration of the server
// package's non-test sources, following a local alias of a wrapper one hop
// at a time (loginLimit := s.limitRoute(…), the rate limiters of
// auth_routes.go). Three shapes are REFUSED rather than classified, because
// classifying them would write a lie with every gate green:
//
//   - an alias that hides an auth wrapper (`h := s.requireSuperAdmin(…)`)
//     behind a bare name — the annex would print "session" for a
//     platform-admin route (the reviewer probe of #2821's first round);
//   - an alias to a function VALUE (`h := func(next …) …`) — the closure's
//     body is unreadable, so whatever it wraps is invisible (second round);
//   - a bare head that is neither an inline literal, nor a declared
//     package-level func, nor an alias that resolves — a handler this
//     generator cannot name is a registration it cannot judge.
//
// One more forbidden form, documented rather than refused because the scan
// reads names, not assignments: a middleware stored in a FIELD of the
// Server (`s.adminGate(h)` where the field was assigned
// `s.requireSuperAdmin` at wiring). The head reads receiver-qualified and
// classifies on its name alone — never store a rung in a field; wrap the
// registration inline like every route in this package does.
func scanRouteInventory(dir string) ([]InventoryRoute, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read the server package: %w", err)
	}
	type registrar struct {
		name    string
		code    string
		aliases map[string]string
	}
	var regs []registrar
	funcs := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, name)) // #nosec G304 -- the package directory the caller rooted at the repo
		if err != nil {
			return nil, err
		}
		code := inventoryLineComments.ReplaceAllString(string(raw), "")
		aliases := map[string]string{}
		for _, m := range localAliasRe.FindAllStringSubmatch(code, -1) {
			aliases[m[1]] = m[2]
		}
		for _, m := range packageFuncRe.FindAllStringSubmatch(code, -1) {
			funcs[m[1]] = true
		}
		regs = append(regs, registrar{name, code, aliases})
	}
	var out []InventoryRoute
	for _, reg := range regs {
		for _, m := range routeRegistrationRe.FindAllStringSubmatch(reg.code, -1) {
			method, pattern, head := m[1], m[2], m[3]
			resolved, err := resolveHead(head, reg.aliases, funcs)
			if err != nil {
				return nil, fmt.Errorf("%s %s (%s): %w", displayMethod(method), pattern, reg.name, err)
			}
			out = append(out, InventoryRoute{
				Method:  method,
				Pattern: pattern,
				Rung:    registrationRung(resolved, method, pattern),
				File:    reg.name,
			})
		}
	}
	return out, nil
}

func displayMethod(method string) string {
	if method == "" {
		return "ANY"
	}
	return method
}

// resolveHead follows a bare head token through the file's local aliases,
// at most four hops (a cycle resolves to nothing useful and is refused as
// unclassifiable).
func resolveHead(head string, aliases map[string]string, funcs map[string]bool) (string, error) {
	resolved := head
	aliased := false
	for range 4 {
		next, ok := aliases[resolved]
		if !ok {
			break
		}
		resolved, aliased = next, true
	}
	switch {
	case aliased && isAuthWrapperHead(resolved):
		// Before the dotted-head pass-through: an alias to s.requireSuperAdmin
		// has a dot too, and the rung it hides is the lie this refuses.
		return "", fmt.Errorf("the auth rung hides behind the local variable %q (→ %s): wrap the registration inline so the rung is readable on the registration line — the annex's Auth column is compiled from that line", head, resolved)
	case aliased && resolved == "func":
		// A function VALUE behind a variable (`h := func(next …) … { return
		// s.requireSuperAdmin(next) }`) is not an inline literal: the
		// generator never sees the closure's body, so whatever rung it wraps
		// is unreadable on the registration line. Refused wholesale — a
		// closure WITHOUT an admin wrapper is refused too, because the
		// classifier cannot tell the difference without reading the body
		// (the reviewer probe of #2821's second round). Zero registration in
		// the tree takes this shape today.
		return "", fmt.Errorf("the handler %q is a function value (→ a closure): a closure's body is unreadable to this generator, so any rung it wraps would be invisible — name a declared handler or wrap the registration inline", head)
	case resolved == "func" || strings.Contains(resolved, "."):
		// An inline function literal, or a receiver-qualified expression
		// (s.requireAuth, http.NotFoundHandler, s.hub.HandleWebSocket): the
		// head names itself.
		return resolved, nil
	case funcs[resolved]:
		// A declared package-level handler (handleBrandAsset, SPAHandler).
		return resolved, nil
	default:
		// An unresolvable bare head, or an alias chain that bottoms out at
		// one (a cycle included): a handler this generator cannot name is a
		// registration it cannot judge.
		return "", fmt.Errorf("the handler head %q is a bare name this generator cannot resolve — the annex cannot name its auth rung; wrap the registration with a visible wrapper or name the handler s.handle*", head)
	}
}

// registrationRung is the OUTER gate a caller meets, read off the
// registration itself: the wrapper when there is one, the public-path rules
// the mux-wide auth middleware applies (the same functions the middleware
// runs, never a copy of them), and the middleware's OPTIONS bypass.
func registrationRung(head, method, pattern string) string {
	switch {
	case strings.Contains(head, "requireSuperAdmin"):
		return "platform admin"
	case strings.HasSuffix(head, "boardWriteAuth") || strings.HasSuffix(head, "triggerWriteAuth"):
		return "session + write rung"
	case strings.Contains(head, "requireAuth"):
		return "session"
	case method == "OPTIONS" || isPublicPath(pattern) || isPublicMarketplaceRead(method, pattern):
		// Open at the operator-JWT gate — public bootstrap routes, the CORS
		// preflight responder, and the families that authenticate themselves
		// (webhook tokens, config-share iws_ tokens, per-run tokens).
		return "open"
	default:
		// A bare registration is still behind the mux-wide auth middleware;
		// the finer per-resource authorization is the curated page's prose.
		return "session"
	}
}

// GenerateRESTInventoryAnnex renders the annex from the tree rooted at
// root: this package's source scan, held against the committed openapi.json
// — a spec operation no registration owns fails the generation, and each
// registration's spec membership becomes the row's Spec column.
func GenerateRESTInventoryAnnex(root string) ([]byte, error) {
	scanned, err := scanRouteInventory(filepath.Join(root, "pkg", "server"))
	if err != nil {
		return nil, err
	}
	var routes []InventoryRoute
	for _, r := range scanned {
		if strings.HasPrefix(r.Pattern, "/api/") || r.Pattern == "/api" {
			routes = append(routes, r)
		}
	}
	// The floor is the scan's blindness net: a regex that stops reading
	// registrations renders a short annex that still says "exhaustive".
	if len(routes) < 380 {
		return nil, fmt.Errorf("the source scan found %d /api route(s) in pkg/server, floor 380 — the scan went blind and the annex would certify a fraction of the surface", len(routes))
	}
	spec, err := readSpecPairs(filepath.Join(root, "openapi.json"))
	if err != nil {
		return nil, err
	}
	for i, r := range routes {
		routes[i].InSpec = inSpec(r, spec)
	}
	if problems := specGhostCheck(routes, spec); len(problems) > 0 {
		return nil, fmt.Errorf("openapi.json publishes routes no pkg/server registration owns (run `task gen` to regenerate both):\n  %s", strings.Join(problems, "\n  "))
	}
	return []byte(renderRESTInventoryAnnex(routes)), nil
}

// specPair is one (method, path) the committed spec publishes. The
// comparison happens in SPEC form: the scan's side is rewritten with
// toOpenAPIPath (the same flattening buildOpenAPI applies), so this side
// reads the document verbatim.
type specPair struct {
	method  string
	pattern string
}

// readSpecPairs extracts every (method, path) of a committed OpenAPI
// document.
func readSpecPairs(path string) (map[specPair]bool, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- the committed spec at the repository root
	if err != nil {
		return nil, fmt.Errorf("read openapi.json: %w", err)
	}
	var doc struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse openapi.json: %w", err)
	}
	out := map[specPair]bool{}
	for p, item := range doc.Paths {
		for key := range item {
			if key == "parameters" {
				continue
			}
			out[specPair{strings.ToUpper(key), p}] = true
		}
	}
	if len(out) < 380 {
		return nil, fmt.Errorf("openapi.json carries %d operation(s), floor 380 — the parse went blind and the cross-check would pass on nothing", len(out))
	}
	return out, nil
}

// specPairsOf renders one registration in the spec's form: the pattern
// flattened by the same toOpenAPIPath buildOpenAPI applies, and a
// method-less registration expanded to the two verbs the spec publishes.
func specPairsOf(r InventoryRoute) []specPair {
	methods := []string{r.Method}
	if r.Method == "" {
		methods = methodsFor("")
	}
	out := make([]specPair, 0, len(methods))
	for _, m := range methods {
		out = append(out, specPair{m, toOpenAPIPath(r.Pattern)})
	}
	return out
}

// inSpec reports whether every operation of the registration is published.
func inSpec(r InventoryRoute, spec map[specPair]bool) bool {
	for _, pair := range specPairsOf(r) {
		if !spec[pair] {
			return false
		}
	}
	return true
}

// specGhostCheck refuses a spec row no registration owns: it would publish
// a route the binary never serves. The other direction — a registration the
// spec lacks — is the row's Spec column, not a refusal: families gated off
// in the offline spec build (the local-studio registrars, the DLQ, the
// cloud schedules) and the literal raw-mux registrations are served routes
// of the binary that the published spec legitimately lacks.
func specGhostCheck(routes []InventoryRoute, spec map[specPair]bool) []string {
	scanned := map[specPair]bool{}
	for _, r := range routes {
		for _, pair := range specPairsOf(r) {
			scanned[pair] = true
		}
	}
	var problems []string
	for pair := range spec {
		if !scanned[pair] {
			problems = append(problems, fmt.Sprintf("%s %s is published by openapi.json and no pkg/server registration owns it", pair.method, pair.pattern))
		}
	}
	sort.Strings(problems)
	if len(problems) > 20 {
		problems = append(problems[:20], fmt.Sprintf("… and %d more", len(problems)-20))
	}
	return problems
}

// renderRESTInventoryAnnex renders the annex: one section per family (the
// spec's own grouping), rows sorted by pattern then verb so two route
// additions never collide on a summary line — the body carries no counts.
func renderRESTInventoryAnnex(routes []InventoryRoute) string {
	sorted := make([]InventoryRoute, len(routes))
	copy(sorted, routes)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Pattern != sorted[j].Pattern {
			return sorted[i].Pattern < sorted[j].Pattern
		}
		return sorted[i].Method < sorted[j].Method
	})
	families := map[string][]InventoryRoute{}
	var order []string
	for _, r := range sorted {
		f := tagFor(r.Pattern)
		if _, ok := families[f]; !ok {
			order = append(order, f)
		}
		families[f] = append(families[f], r)
	}
	sort.Strings(order)

	var b strings.Builder
	b.WriteString(`# Cloud REST API — generated inventory

<!-- Generated by ` + "`iterion openapi inventory`" + ` (` + "`task restapi:gen`" + `) — do not edit by hand.
     The freshness and two-way gates of pkg/server/docs_rest_inventory_test.go fail on drift. -->

**Scope.** Every route this binary registers under ` + "`/api`" + ` with a literal
pattern — the recording mux's whole table, plus the few raw-mux literals —
each with its outer auth rung. The BULK of the ` + "`/api/v1/native`" + `,
` + "`/api/v1/dispatcher`" + ` and ` + "`/api/v1/mcp/{board,runs}`" + ` sub-trees registers
through non-literal patterns on the raw mux and stays out, exactly as it
stays out of the published spec — while their few literal registrations
(the forge-facing card routes of board_forge.go and pipeline_boards.go)
ARE in this inventory, marked ` + "`Spec: no`" + ` where the offline spec build
leaves them unwired. The curated [cloud-rest-api.md](cloud-rest-api.md)
carries what a generator cannot write: per-route authorization detail,
refusal reasons, pitfalls.

**Auth rung** is the OUTER gate a caller meets, read off the registration
line itself (a rung hidden behind a local variable or a closure refuses
the generation rather than print a wrong rung):

- ` + "`platform admin`" + ` — an authenticated platform super-admin (` + "`requireSuperAdmin`" + `).
- ` + "`session + write rung`" + ` — an authenticated member with the family's write rung
  (` + "`boardWriteAuth`" + ` / ` + "`triggerWriteAuth`" + `).
- ` + "`session`" + ` — any authenticated identity; the finer per-resource authorization
  is the curated page's prose.
- ` + "`open`" + ` — the operator-JWT gate lets the request through: public bootstrap
  routes, the CORS preflight responder, and the self-authenticating families
  (inbound webhook tokens, config-share ` + "`iws_`" + ` tokens, per-run tokens) the
  curated page names.

**Spec** marks whether the published [` + "`openapi.json`" + `](../openapi.json) carries the
route. ` + "`no`" + ` names a served route the offline spec build does not wire — a
family gated on a deployment shape or a literal raw-mux registration — so
a client generated from the spec will not know it. Those rows are
registered by ` + noSpecRegistrars(routes) + `.
`)
	for _, f := range order {
		b.WriteString("\n## `/api/")
		b.WriteString(f)
		b.WriteString("` family\n\n| Method | Path | Auth | Spec |\n|---|---|---|---|\n")
		for _, r := range families[f] {
			method := r.Method
			if method == "" {
				method = "ANY"
			}
			spec := "yes"
			if !r.InSpec {
				spec = "no"
			}
			fmt.Fprintf(&b, "| `%s` | `%s` | %s | %s |\n", method, r.Pattern, r.Rung, spec)
		}
	}
	return b.String()
}

// noSpecRegistrars names the registrar files that carry the rows the spec
// does not publish — derived from the rows themselves, so a family that
// joins or leaves the gated set moves the sentence with it.
func noSpecRegistrars(routes []InventoryRoute) string {
	seen := map[string]bool{}
	var files []string
	for _, r := range routes {
		if r.InSpec || seen[r.File] {
			continue
		}
		seen[r.File] = true
		files = append(files, r.File)
	}
	sort.Strings(files)
	if len(files) == 0 {
		return "no registrar — every route is published"
	}
	return "`" + strings.Join(files, "`, `") + "`"
}

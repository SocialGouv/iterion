package server

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// docs/cloud-rest-api.md is a CURATED reference — its own header says so,
// and names the families it leaves to the generated spec. Curated is a
// contract a reader relies on: every endpoint it lists "exists in
// pkg/server". Nothing held that half up, and the class is not
// hypothetical — #2401's item 24 was exactly this defect one layer over,
// a page describing a route the mux never registered.
//
// So this gate reads the page's own route literals and judges them
// against the same source-derived served table the CLI reachability gate
// uses (servedRouteTable). It is the FORWARD direction only: a documented
// route must be served. The other direction — how much of the 527-literal
// surface a curated page owes its reader — is a scope decision, not a
// test (see the wave report's ARB).

const restReferenceDoc = "cloud-rest-api.md"

// restRefPairFloor is the number of (method, path) pairs the page must
// still yield. Measured at 217 on 2026-10-29; the floor catches a parse
// that stops reading the tables and would otherwise judge nothing.
const restReferencePairFloor = 200

var (
	restRefMethodCell = regexp.MustCompile(`^(GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS)$`)
	restRefMethodSep  = regexp.MustCompile(`\s*(?:/|,|\bor\b)\s*`)
	restRefInline     = regexp.MustCompile("`(GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS) (/api/[^`]*)`")
)

// restRefCompoundCells expand the path cells that pack several routes
// into one, with the continuation form the page writes ("…/x" meaning the
// sibling of the first path). Listing them here rather than guessing
// keeps the gate honest in both directions: a NEW compound cell the
// parser cannot read is a failure that names itself, not a silent skip.
var restRefCompoundCells = map[string][]string{
	"/api/runs/{id}/files` / `…/files/content` / `…/files/diff": {
		"/api/runs/{id}/files",
		"/api/runs/{id}/files/content",
		"/api/runs/{id}/files/diff",
	},
	"/api/runs/{id}/review/scope` / `…/review/diff": {
		"/api/runs/{id}/review/scope",
		"/api/runs/{id}/review/diff",
	},
}

// restRefNotServedHere name the documented paths that are deliberately
// not routes of this mux, with the reason. First exact match wins.
var restRefNotServedHere = map[string]string{
	"/api/webhooks/{provider}/{id}": "the inbound webhook tree is mounted by its own registrar on the raw ServeMux, outside the recording mux this table is derived from",
}

type restRefRoute struct {
	method string
	path   string
	line   int
}

// restReferencePath locates the page from the package directory.
func restReferencePath(t *testing.T) string {
	t.Helper()
	p := filepath.Join("..", "..", "docs", restReferenceDoc)
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("stat %s: %v — the reference moved and this gate stopped judging it", p, err)
	}
	return p
}

// normalizeDocPath makes a documented literal comparable to a registered
// pattern: the page's angle-bracket placeholders are the mux's braces,
// and a documented query string is not part of the route.
func normalizeDocPath(raw string) string {
	p := strings.TrimSpace(raw)
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i]
	}
	p = strings.ReplaceAll(p, "<", "{")
	p = strings.ReplaceAll(p, ">", "}")
	return strings.TrimSuffix(p, "/")
}

// documentedRESTRoutes reads every (method, path) the page spells, from
// its tables and from its inline literals.
func documentedRESTRoutes(t *testing.T) []restRefRoute {
	t.Helper()
	body, err := os.ReadFile(restReferencePath(t))
	if err != nil {
		t.Fatalf("read the REST reference: %v", err)
	}
	var out []restRefRoute
	seen := map[restRefRoute]bool{}
	add := func(method, path string, line int) {
		r := restRefRoute{method: method, path: normalizeDocPath(path)}
		if r.path == "" || !strings.HasPrefix(r.path, "/api") {
			return
		}
		key := r
		if seen[key] {
			return
		}
		seen[key] = true
		r.line = line
		out = append(out, r)
	}

	for i, line := range strings.Split(string(body), "\n") {
		lineNo := i + 1
		for _, m := range restRefInline.FindAllStringSubmatch(line, -1) {
			// An ellipsis inside a literal of the PROSE is a shorthand
			// for a family the tables spell out ("the order is `PUT
			// /api/orgs/…` first"), not a route claim. In a table's path
			// CELL it is rot, and restRefCompoundCells below is where a
			// cell has to say what it means.
			if strings.Contains(m[2], "…") {
				continue
			}
			add(m[1], m[2], lineNo)
		}
		if !strings.HasPrefix(line, "|") {
			continue
		}
		cells := strings.Split(strings.Trim(strings.TrimSpace(line), "|"), "|")
		if len(cells) < 2 {
			continue
		}
		methodCell := strings.Trim(strings.TrimSpace(cells[0]), "`")
		pathCell := strings.Trim(strings.TrimSpace(cells[1]), "`")
		var verbs []string
		for _, v := range restRefMethodSep.Split(methodCell, -1) {
			v = strings.Trim(strings.TrimSpace(v), "`")
			if v == "" {
				continue
			}
			if !restRefMethodCell.MatchString(v) {
				verbs = nil
				break
			}
			verbs = append(verbs, v)
		}
		if len(verbs) == 0 || !strings.HasPrefix(pathCell, "/api") {
			continue
		}
		paths := []string{pathCell}
		if strings.ContainsAny(pathCell, "`…") {
			expanded, ok := restRefCompoundCells[pathCell]
			if !ok {
				t.Errorf("%s:%d packs several routes into one path cell this gate cannot read (%q): list its paths in restRefCompoundCells", restReferenceDoc, lineNo, pathCell)
				continue
			}
			paths = expanded
		}
		for _, v := range verbs {
			for _, p := range paths {
				add(v, p, lineNo)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].path != out[j].path {
			return out[i].path < out[j].path
		}
		return out[i].method < out[j].method
	})
	return out
}

// TestEveryRouteTheRESTReferenceDocumentsIsServed is the gate: a curated
// page that names a route the server does not register sends a reader
// (or an SDK author) at a 404, which is the defect item 24 of #2401 was.
func TestEveryRouteTheRESTReferenceDocumentsIsServed(t *testing.T) {
	documented := documentedRESTRoutes(t)
	if len(documented) < restReferencePairFloor {
		t.Fatalf("read %d (method, path) pairs from %s, want at least %d: the parse went blind, which would make this gate judge nothing", len(documented), restReferenceDoc, restReferencePairFloor)
	}

	served := servedRouteTable(t)
	byMethod := map[string][]string{}
	for _, r := range served {
		byMethod[r.Method] = append(byMethod[r.Method], r.Pattern)
	}

	matched := 0
	for _, doc := range documented {
		if reason, ok := restRefNotServedHere[doc.path]; ok {
			_ = reason
			continue
		}
		found := false
		for _, pattern := range byMethod[doc.method] {
			if ok, _ := servesPath(pattern, doc.path); ok {
				found = true
				break
			}
		}
		// A pattern registered without a method (the mux's bare form)
		// answers every verb.
		if !found {
			for _, pattern := range byMethod[""] {
				if ok, _ := servesPath(pattern, doc.path); ok {
					found = true
					break
				}
			}
		}
		if !found {
			t.Errorf("%s:%d documents %s %s and no registrar of this server routes it: fix the page, or restore the route", restReferenceDoc, doc.line, doc.method, doc.path)
			continue
		}
		matched++
	}
	if matched < restReferencePairFloor-len(restRefNotServedHere) {
		t.Fatalf("only %d documented route(s) matched a registered pattern: the served table went blind", matched)
	}
}

// TestTheRESTReferenceStillDeclaresItsCuratedScope pins the sentence the
// gate above rests on. A page that quietly drops "curated, not
// exhaustive" is claiming the whole surface, and the forward-only gate
// would then be the weaker half of a promise nobody made deliberately.
func TestTheRESTReferenceStillDeclaresItsCuratedScope(t *testing.T) {
	body, err := os.ReadFile(restReferencePath(t))
	if err != nil {
		t.Fatalf("read the REST reference: %v", err)
	}
	text := string(body)
	for _, claim := range []string{
		"curated, not exhaustive",
		"generated spec",
	} {
		if !strings.Contains(text, claim) {
			t.Errorf("%s no longer says %q: either it became exhaustive (then gate the other direction too) or the scope statement was lost", restReferenceDoc, claim)
		}
	}
}

// TestTheCompoundCellTableStillDescribesThePage keeps the expansion table
// from outliving the cells it reads: a stale entry makes the gate look
// wider than the page it judges.
func TestTheCompoundCellTableStillDescribesThePage(t *testing.T) {
	body, err := os.ReadFile(restReferencePath(t))
	if err != nil {
		t.Fatalf("read the REST reference: %v", err)
	}
	text := string(body)
	for cell := range restRefCompoundCells {
		if !strings.Contains(text, cell) {
			t.Errorf("restRefCompoundCells declares a cell the page no longer holds (%q): drop the entry", cell)
		}
	}
	for path, reason := range restRefNotServedHere {
		if reason == "" {
			t.Errorf("restRefNotServedHere entry %q carries no reason", path)
		}
	}
}

package serversurface

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/routescan"
	"github.com/SocialGouv/iterion/internal/sweepwalk"
)

// Item 24 of #2401: `iterion __scan-shards --mode=cloud` POSTed every shard to
// /api/v1/runs/launch, a path no mux registers — cloud shard dispatch answered
// 404 for each shard and nothing in either suite could see it. The CLI's tests
// drive the command against a fake endpoint, the server's tests drive the
// routes it does register, and the SEAM between them — the path literal one
// side spells and the other has to serve — was witnessed by nobody.
//
// That seam is the class, so the repair is this gate rather than one corrected
// literal: every /api/… path the CLI source spells must resolve against the
// union of the routing tables production mounts, and the union is DERIVED
// (routing table + registrar sources, internal/routescan), never a list
// copied here that would go stale the day a route moves. pkg/server's
// TestTheSourceDerivedTableCoversTheRunningOne certifies that same scanner
// against a running server's recorded table.
//
// Reach and limit, stated so nobody reads more into a green than it carries:
// the gate judges the PATH. A path that exists but serves another method, or
// a path assembled from a value this file cannot read statically, is outside
// it — the floors below are what keep a green honest about how much it saw.

// cliAPIPathLiteral matches an /api/… path literal in Go source. The CLI
// spells these as the first operand of the path argument, so a concatenated
// tail ("/api/runs/" + id) is read as the prefix it starts with.
var cliAPIPathLiteral = regexp.MustCompile(`"(/api/[^"]*)"`)

// cliSourceRoots are the two trees that spell the server's REST paths: the
// Cobra commands and the remote client they call, relative to the module
// root. The floor is the number of
// distinct literals each tree must still yield — a scan that stops finding
// them (a rename, a move to a generated client) fails instead of covering
// nothing.
var cliSourceRoots = []struct {
	dir   string
	floor int
}{
	{filepath.Join("cmd", "iterion"), 60},
	{filepath.Join("pkg", "cli"), 15},
}

// cliPathExemptions name the path literals that are NOT meant to resolve to a
// route of this server, with the reason. First matching prefix wins. A new
// unresolved literal is a defect until it is either served or named here.
var cliPathExemptions = []struct{ prefix, reason string }{
	{"/api/v1/mcp/", "the per-run MCP sub-trees are mounted on the raw *http.ServeMux by their own registrars (board + runs), outside both the recording mux and this union"},
}

// cliAPIPaths returns the distinct /api/… literals one source tree spells,
// dir relative to the module root.
func cliAPIPaths(t *testing.T, root, dir string, floor int) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, dir))
	if err != nil {
		t.Fatalf("read the CLI source tree %s: %v", dir, err)
	}
	seen := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		code, _ := routescan.RegistrarSource(t, filepath.Join(root, dir, name))
		for _, m := range cliAPIPathLiteral.FindAllStringSubmatch(code, -1) {
			seen[m[1]] = true
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	if len(out) < floor {
		t.Fatalf("%s yielded %d distinct /api/ path literal(s), floor %d — the scan no longer reads the CLI's paths, so a green here would mean nothing", dir, len(out), floor)
	}
	return out
}

// cliPathExemption returns the reason a literal is not expected to resolve,
// or "" when it is expected to.
func cliPathExemption(path string) string {
	for _, ex := range cliPathExemptions {
		if strings.HasPrefix(path, ex.prefix) {
			return ex.reason
		}
	}
	return ""
}

// cliConcatBases name the literals that are bases rather than requests and
// carry no trailing slash to say so — matched EXACTLY, never by prefix, so a
// real path under one of them is still judged as a path. Each entry is a
// reviewed statement that no request is sent to the bare literal; the routes
// UNDER it are still required to exist.
var cliConcatBases = map[string]string{
	"/api/orgs":          "tenancy-scope prefix of the members/invitations command builders (cmd/iterion/remote_identity.go), always concatenated with the resolved org id",
	"/api/v1/dispatcher": "mount prefix the local dispatch server hands to dispatcher.Manager.RegisterRoutes (pkg/cli/dispatch.go), not a request path",
	"/api/v1/native":     "mount prefix the local dispatch server hands to the native board registrar (pkg/cli/dispatch.go), not a request path",
}

// isConcatBase reports whether a literal is a base the CLI concatenates onto
// rather than a path it sends as it stands: the trailing slash is the usual
// spelling ("/api/runs/" + id), the table above holds the declared rest.
func isConcatBase(raw string) bool {
	if strings.HasSuffix(raw, "/") {
		return true
	}
	_, ok := cliConcatBases[raw]
	return ok
}

// normalizeCLIPath drops what the CLI appends to a path and the server never
// routes on: a query string, and the trailing slash of a concatenation
// prefix.
func normalizeCLIPath(path string) string {
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	if path != "/" {
		path = strings.TrimSuffix(path, "/")
	}
	return path
}

func TestEveryAPIPathTheCLISpellsIsServed(t *testing.T) {
	t.Parallel()
	root := sweepwalk.ModuleRoot(t)
	table := routescan.ServedRouteTable(t, root)
	resolved := 0
	for _, src := range cliSourceRoots {
		for _, raw := range cliAPIPaths(t, root, src.dir, src.floor) {
			path := normalizeCLIPath(raw)
			if path == "" || path == "/api" {
				continue
			}
			var served, extended bool
			for _, route := range table {
				s, e := routescan.ServesPath(route.Pattern, path)
				served = served || s
				extended = extended || e
			}
			// A literal the CLI concatenates onto is a base: what must exist
			// is a route CONTINUING it. A complete path must be served as
			// itself — see routescan.ServesPath.
			if served || (extended && isConcatBase(raw)) {
				resolved++
				continue
			}
			if reason := cliPathExemption(path); reason != "" {
				continue
			}
			detail := "no mux serves it"
			if extended {
				detail = "no mux serves it — only longer routes under it, so a request there answers 404 while the family looks alive"
			}
			t.Errorf("%s (%s): the CLI spells this path and %s; serve it, correct the literal, or name it in cliPathExemptions with its reason", raw, src.dir, detail)
		}
	}
	// The intersection, not the presence: a table read that silently returned
	// nothing would make every path above resolve against an empty set and the
	// loop would pass without having checked anything.
	if resolved < 70 {
		t.Fatalf("only %d CLI path(s) resolved against the routing table — the intersection collapsed, so this witness proves nothing", resolved)
	}
}

// cliMethodPath pairs a method with the path sent beside it, in the two
// spellings the CLI uses: the remote client's (method, path) arguments, and a
// request built from the server URL plus a path literal.
var cliMethodPathLiterals = []*regexp.Regexp{
	regexp.MustCompile(`"(GET|POST|PUT|DELETE|PATCH)",\s*"(/api/[^"]*)"`),
	regexp.MustCompile(`http\.Method(Get|Post|Put|Delete|Patch),\s*[A-Za-z_][A-Za-z0-9_]*\+"(/api/[^"]*)"`),
}

type cliCall struct{ method, path string }

// cliMethodPaths returns the (method, path) pairs a source tree spells with
// the two beside each other — the subset of paths whose METHOD this file can
// read, which is what lifts the check above from "the path exists" to "the
// request would be routed".
func cliMethodPaths(t *testing.T, root string) []cliCall {
	t.Helper()
	seen := map[cliCall]bool{}
	for _, src := range cliSourceRoots {
		entries, err := os.ReadDir(filepath.Join(root, src.dir))
		if err != nil {
			t.Fatalf("read the CLI source tree %s: %v", src.dir, err)
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			code, _ := routescan.RegistrarSource(t, filepath.Join(root, src.dir, name))
			for _, re := range cliMethodPathLiterals {
				for _, m := range re.FindAllStringSubmatch(code, -1) {
					seen[cliCall{strings.ToUpper(m[1]), m[2]}] = true
				}
			}
		}
	}
	out := make([]cliCall, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].path != out[j].path {
			return out[i].path < out[j].path
		}
		return out[i].method < out[j].method
	})
	return out
}

// TestEveryMethodAndPathTheCLISendsIsRouted is the method-level half: a path
// that exists on another verb routes the request nowhere. /api/runs serves
// GET and POST; a rename of the POST registration alone leaves the path
// resolvable and the launch dead — measured on this tree, and the path-level
// check above passed through it.
func TestEveryMethodAndPathTheCLISendsIsRouted(t *testing.T) {
	t.Parallel()
	root := sweepwalk.ModuleRoot(t)
	table := routescan.ServedRouteTable(t, root)
	pairs := cliMethodPaths(t, root)
	if len(pairs) < 60 {
		t.Fatalf("read %d (method, path) pair(s) from the CLI sources, floor 60 — the pairing went blind and this witness covers almost nothing", len(pairs))
	}
	for _, call := range pairs {
		path := normalizeCLIPath(call.path)
		if path == "" || path == "/api" {
			continue
		}
		routed := false
		for _, route := range table {
			// An empty recorded method is a registration without one: it
			// answers every verb.
			if route.Method != "" && route.Method != call.method {
				continue
			}
			served, extended := routescan.ServesPath(route.Pattern, path)
			if served || (extended && isConcatBase(call.path)) {
				routed = true
				break
			}
		}
		if routed {
			continue
		}
		if reason := cliPathExemption(path); reason != "" {
			continue
		}
		t.Errorf("%s %s: the CLI sends this request and no route serves that method on that path — the path may exist on another verb, which routes nothing; serve it, correct the call, or name it in cliPathExemptions with its reason", call.method, call.path)
	}
}

// TestTheDeclaredBasesAreStillBases reads the declaration table in BOTH
// directions, so it cannot rot into fiction: a declared base the CLI no
// longer spells is a stale waiver, and one that has GAINED a route of its own
// is a waiver that now hides a real path from the gate.
func TestTheDeclaredBasesAreStillBases(t *testing.T) {
	t.Parallel()
	root := sweepwalk.ModuleRoot(t)
	spelled := map[string]bool{}
	for _, src := range cliSourceRoots {
		for _, raw := range cliAPIPaths(t, root, src.dir, src.floor) {
			spelled[raw] = true
		}
	}
	table := routescan.ServedRouteTable(t, root)
	for base, reason := range cliConcatBases {
		if !spelled[base] {
			t.Errorf("%q is declared a concatenation base (%s) and no CLI source spells it any more — drop the entry", base, reason)
		}
		for _, route := range table {
			if served, _ := routescan.ServesPath(route.Pattern, base); served {
				t.Errorf("%q is declared a concatenation base (%s) but %s %s now serves it — drop the entry so the gate judges the path", base, reason, route.Method, route.Pattern)
				break
			}
		}
	}
}

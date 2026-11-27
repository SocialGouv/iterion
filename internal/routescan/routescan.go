// Package routescan derives the REST surface the iterion binary serves from
// the SOURCE of its registrars: the routes pkg/server registers as literal
// patterns, the native board registrar (pkg/dispatcher/native/http.go) and
// the dispatcher manager (pkg/dispatcher/manager.go), the two families
// mounted on the raw *http.ServeMux and so absent from the recording mux.
//
// It is test-support code, in the style of internal/sweepwalk: every entry
// point takes a testing.TB and fails it when a scan goes blind. It is a
// non-test package so that the sweeps judging a source against this table
// (internal/sweep/serversurface) and pkg/server's cross-check against a
// running server (TestTheSourceDerivedTableCoversTheRunningOne) run the SAME
// scanner: a _test.go helper cannot be imported across packages, and a copy
// would certify a twin. It must not import pkg/server.
//
// The table is read from the source and not from a running server on
// purpose: a registrar self-gates on the stores its family needs (an unwired
// usage-cap store registers no /api/admin/settings/usage-caps), so the table
// of any one instance is a DEPLOYMENT's surface, while the question a sweep
// asks is "does this binary serve it anywhere" — the union over every wiring.
package routescan

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Route is one registration: an HTTP method (empty = any) and the path
// pattern as registered (Go 1.22 ServeMux syntax, e.g. "/api/orgs/{id}").
type Route struct {
	Method  string
	Pattern string
}

// serverSourceFloor is the number of literal registrations the pkg/server
// scan must still find; below it the scan went blind.
const serverSourceFloor = 380

var (
	lineComments       = regexp.MustCompile(`(?m)//.*$`)
	registrationCalls  = regexp.MustCompile(`\.Handle(?:Func)?\(`)
	serverRegistration = regexp.MustCompile(`(?i:mux)\.Handle(?:Func)?\("(?:([A-Z]+) )?(/[^"]*)"`)
	prefixRegistration = regexp.MustCompile(`mux\.Handle\("([A-Z]+) "\+p\+"([^"]*)"`)
	dispatcherMount    = regexp.MustCompile(`Dispatcher\.RegisterRoutesWithMiddleware\(s\.mux\.ServeMux, "([^"]+)"`)
)

// RegistrarSource reads one registrar's source with its line comments
// stripped — a retired route quoted in a comment is not a registration — and
// counts the registration CALLS it holds, whatever the receiver is called —
// s.mux, s.mux.ServeMux, a local alias of either — so an alias cannot hide a
// registration from the count. A derivation parses one literal spelling; a
// call it counted but did not parse is a route it cannot guard, and the
// caller reddens on the difference rather than certifying a file it only
// half read.
func RegistrarSource(t testing.TB, path string) (string, int) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the registrar %s: %v", path, err)
	}
	code := lineComments.ReplaceAllString(string(raw), "")
	return code, len(registrationCalls.FindAllString(code, -1))
}

// ServerSourceRoutes derives every route pkg/server registers as a literal
// pattern, from the non-test sources of root/pkg/server.
func ServerSourceRoutes(t testing.TB, root string) []Route {
	t.Helper()
	dir := filepath.Join(root, "pkg", "server")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the server package: %v", err)
	}
	var out []Route
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		code, _ := RegistrarSource(t, filepath.Join(dir, name))
		for _, m := range serverRegistration.FindAllStringSubmatch(code, -1) {
			out = append(out, Route{Method: m[1], Pattern: m[2]})
		}
	}
	if len(out) < serverSourceFloor {
		t.Fatalf("the source scan found %d literal registration(s) in pkg/server, floor %d — the scan went blind and every path judged against it would read as unserved", len(out), serverSourceFloor)
	}
	return out
}

// NativeBoardAPIRoutes derives the cloud board's routes from the registrar
// that mounts them. They are registered on the raw *http.ServeMux
// (board_cloud_routes.go hands it s.mux.ServeMux), so the recording mux
// never sees them — a list written out instead would go stale the day a
// route is added. Every registration call the file holds must parse.
func NativeBoardAPIRoutes(t testing.TB, root string) []Route {
	t.Helper()
	src := filepath.Join(root, "pkg", "dispatcher", "native", "http.go")
	code, calls := RegistrarSource(t, src)
	matches := prefixRegistration.FindAllStringSubmatch(code, -1)
	if len(matches) != calls {
		t.Fatalf("%s holds %d registration call(s) and this witness parsed %d — a registration it cannot read is one it cannot guard; spell the route as one literal pattern on mux.Handle", src, calls, len(matches))
	}
	if len(matches) < 20 {
		t.Fatalf("derived %d routes from %s — the registrar's shape changed and this enumeration now covers almost nothing", len(matches), src)
	}
	out := make([]Route, 0, len(matches))
	for _, m := range matches {
		out = append(out, Route{Method: m[1], Pattern: "/api/v1/native" + m[2]})
	}
	return out
}

// DispatcherManagerRoutes derives the dispatcher's REST surface from the
// registrar that mounts it. The server hands it s.mux.ServeMux (the recording
// mux never sees these routes, pkg/server/server_routes.go), so the routing
// table alone would read the whole family as unserved.
func DispatcherManagerRoutes(t testing.TB, root string) []Route {
	t.Helper()
	// The prefix is the server's: a mount moved to another prefix has to
	// move here too, and the assertion says so.
	routesSrc, _ := RegistrarSource(t, filepath.Join(root, "pkg", "server", "server_routes.go"))
	mounted := dispatcherMount.FindStringSubmatch(routesSrc)
	if mounted == nil {
		t.Fatalf("pkg/server/server_routes.go no longer mounts the dispatcher through RegisterRoutesWithMiddleware with a literal prefix — this derivation cannot name the family's paths any more")
	}
	prefix := strings.TrimSuffix(mounted[1], "/")

	src := filepath.Join(root, "pkg", "dispatcher", "manager.go")
	code, _ := RegistrarSource(t, src)
	// The registrar's own spelling: mux.Handle("<METHOD> "+p+"<tail>", …).
	matches := prefixRegistration.FindAllStringSubmatch(code, -1)
	if len(matches) < 12 {
		t.Fatalf("derived %d route(s) from %s — the registrar's shape changed and the dispatcher family is no longer covered", len(matches), src)
	}
	out := make([]Route, 0, len(matches))
	for _, m := range matches {
		out = append(out, Route{Method: m[1], Pattern: prefix + m[2]})
	}
	return out
}

// ServedRouteTable is the union of every table production mounts: the routes
// pkg/server registers plus the registrars of other packages that mount on
// the raw *http.ServeMux and are absent from the recording mux.
func ServedRouteTable(t testing.TB, root string) []Route {
	t.Helper()
	table := ServerSourceRoutes(t, root)
	table = append(table, NativeBoardAPIRoutes(t, root)...)
	table = append(table, DispatcherManagerRoutes(t, root)...)
	return table
}

// ServesPath reports how a registered pattern answers a path: served when it
// IS that path (a {wildcard} segment matches the literal's segment), extended
// when it continues it — the shape of the prefixes a client concatenates an
// id onto ("/api/runs/" + id).
//
// The distinction is the sweeps' bite. Judging a complete path by its prefix
// alone makes a rename invisible: "/api/runs" would still "resolve" through
// its own children ("/api/runs/{id}/events") on a server that no longer
// serves POST /api/runs at all.
func ServesPath(pattern, path string) (served, extended bool) {
	pSegs := strings.Split(strings.Trim(pattern, "/"), "/")
	lSegs := strings.Split(strings.Trim(path, "/"), "/")
	if len(pSegs) < len(lSegs) {
		return false, false
	}
	for i, seg := range lSegs {
		got := pSegs[i]
		if strings.HasPrefix(got, "{") && strings.HasSuffix(got, "}") {
			continue
		}
		if got != seg {
			return false, false
		}
	}
	if len(pSegs) == len(lSegs) {
		return true, false
	}
	return false, true
}

package server

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/routescan"
	"github.com/SocialGouv/iterion/internal/sweepwalk"
)

// serverSourceRoutes derives every route pkg/server registers as a literal
// pattern, from the package's own source (routescan.ServerSourceRoutes, the
// scanner the module-wide route sweeps of internal/sweep/serversurface judge
// the CLI and the REST reference against).
func serverSourceRoutes(t *testing.T) []RouteInfo {
	t.Helper()
	return routeInfos(routescan.ServerSourceRoutes(t, sweepwalk.ModuleRoot(t)))
}

// TestTheSourceDerivedTableCoversTheRunningOne proves the source scan is the
// table it claims to be: every route a running server RECORDED must be one
// the scan found. A pattern the scan's regex cannot read is a hole in every
// gate built on it — it would read a served path as dead, or (the dangerous
// direction) let a dead one pass because some unparsed sibling looked like a
// prefix. It needs a running *Server, so it lives here, and it certifies the
// SAME scanner (internal/routescan) the sweeps of
// internal/sweep/serversurface use. (The studio's SPA shell routes are
// mounted from a list of UI paths and carry no /api prefix; the REST surface
// the sweeps judge is the /api tree.)
func TestTheSourceDerivedTableCoversTheRunningOne(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t)
	derived := map[routescan.Route]bool{}
	for _, r := range routescan.ServerSourceRoutes(t, sweepwalk.ModuleRoot(t)) {
		derived[r] = true
	}
	compared := 0
	for _, r := range srv.mux.Routes() {
		if !strings.HasPrefix(r.Pattern, "/api/") {
			continue
		}
		compared++
		if !derived[routescan.Route{Method: r.Method, Pattern: r.Pattern}] {
			t.Errorf("%s %s is registered at run time and the source scan of pkg/server did not find it — spell the pattern as one string literal on mux.Handle, or this gate is blind to its family", r.Method, r.Pattern)
		}
	}
	if compared < 100 {
		t.Fatalf("cross-checked %d live /api route(s) — too few for this to witness anything about the source scan", compared)
	}
}

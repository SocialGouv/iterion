package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestNodeChangesRoutesAreRegistered is Revi's R354aa7.
//
// The two handlers existed but were never wired onto the mux — dropped by
// a merge resolution — so every getNodeChanges call from the Files tab hit
// an unrouted path and the panel showed a permanent warning for every node
// of every run. It stayed invisible end to end: openapi.json and schema.ts
// lost their entries in the same resolution, so `task openapi:check`
// regenerated a spec matching the broken reality and CI went green, and
// the Go tests called the Service directly rather than the HTTP surface.
//
// This asserts the ROUTES exist, which a symbol-level check cannot: the
// handler functions were present the whole time.
func TestNodeChangesRoutesAreRegistered(t *testing.T) {
	srv, _ := newTestServer(t)
	for _, path := range []string{
		"/api/runs/some-run/nodes/implement/changes",
		"/api/runs/some-run/nodes/implement/diff?path=a.txt",
	} {
		rec := httptest.NewRecorder()
		srv.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		// An unrouted path gets the mux's own catch-all, which says so in
		// as many words. A registered handler reaching a missing run also
		// 404s, so the BODY is the discriminator, not the status.
		if strings.Contains(rec.Body.String(), "no such API endpoint") {
			t.Errorf("%s is not routed — the mux answered %q before any handler ran",
				path, strings.TrimSpace(rec.Body.String()))
		}
	}
}

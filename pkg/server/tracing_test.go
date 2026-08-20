package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	sentry "github.com/getsentry/sentry-go"

	"github.com/SocialGouv/iterion/pkg/errtrack"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// tracedServer builds a Server whose full handler chain (the one
// http.Server is given) is exercised — not srv.mux, which every other
// test substitutes to bypass auth.
func tracedServer(t *testing.T) *Server {
	t.Helper()
	workDir := t.TempDir()
	return New(Config{
		DisableAuth:             true,
		WorkDir:                 workDir,
		StoreDir:                filepath.Join(workDir, ".iterion"),
		SkipProjectRegistration: true,
	}, iterlog.New(iterlog.LevelError, os.Stderr))
}

func newTransactions(before, after []*sentry.Event) []*sentry.Event {
	var out []*sentry.Event
	for _, e := range after[len(before):] {
		if e.Type == "transaction" {
			out = append(out, e)
		}
	}
	return out
}

// The wiring proof: an API request served by the REAL handler chain
// (tracing middleware → auth → mux) becomes one transaction, named
// after the route pattern rather than the URL — so a thousand run ids
// stay one entry in the performance view instead of a thousand.
func TestAPIRequestBecomesOneRouteNamedTransaction(t *testing.T) {
	tr := enableTracker(t)
	if !errtrack.TracingEnabled() {
		t.Fatal("tracing is off — the test binary's Init did not set a sample rate")
	}
	srv := tracedServer(t)
	seedRun(t, srv, "run-traced-1", "demo", store.RunStatusFinished)
	before := tr.all()

	rec := httptest.NewRecorder()
	srv.handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/runs/run-traced-1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d — the middleware changed the response", rec.Code)
	}
	errtrack.Flush()

	txns := newTransactions(before, tr.all())
	if len(txns) != 1 {
		t.Fatalf("want 1 transaction for 1 request, got %d", len(txns))
	}
	if txns[0].Transaction != "GET /api/runs/{id}" {
		t.Errorf("transaction name = %q, want the route pattern", txns[0].Transaction)
	}
}

// routePattern is what keeps transaction cardinality bounded; it must
// resolve the mux's registered pattern for a request the auth layer may
// never forward, and stay quiet on a path nothing serves.
func TestRoutePatternResolvesTheRegisteredPattern(t *testing.T) {
	srv := tracedServer(t)

	if got := srv.routePattern(httptest.NewRequest(http.MethodGet, "/api/runs/abc123", nil)); got != "GET /api/runs/{id}" {
		t.Errorf("routePattern = %q", got)
	}
	// Anything the API does not claim falls to the SPA catch-all —
	// still one name, which is the whole point.
	if got := srv.routePattern(httptest.NewRequest(http.MethodGet, "/some/spa/deep/link", nil)); got != "GET /" {
		t.Errorf("routePattern on an SPA path = %q, want the catch-all", got)
	}
	var nilSrv *Server
	if got := nilSrv.routePattern(httptest.NewRequest(http.MethodGet, "/api/runs/abc", nil)); got != "" {
		t.Errorf("routePattern on a nil server = %q", got)
	}
}

package server

import (
	"bytes"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/identity"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/pat"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// newSweepServer wires the credential + identity stack so routes() registers
// the groups that are conditional on it — BYOK, org credentials, generic
// secrets, OAuth forfaits, PATs, webhook configs. The bare newTestServer
// registers 70 routes; those conditional groups are exactly the ones that had
// no origin guard, so sweeping without them would miss the point.
func newSweepServer(t *testing.T, opts ...func(*Config)) *Server {
	t.Helper()
	key := bytes.Repeat([]byte{7}, 32)
	signer, err := auth.NewJWTSigner(base64.RawStdEncoding.EncodeToString(key), 15*time.Minute)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	svc, err := auth.NewService(auth.Config{
		Store:      identity.NewMemoryStore(),
		Sessions:   auth.NewMemorySessionStore(),
		Signer:     signer,
		SignupMode: auth.SignupOpen,
		RefreshTTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("auth service: %v", err)
	}
	sealer, err := secrets.NewAESGCMSealer(key)
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	// A REAL native tracker store, so /api/v1/native/* and the board-MCP
	// transport are actually registered. Without it the OPTIONS assertions
	// below would pass for the trivial reason that nothing matches those
	// paths — the "test that proves nothing" shape this file exists to refuse.
	nativeStore, err := native.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("native store: %v", err)
	}
	cfg := Config{
		NativeTrackerStore:      nativeStore,
		WorkDir:                 t.TempDir(),
		Bind:                    "127.0.0.1",
		SkipProjectRegistration: true,
		AuthService:             svc,
		AuthSigner:              signer,
		PATs:                    pat.NewMemoryStore(),
		ApiKeys:                 secrets.NewMemoryApiKeyStore(),
		GenericSecrets:          secrets.NewMemoryGenericSecretStore(),
		Sealer:                  sealer,
		OAuthForfait:            secrets.NewMemoryOAuthStore(),
		OAuthPending:            secrets.NewMemoryOAuthPendingStore(),
		BotBindings:             secrets.NewMemoryBotSecretBindingStore(),
		WebhookConfigs:          webhooks.NewMemoryConfigStore(),
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	return New(cfg, iterlog.New(iterlog.LevelError, nil))
}

// foreignOrigin is a sibling host under the SAME registrable domain as the
// deployment's historical public host (iterion.fabrique.social.gouv.fr, whose
// site is social.gouv.fr because gouv.fr is a public suffix). That is the case
// SameSite=Lax does NOT cover: the browser treats such a request as same-site
// and attaches the session cookie, so only an Origin check can refuse it.
const foreignOrigin = "https://evil.fabrique.social.gouv.fr"

const sweepHost = "iterion.fabrique.social.gouv.fr"

// concretePath turns a Go 1.22 ServeMux pattern into a requestable path by
// filling every wildcard: "/api/orgs/{id}/oauth/{kind}" → "/api/orgs/x/oauth/x".
func concretePath(pattern string) string {
	var b strings.Builder
	for {
		open := strings.IndexByte(pattern, '{')
		if open < 0 {
			b.WriteString(pattern)
			return b.String()
		}
		end := strings.IndexByte(pattern[open:], '}')
		if end < 0 {
			b.WriteString(pattern)
			return b.String()
		}
		b.WriteString(pattern[:open])
		b.WriteString("x")
		pattern = pattern[open+end+1:]
	}
}

// unrecordedSubtrees are state-changing /api paths that the recording mux
// CANNOT see: the native board, the dispatcher and the board-MCP transport are
// handed the concrete *http.ServeMux and register on it directly, which is why
// the published OpenAPI says they are "served but registered on a separate
// mux". The gate covers them anyway — it is a path+method predicate evaluated
// before routing — but "anyway" is exactly the kind of claim that should be
// asserted rather than reasoned about, so they are swept explicitly.
var unrecordedSubtrees = []RouteInfo{
	{Method: http.MethodPost, Pattern: "/api/v1/native/issues"},
	{Method: http.MethodPost, Pattern: "/api/v1/native/issues/x/transition"},
	{Method: http.MethodPost, Pattern: "/api/v1/dispatcher/refresh"},
	{Method: http.MethodPost, Pattern: "/api/v1/mcp/board"},
}

// sweptRoutes returns every state-changing /api route this server exposes:
// the recorded routing table, plus the sub-trees above that it cannot record.
// Reading the table (rather than a hand-kept list) is the point — a route
// added tomorrow is swept tomorrow, with no edit here.
func sweptRoutes(t *testing.T, srv *Server) []RouteInfo {
	t.Helper()
	out := append([]RouteInfo(nil), unrecordedSubtrees...)
	for _, rt := range srv.mux.Routes() {
		if !strings.HasPrefix(rt.Pattern, "/api/") {
			continue
		}
		method := rt.Method
		if method == "" {
			// A method-less registration answers every method, POST included.
			method = http.MethodPost
		}
		if !isStateChangingMethod(method) {
			continue
		}
		out = append(out, RouteInfo{Method: method, Pattern: rt.Pattern})
	}
	return out
}

// TestEveryStateChangingAPIRouteRefusesForeignOrigin is the guard that keeps
// the CSRF hole from growing back. It sweeps the live routing table and
// requires a 403 from EVERY state-changing /api route when the request carries
// a foreign Origin.
//
// It exists because the previous defence — a per-handler requireSafeOrigin
// call — was opt-in, and opt-in drifted: 70 of 247 state-changing routes had
// it, leaving the credential, secret, OAuth and org-admin endpoints reachable
// by a same-site cross-origin POST. A per-handler fix would have drifted the
// same way; this test is what makes the guarantee hold for the route nobody
// remembered to guard.
//
// The request is shaped like the real attack: Content-Type text/plain makes it
// a CORS "simple request", so the browser sends it with NO preflight, and the
// JSON decoders here never inspect Content-Type.
//
// Running it through srv.handler (the composed chain) rather than the gate
// alone is deliberate — it proves the gate is actually WIRED. It is also free
// of side effects precisely because the gate refuses before any handler runs.
func TestEveryStateChangingAPIRouteRefusesForeignOrigin(t *testing.T) {
	srv := newSweepServer(t)

	routes := sweptRoutes(t, srv)
	// A sweep that matched nothing — or that quietly stopped registering the
	// conditional groups — would pass while proving nothing.
	if len(routes) < 150 {
		t.Fatalf("swept only %d state-changing /api routes; expected 150+ — the sweep is not seeing the real routing table", len(routes))
	}
	// Name the routes this test exists for. A refactor that stops registering
	// them must fail here rather than silently shrink the sweep.
	seen := make(map[string]bool, len(routes))
	for _, rt := range routes {
		seen[rt.Method+" "+rt.Pattern] = true
	}
	for _, must := range []string{
		"POST /api/me/api-keys",
		"POST /api/me/secrets",
		"POST /api/teams/{id}/api-keys",
		"POST /api/teams/{id}/secrets",
		"POST /api/orgs/{id}/api-keys",
		"POST /api/auth/login",
		// The sub-trees the recording mux cannot see.
		"POST /api/v1/native/issues",
		"POST /api/v1/mcp/board",
	} {
		if !seen[must] {
			t.Fatalf("%q is not in the swept set — the sweep no longer covers the endpoints it was written for", must)
		}
	}

	for _, rt := range routes {
		t.Run(rt.Method+" "+rt.Pattern, func(t *testing.T) {
			r := httptest.NewRequest(rt.Method, concretePath(rt.Pattern), strings.NewReader("{}"))
			r.Host = sweepHost
			r.Header.Set("Origin", foreignOrigin)
			r.Header.Set("Content-Type", "text/plain")
			w := httptest.NewRecorder()
			srv.handler.ServeHTTP(w, r)
			if w.Code != http.StatusForbidden {
				t.Errorf("%s %s: cross-origin status = %d; want 403 — this route is CSRF-reachable from any same-site sibling host",
					rt.Method, rt.Pattern, w.Code)
			}
		})
	}
}

// TestOriginGateAdmitsLegitimateCallers is the other half: the gate must not
// refuse the callers the product depends on. Asserted against the gate itself
// rather than the composed handler so no business handler executes (and so a
// failure names the gate, not a downstream 500).
func TestOriginGateAdmitsLegitimateCallers(t *testing.T) {
	srv := newSweepServer(t)
	routes := sweptRoutes(t, srv)

	callers := []struct {
		name   string
		origin string
		host   string
	}{
		// The studio SPA on either public host — same-origin, no config needed.
		{"SPA on the canonical host", "https://iterion.cloud", "iterion.cloud"},
		{"SPA on the historical host", "https://" + sweepHost, sweepHost},
		// The CLI, runner pods and forge webhooks send no Origin at all.
		{"non-browser caller (CLI, runner, webhook)", "", "iterion.cloud"},
		// The desktop app's WebView origin.
		{"desktop wails", "wails://wails", "localhost:4891"},
	}

	for _, c := range callers {
		t.Run(c.name, func(t *testing.T) {
			for _, rt := range routes {
				r := httptest.NewRequest(rt.Method, concretePath(rt.Pattern), strings.NewReader("{}"))
				r.Host = c.host
				if c.origin != "" {
					r.Header.Set("Origin", c.origin)
				}
				w := httptest.NewRecorder()
				if !srv.originGateAllows(w, r) {
					t.Fatalf("%s %s: gate refused %s (status=%d); this breaks a shipped client",
						rt.Method, rt.Pattern, c.name, w.Code)
				}
			}
		})
	}
}

// TestOriginGateSweepBites falsifies the sweep above: with the kill switch on,
// the very same request must stop being a 403. Without this, a sweep that
// 403'd for some unrelated reason would look like a passing guard.
func TestOriginGateSweepBites(t *testing.T) {
	srv := newSweepServer(t)
	const path = "/api/me/api-keys"

	probe := func() int {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}"))
		r.Host = sweepHost
		r.Header.Set("Origin", foreignOrigin)
		r.Header.Set("Content-Type", "text/plain")
		w := httptest.NewRecorder()
		srv.handler.ServeHTTP(w, r)
		return w.Code
	}

	if got := probe(); got != http.StatusForbidden {
		t.Fatalf("gate armed: status = %d; want 403", got)
	}
	t.Setenv("ITERION_REQUIRE_ORIGIN", "0")
	if got := probe(); got == http.StatusForbidden {
		t.Fatal("kill switch set but the request is still 403 — the 403 does not come from the origin gate, so the sweep proves nothing")
	}
}

// TestSafeMethodsAreNotGated keeps the gate off the read path: a GET carrying
// a foreign Origin must still be routed (CORS already stops the attacker from
// READING the response, and gating GET would break ordinary navigation).
func TestSafeMethodsAreNotGated(t *testing.T) {
	srv := newSweepServer(t)
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		r := httptest.NewRequest(method, "/api/runs", nil)
		r.Host = sweepHost
		r.Header.Set("Origin", foreignOrigin)
		w := httptest.NewRecorder()
		if !srv.originGateAllows(w, r) {
			t.Errorf("%s was gated; safe methods must pass through", method)
		}
	}
}

// TestNonAPIPathsAreNotGated keeps the SPA itself reachable — the gate is an
// API boundary, not a static-asset one.
func TestNonAPIPathsAreNotGated(t *testing.T) {
	srv := newSweepServer(t)
	for _, path := range []string{"/", "/index.html", "/assets/index.js", "/board"} {
		r := httptest.NewRequest(http.MethodPost, path, nil)
		r.Host = sweepHost
		r.Header.Set("Origin", foreignOrigin)
		w := httptest.NewRecorder()
		if !srv.originGateAllows(w, r) {
			t.Errorf("%s was gated; the gate must only cover /api/", path)
		}
	}
}

// TestNoMethodlessAPIRegistration pins the property the OPTIONS exemption
// rests on.
//
// authMiddleware lets every OPTIONS through unauthenticated, which is safe
// only because an OPTIONS under /api/ can then match nothing but the
// `OPTIONS /api/` preflight responder. That holds because every production
// route declares its method — including the sub-trees registered on the raw
// ServeMux, whose own comment says "one pattern per (method, path)". True
// today, but a property of the route table rather than of any code, so a
// method-less registration added tomorrow would silently hand an
// unauthenticated OPTIONS to a real handler. Raised by Revi on the PR.
func TestNoMethodlessAPIRegistration(t *testing.T) {
	srv := newSweepServer(t)
	for _, rt := range srv.mux.Routes() {
		if strings.HasPrefix(rt.Pattern, "/api/") && rt.Method == "" {
			t.Errorf("%q is registered with no method: an unauthenticated OPTIONS would reach its handler instead of the preflight responder", rt.Pattern)
		}
	}
}

// TestOptionsReachesOnlyThePreflightResponder is the runtime half, and it
// covers the sub-trees the recording mux cannot see. The preflight responder
// answers 204 with an empty body; any other status means an OPTIONS found a
// business handler.
func TestOptionsReachesOnlyThePreflightResponder(t *testing.T) {
	srv := newSweepServer(t)
	paths := []string{
		"/api/runs",
		"/api/me/api-keys",
		"/api/auth/login",
		// The sub-trees registered on the concrete ServeMux.
		"/api/v1/native/issues",
		"/api/v1/dispatcher/refresh",
		"/api/v1/mcp/board",
	}
	// Prove the sub-trees are really mounted: a 405/404 on the POST would mean
	// the OPTIONS assertions below are vacuous.
	for _, p := range []string{"/api/v1/native/issues", "/api/v1/mcp/board"} {
		probe := httptest.NewRequest(http.MethodPost, p, strings.NewReader("{}"))
		probe.Host = sweepHost
		pw := httptest.NewRecorder()
		srv.handler.ServeHTTP(pw, probe)
		if pw.Code == http.StatusNotFound || pw.Code == http.StatusMethodNotAllowed {
			t.Fatalf("POST %s -> %d: the sub-tree is not registered on this server, so the OPTIONS assertions would prove nothing", p, pw.Code)
		}
	}

	for _, p := range paths {
		r := httptest.NewRequest(http.MethodOptions, p, nil)
		r.Host = sweepHost
		r.Header.Set("Origin", foreignOrigin)
		r.Header.Set("Access-Control-Request-Method", "POST")
		w := httptest.NewRecorder()
		srv.handler.ServeHTTP(w, r)

		if w.Code != http.StatusNoContent {
			t.Errorf("OPTIONS %s: status %d; want 204 — it did not land on the preflight responder", p, w.Code)
		}
		if body := w.Body.String(); body != "" {
			t.Errorf("OPTIONS %s: body %q; the preflight responder writes none, so a handler answered", p, body)
		}
		// And it must not hand a foreign origin an ACAO.
		if acao := w.Header().Get("Access-Control-Allow-Origin"); acao != "" {
			t.Errorf("OPTIONS %s: ACAO %q for a foreign origin", p, acao)
		}
	}
}

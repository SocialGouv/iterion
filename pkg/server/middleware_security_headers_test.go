package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// cspDirective returns the source list of one directive, or "" when the policy
// does not declare it.
func cspDirective(csp, name string) string {
	for _, part := range strings.Split(csp, ";") {
		part = strings.TrimSpace(part)
		if rest, ok := strings.CutPrefix(part, name+" "); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

// TestSecurityHeadersOnDocuments locks the header set the studio serves. Before
// this middleware the only one present in production was HSTS (from the
// ingress), so the SPA was framable and nothing confined an XSS.
func TestSecurityHeadersOnDocuments(t *testing.T) {
	srv := newSweepServer(t)
	for _, path := range []string{"/", "/index.html", "/board"} {
		t.Run(path, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, path, nil)
			w := httptest.NewRecorder()
			srv.handler.ServeHTTP(w, r)

			want := map[string]string{
				"X-Content-Type-Options": "nosniff",
				"Referrer-Policy":        "strict-origin-when-cross-origin",
				"X-Frame-Options":        "SAMEORIGIN",
			}
			for k, v := range want {
				if got := w.Header().Get(k); got != v {
					t.Errorf("%s = %q; want %q", k, got, v)
				}
			}
			csp := w.Header().Get("Content-Security-Policy")
			if csp == "" {
				t.Fatal("no Content-Security-Policy on a document response")
			}
			// The directives that carry the guarantee. Named individually so a
			// weakening edit (adding 'unsafe-inline', dropping frame-ancestors)
			// fails here and not in a browser months later.
			for _, directive := range []string{
				"default-src 'self'",
				"script-src 'self'",
				"frame-ancestors 'self'",
				"object-src 'none'",
				"base-uri 'self'",
			} {
				if !strings.Contains(csp, directive) {
					t.Errorf("CSP missing %q; got %q", directive, csp)
				}
			}
			// 'unsafe-eval' is never acceptable, anywhere: it re-opens
			// string-to-code execution, which is the whole point of the policy.
			// Nor is any CDN — Monaco is bundled.
			for _, forbidden := range []string{"'unsafe-eval'", "cdn.jsdelivr.net", "unpkg.com"} {
				if strings.Contains(csp, forbidden) {
					t.Errorf("CSP contains %q", forbidden)
				}
			}
			// script-src carries the guarantee, so it is asserted by exact
			// source list rather than by substring: 'unsafe-inline' there would
			// turn an injected string back into executing code. style-src does
			// carry 'unsafe-inline' (measured: the CSS-in-JS in the dependency
			// tree injects <style> at runtime) and that is a separate, lower
			// exposure — see the policy comment.
			if got := cspDirective(csp, "script-src"); got != "'self'" {
				t.Errorf("script-src = %q; want exactly \"'self'\"", got)
			}
		})
	}
}

// TestSecurityHeadersRideShortCircuitedResponses checks the headers are on the
// responses the middleware stack refuses, not only the ones it serves — that is
// why securityHeaders is wired outermost.
func TestSecurityHeadersRideShortCircuitedResponses(t *testing.T) {
	srv := newSweepServer(t)

	t.Run("403 from the origin gate", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/api/me/api-keys", strings.NewReader("{}"))
		r.Host = sweepHost
		r.Header.Set("Origin", foreignOrigin)
		w := httptest.NewRecorder()
		srv.handler.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Fatalf("status = %d; want 403", w.Code)
		}
		if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("X-Content-Type-Options = %q on a 403; want nosniff", got)
		}
	})

	t.Run("401 from the auth gate", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/api/runs", nil)
		w := httptest.NewRecorder()
		srv.handler.ServeHTTP(w, r)
		if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("X-Content-Type-Options = %q on a %d; want nosniff", got, w.Code)
		}
	})
}

// TestSecurityHeadersKillSwitch proves the headers come from THIS middleware:
// with the switch off they must disappear. Without it, headers set by some
// other layer would look like this one working.
func TestSecurityHeadersKillSwitch(t *testing.T) {
	srv := newSweepServer(t)
	probe := func() string {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		w := httptest.NewRecorder()
		srv.handler.ServeHTTP(w, r)
		return w.Header().Get("Content-Security-Policy")
	}
	if probe() == "" {
		t.Fatal("no CSP with the middleware armed")
	}
	t.Setenv("ITERION_SECURITY_HEADERS", "0")
	if got := probe(); got != "" {
		t.Fatalf("kill switch set but CSP is still %q — it does not come from securityHeaders", got)
	}
}

// newPreviewCSPServer is the sweep server with a real run store and the
// dev-mode identity: the preview endpoint is authenticated, and these tests
// are about the response HEADERS, not the auth gate (which security.origin-gate
// and the auth tests already pin).
func newPreviewCSPServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	return newSweepServer(t, func(c *Config) {
		c.DisableAuth = true
		c.WorkDir = dir
		c.StoreDir = filepath.Join(dir, ".iterion")
	})
}

// TestRunPreviewKeepsItsOwnCSP guards the override contract: the preview
// endpoint serves attacker-influenced run output and replaces the studio
// policy with a sandbox one. The middleware runs first and must not win.
//
// It drives the endpoint through the composed handler. An earlier version
// compared the studio policy against a `const previewCSP` it declared itself
// and asserted that literal contained "sandbox" — coupled to nothing, and
// proven tautological by an adversarial review that deleted the production
// `Set` and watched the test still pass.
func TestRunPreviewKeepsItsOwnCSP(t *testing.T) {
	srv := newPreviewCSPServer(t)

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	srv.handler.ServeHTTP(w, r)
	studioCSP := w.Header().Get("Content-Security-Policy")
	if studioCSP == "" {
		t.Fatal("no CSP on the studio document")
	}

	// The override is written on the SUCCESS path only, so the test has to
	// reach it: a real run in the store and a real upstream to proxy. An
	// earlier attempt asserted against an error response and read back the
	// studio policy — the handler had returned long before its own Set.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<html><body>dev server</body></html>"))
	}))
	t.Cleanup(upstream.Close)

	seedRun(t, srv, "preview-csp-run", "wf", store.RunStatusFinished)

	pr := httptest.NewRequest(http.MethodGet, "/api/runs/preview-csp-run/preview?target="+url.QueryEscape(upstream.URL+"/"), nil)
	pw := httptest.NewRecorder()
	srv.handler.ServeHTTP(pw, pr)
	if pw.Code != http.StatusOK {
		t.Fatalf("preview did not reach its success path: status %d, body %s", pw.Code, pw.Body.String())
	}

	previewCSP := pw.Header().Get("Content-Security-Policy")
	if previewCSP == studioCSP {
		t.Fatalf("the preview response carries the STUDIO policy (%q); its own override is gone", previewCSP)
	}
	if !strings.Contains(previewCSP, "sandbox") {
		t.Fatalf("preview policy no longer sandboxes: %q", previewCSP)
	}
}

// TestPreviewProxyCannotOverrideOurSecurityHeaders pins the strip list. The
// proxy Adds upstream headers, and the upstream is attacker-influenced (a run
// picks the target), so a hostile second value arrives beside ours and the
// browser resolves the pair in the sender's favour.
//
// Found by adversarial review: an upstream returning `Permissions-Policy:
// camera=*` re-enabled the camera against our `camera=()`, and `Referrer-Policy:
// unsafe-url` leaked the full preview URL to a third party.
func TestPreviewProxyCannotOverrideOurSecurityHeaders(t *testing.T) {
	srv := newPreviewCSPServer(t)

	hostile := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Permissions-Policy", "camera=*")
		w.Header().Set("Referrer-Policy", "unsafe-url")
		w.Header().Set("X-Content-Type-Options", "MANGLED")
		_, _ = w.Write([]byte("hi"))
	}))
	t.Cleanup(hostile.Close)

	seedRun(t, srv, "preview-hdr-run", "wf", store.RunStatusFinished)

	r := httptest.NewRequest(http.MethodGet, "/api/runs/preview-hdr-run/preview?target="+url.QueryEscape(hostile.URL+"/"), nil)
	w := httptest.NewRecorder()
	srv.handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("preview did not reach its success path: status %d, body %s", w.Code, w.Body.String())
	}

	// Exactly one value each, and it must be ours.
	for header, want := range map[string]string{
		"Permissions-Policy":     "camera=(), microphone=(), geolocation=(), payment=(), usb=()",
		"Referrer-Policy":        "strict-origin-when-cross-origin",
		"X-Content-Type-Options": "nosniff",
	} {
		got := w.Header().Values(header)
		if len(got) != 1 || got[0] != want {
			t.Errorf("%s = %q; want exactly [%q] — the upstream's value survived", header, got, want)
		}
	}
}

// TestArtifactFilesRefuseToBecomeDocuments pins the rule that a run artifact —
// bytes an agent wrote — cannot execute on the studio's origin.
//
// Found by adversarial review, which navigated a real browser to a seeded
// `report.html` artifact, ran its inline script on the studio origin and read
// the operator's local secrets index from it. The review-scope endpoint next
// door had refused exactly this since it was written; the artifact endpoint,
// sharing its content-type helper but not its discipline, had not.
func TestArtifactFilesRefuseToBecomeDocuments(t *testing.T) {
	scriptBearing := []string{
		"text/html; charset=utf-8",
		"image/svg+xml",
		"application/xhtml+xml",
		"text/xml",
	}
	for _, ct := range scriptBearing {
		if inlineSafeArtifactType(ct) {
			t.Errorf("%q would be served inline — it can execute script on the studio origin", ct)
		}
	}
	// …while the types the endpoint promises to preview still are.
	for _, ct := range []string{
		"text/markdown; charset=utf-8",
		"application/json",
		"text/plain; charset=utf-8",
		"image/png",
		"video/mp4",
		"application/pdf",
	} {
		if !inlineSafeArtifactType(ct) {
			t.Errorf("%q is no longer previewable inline; the studio's artifact preview regressed", ct)
		}
	}
}

// TestEveryResponseCarriesACSP is the other half of the artifact finding: the
// policy used to be skipped for /api/, which is precisely where the untrusted
// markup lives. A CSP-less response there is the hole, whatever the
// disposition says.
func TestEveryResponseCarriesACSP(t *testing.T) {
	srv := newSweepServer(t)
	for _, path := range []string{
		"/",
		"/api/runs",
		"/api/runs/x/artifact-files/report.html",
		"/healthz",
	} {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		srv.handler.ServeHTTP(w, r)
		if got := w.Header().Get("Content-Security-Policy"); got == "" {
			t.Errorf("%s: no Content-Security-Policy (status %d)", path, w.Code)
		}
	}
}

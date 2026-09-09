package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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

// TestRunPreviewKeepsItsOwnCSP guards the override contract: the preview
// endpoint serves attacker-influenced run output and replaces the studio policy
// with a stricter sandbox one. The middleware must not win over it.
func TestRunPreviewKeepsItsOwnCSP(t *testing.T) {
	srv := newSweepServer(t)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	srv.handler.ServeHTTP(w, r)
	studioCSP := w.Header().Get("Content-Security-Policy")

	// The preview handler sets its policy with Set, which replaces whatever the
	// middleware wrote. Assert the two policies are actually different, so a
	// future edit that makes the preview inherit the studio policy is visible.
	const previewCSP = "sandbox allow-scripts allow-forms allow-same-origin; frame-ancestors 'self'"
	if studioCSP == previewCSP {
		t.Fatal("the studio policy equals the preview sandbox policy; the preview override is no longer distinguishable")
	}
	if !strings.Contains(previewCSP, "sandbox") {
		t.Fatal("preview policy no longer sandboxes")
	}
}

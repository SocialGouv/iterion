package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestIsAllowedOriginReq locks the request-aware HTTP origin check that fixes
// the cloud/deployed studio: a same-origin POST (the SPA dialing the host that
// served it) must pass even though that host is not loopback. This mirrors the
// WebSocket upgrader's sameOrigin policy (TestSameOrigin) — the HTTP path had
// drifted to a loopback-only allowlist, which 403'd every state-changing POST
// from a non-loopback studio (e.g. "Dispatch existing board items").
func TestIsAllowedOriginReq(t *testing.T) {
	const port = 4891
	cases := []struct {
		name      string
		publicURL string
		origin    string
		host      string
		want      bool
	}{
		{"empty origin (curl / server-to-server)", "", "", "iterion.example.com", true},
		{"same-origin cloud", "", "https://iterion.example.com", "iterion.example.com", true},
		{"same-origin cloud, host case-insensitive", "", "https://Iterion.Example.com", "iterion.example.com", true},
		{"loopback localhost", "", "http://localhost:4891", "localhost:4891", true},
		{"loopback 127.0.0.1", "", "http://127.0.0.1:4891", "127.0.0.1:4891", true},
		{"cross-site drive-by", "", "https://evil.example", "iterion.example.com", false},
		{"wrong loopback port", "", "http://localhost:5173", "localhost:4891", false},
		// PublicURL covers proxies that rewrite Host so the same-origin
		// check can't fire (Origin host != request Host).
		{"public URL when host rewritten by proxy", "https://iterion.example.com", "https://iterion.example.com", "iterion-internal.svc", true},
		{"public URL with trailing path normalised", "https://iterion.example.com/", "https://iterion.example.com", "iterion-internal.svc", true},
		{"non-public cross origin still rejected", "https://iterion.example.com", "https://evil.example", "iterion-internal.svc", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &Server{cfg: Config{Port: port, PublicURL: c.publicURL}}
			r := httptest.NewRequest(http.MethodPost, "/api/runs", nil)
			r.Host = c.host
			if c.origin != "" {
				r.Header.Set("Origin", c.origin)
			}
			if got := s.isAllowedOriginReq(r); got != c.want {
				t.Errorf("isAllowedOriginReq(origin=%q, host=%q, public=%q) = %v; want %v",
					c.origin, c.host, c.publicURL, got, c.want)
			}
		})
	}
}

// TestRequireSafeOrigin verifies the 403 gate end-to-end: a same-origin POST
// passes through, a cross-site one is rejected with 403 + JSON body.
func TestRequireSafeOrigin(t *testing.T) {
	s := &Server{cfg: Config{Port: 4891}}

	t.Run("same-origin passes", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/api/runs", nil)
		r.Host = "iterion.example.com"
		r.Header.Set("Origin", "https://iterion.example.com")
		w := httptest.NewRecorder()
		if !s.requireSafeOrigin(w, r) {
			t.Fatalf("same-origin POST rejected; want pass (status=%d)", w.Code)
		}
	})

	t.Run("cross-site rejected 403", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/api/runs", nil)
		r.Host = "iterion.example.com"
		r.Header.Set("Origin", "https://evil.example")
		w := httptest.NewRecorder()
		if s.requireSafeOrigin(w, r) {
			t.Fatal("cross-site POST passed; want 403")
		}
		if w.Code != http.StatusForbidden {
			t.Errorf("status = %d; want %d", w.Code, http.StatusForbidden)
		}
	})
}

// TestProjectMutationsRejectCrossOrigin verifies the project-mutating handlers
// (switch / add / remove) are gated by requireSafeOrigin: a cross-site browser
// POST (Content-Type text/plain is a CORS "simple request", so no preflight)
// is rejected with 403 BEFORE any registry/filesystem mutation. Surfaced by a
// Seki self-audit: these endpoints previously ran without the guard, so a page
// the operator visited could re-point the studio workspace.
func TestProjectMutationsRejectCrossOrigin(t *testing.T) {
	s := &Server{cfg: Config{Port: 4891}}
	handlers := []struct {
		name   string
		method string
		path   string
		fn     func(http.ResponseWriter, *http.Request)
	}{
		{"switch", http.MethodPost, "/api/projects/switch", s.handleSwitchProject},
		{"add", http.MethodPost, "/api/projects", s.handleAddProject},
		{"remove", http.MethodDelete, "/api/projects/x", s.handleRemoveProject},
	}
	for _, h := range handlers {
		t.Run(h.name, func(t *testing.T) {
			r := httptest.NewRequest(h.method, h.path, nil)
			r.Host = "iterion.example.com"
			r.Header.Set("Origin", "https://evil.example")
			r.Header.Set("Content-Type", "text/plain")
			w := httptest.NewRecorder()
			h.fn(w, r)
			if w.Code != http.StatusForbidden {
				t.Errorf("%s: cross-origin status = %d; want %d (requireSafeOrigin guard missing?)",
					h.name, w.Code, http.StatusForbidden)
			}
		})
	}
}

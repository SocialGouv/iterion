package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/httpx"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// The CORS branches own one Vary token, "Origin". Writing it with Set discarded
// whatever a middleware upstream had recorded, and the loss is invisible: the
// response still looks right, it is merely cacheable across a dimension it
// genuinely varies on, so a shared cache can hand one client's representation
// to another.
//
// Exercised through the real server rather than the helper, and from the only
// position that can catch it — a middleware WRAPPING the handler, which is
// where every such token comes from.
func TestCorsPreflightKeepsAnUpstreamVary(t *testing.T) {
	srv := New(Config{Port: 4891, DisableAuth: true, SkipProjectRegistration: true}, iterlog.Nop())

	const upstream = "X-Upstream-Token"
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpx.AddVary(w, upstream)
		srv.Handler().ServeHTTP(w, r)
	})

	req := httptest.NewRequest(http.MethodOptions, "http://127.0.0.1/api/files", nil)
	req.Header.Set("Origin", "http://localhost:4891")
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)

	vary := strings.Join(rec.Header().Values("Vary"), ", ")
	if rec.Header().Get("Access-Control-Allow-Origin") == "" {
		t.Fatalf("preflight did not reflect the origin (status %d, Vary %q)", rec.Code, vary)
	}
	for _, want := range []string{"Origin", upstream} {
		if !strings.Contains(vary, want) {
			t.Fatalf("Vary = %q, lost %q — a downstream Set discarded an upstream token", vary, want)
		}
	}
}

// The refusal is itself an Origin-dependent representation: ACAO is present for
// an allowed origin and absent otherwise. Declaring the dimension only on the
// allowed path lets a cache store the ACAO-less variant under the bare URL and
// hand it to an allowlisted origin, whose browser then blocks a request that
// should have succeeded.
func TestCorsDeclaresVaryEvenWhenTheOriginIsRefused(t *testing.T) {
	srv := New(Config{Port: 4891, DisableAuth: true, SkipProjectRegistration: true}, iterlog.Nop())

	t.Run("preflight", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodOptions, "http://127.0.0.1/api/files", nil)
		req.Header.Set("Origin", "https://evil.example")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)

		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Fatalf("reflected a refused origin: %q", got)
		}
		if vary := strings.Join(rec.Header().Values("Vary"), ", "); !strings.Contains(vary, "Origin") {
			t.Fatalf("refusal did not declare Vary: Origin (got %q)", vary)
		}
	})

	t.Run("reflectAllowedOrigin", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/runs", nil)
		req.Header.Set("Origin", "https://evil.example")
		rec := httptest.NewRecorder()
		srv.reflectAllowedOrigin(rec, req)

		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Fatalf("reflected a refused origin: %q", got)
		}
		if vary := strings.Join(rec.Header().Values("Vary"), ", "); !strings.Contains(vary, "Origin") {
			t.Fatalf("refused response did not declare Vary: Origin (got %q)", vary)
		}
	})
}

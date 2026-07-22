package server

import (
	"net/http/httptest"
	"testing"
)

// TestExtractBearerAcceptsTokenOnWSPaths verifies that the ?t=<jwt>
// query-param fallback for WebSocket clients works on both the
// file-event hub (/api/ws, no trailing slash) and the per-run streams
// (/api/ws/runs/<id>). A regression on this lets browser-driven WS
// authenticate while rejecting CLI/SDK clients on /api/ws, which
// cannot attach an Authorization header to the Upgrade request.
func TestExtractBearerAcceptsTokenOnWSPaths(t *testing.T) {
	t.Parallel()
	cases := []struct {
		path string
		want string
	}{
		{"/api/ws", "abc"},
		{"/api/ws/", "abc"},
		{"/api/ws/runs/foo", "abc"},
		{"/api/files/save", ""}, // ?t= must NOT leak onto regular HTTP routes
		{"/api/parse", ""},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.path, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest("GET", tc.path+"?t=abc", nil)
			if got := extractBearer(req); got != tc.want {
				t.Fatalf("extractBearer(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

// TestIsPublicPathRunTokenSurfaces pins the JWT-gate bypass for the
// per-run X-Iterion-Run token surfaces: the handlers authenticate the
// run token themselves and 401 on a missing/unknown one, and a
// sandboxed or runner-launched run carries no operator JWT. Regressing
// this re-breaks board MCP writes and deterministic forge review
// publishing on cloud instances (observed live: Revi run 019f8ad0
// publish_review HTTP 401 "authentication required").
func TestIsPublicPathRunTokenSurfaces(t *testing.T) {
	t.Parallel()
	cases := []struct {
		path string
		want bool
	}{
		{"/api/v1/forge/publish-review", true},
		{"/api/v1/mcp/board", true},
		{"/api/v1/mcp/board/tools", true},
		{"/api/v1/forge/publish-reviewX", false}, // exact match only
		{"/api/v1/native", false},                // stays JWT-gated
		{"/api/runs", false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.path, func(t *testing.T) {
			t.Parallel()
			if got := isPublicPath(tc.path); got != tc.want {
				t.Fatalf("isPublicPath(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/deeplink"
)

// newLegacyRedirectMux wires ONLY what the redirect needs, so a failure names
// the redirect rather than some unrelated part of server construction.
func newLegacyRedirectMux(t *testing.T) *recordingMux {
	t.Helper()
	s := &Server{mux: newRecordingMux()}
	s.registerStudioLegacyRedirects()
	// Stand in for the SPA catch-all and the API surface, so a path the
	// redirect must NOT claim is observable as "reached the fallback" instead
	// of as a 404 that proves nothing about which handler answered.
	s.mux.HandleFunc("GET /", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	s.mux.HandleFunc("GET /api/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	return s.mux
}

func TestLegacyStudioURLRedirectsToTheStudio(t *testing.T) {
	mux := newLegacyRedirectMux(t)

	cases := []struct {
		name string
		path string
		want string
	}{
		{"a run deep link, the shape on forge statuses and in mail", "/runs/01a09ec6-b5b3-7660-8ef1-295031b7260d", "/studio/runs/01a09ec6-b5b3-7660-8ef1-295031b7260d"},
		{"the bare list page, not only what is beneath it", "/runs", "/studio/runs"},
		{"the query decides the destination and must survive", "/runs/new?bot=review-pr&file=a.bot", "/studio/runs/new?bot=review-pr&file=a.bot"},
		{"a nested admin page", "/admin/users", "/studio/admin/users"},
		{"a segment with a hyphen", "/whats-next", "/studio/whats-next"},
		{"config-editor is not the /config share link", "/config-editor", "/studio/config-editor"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if rec.Code != http.StatusFound {
				t.Fatalf("GET %s: status = %d, want %d (%s)", tc.path, rec.Code, http.StatusFound, rec.Body.String())
			}
			if got := rec.Header().Get("Location"); got != tc.want {
				t.Fatalf("GET %s: Location = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

// The redirect is a claim over a slice of the root URL space, so what it does
// NOT claim is as much of the contract as what it does. Each path below breaks
// something real if it starts 302-ing: a JSON client parses a redirect body, a
// share token in a fragment is dropped by an extra hop, a sign-in bounces.
func TestLegacyStudioRedirectLeavesTheRootSurfacesAlone(t *testing.T) {
	mux := newLegacyRedirectMux(t)

	for _, path := range []string{
		"/",
		"/login",
		"/login?next=%2Fstudio%2Fruns",
		"/auth/reset?token=abc",
		"/auth/forgot-password",
		"/invitations/accept",
		"/cli-auth?redirect_uri=http%3A%2F%2F127.0.0.1%2Fcb",
		"/config/01a09ec6",
		"/marketplace",
		"/api/runs/01a09ec6",
		"/api/auth/oidc/github/callback",
		// Already moved: redirecting again would loop.
		"/studio",
		"/studio/runs/01a09ec6",
	} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			if rec.Code == http.StatusFound {
				t.Fatalf("GET %s was redirected to %q — it must stay where it is", path, rec.Header().Get("Location"))
			}
		})
	}
}

// A segment that also names a root surface would silently steal it. The list
// is frozen and hand-written, so the overlap is checked rather than trusted.
func TestFrozenLegacySegmentsDoNotOverlapTheRootSurfaces(t *testing.T) {
	rootSurfaces := []string{"api", "assets", "auth", "brand", "cli-auth", "config", "healthz", "invitations", "login", "marketplace", "readyz", "x"}
	for _, seg := range legacyStudioSegments {
		for _, root := range rootSurfaces {
			if seg == root {
				t.Errorf("legacy segment %q also names a root surface — redirecting it would take that surface away", seg)
			}
		}
		if strings.TrimPrefix(deeplink.StudioBase, "/") == seg {
			t.Errorf("legacy segment %q is the studio base itself — the redirect would loop", seg)
		}
	}
}

func TestIsLegacyStudioPath(t *testing.T) {
	for _, p := range []string{"/runs", "/runs/abc", "/admin/users", "/whats-next"} {
		if !isLegacyStudioPath(p) {
			t.Errorf("isLegacyStudioPath(%q) = false, want true", p)
		}
	}
	// "/runsomething" shares a prefix with "runs" but is a different segment.
	for _, p := range []string{"/", "/login", "/config/x", "/marketplace", "/runsomething", "/studio/runs"} {
		if isLegacyStudioPath(p) {
			t.Errorf("isLegacyStudioPath(%q) = true, want false", p)
		}
	}
}

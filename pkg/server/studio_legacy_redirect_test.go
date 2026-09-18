package server

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
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
		// Clean() removes a trailing slash, and a trailing slash is a real
		// published address. A guard that rejected everything Clean changes
		// would 404 these.
		{"a trailing slash is not a traversal", "/account/", "/studio/account/"},
		{"a nested trailing slash either", "/admin/users/", "/studio/admin/users/"},
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

// A redirect that signs a Location leaving the base it just prepended is the
// defect here. ServeMux cleans a literal "..", so it never reaches the
// handler; "%2e%2e" does reach it, and the URL parser every browser uses
// resolves it as a dot-dot segment — so the product would 302 a reader out of
// /studio and onto any path on the origin, vouching for it.
//
// The assertion is on the RESOLVED target, not on the spelling that produced
// it: that is what a browser does with the Location, and it cannot be defeated
// by a future encoding the guard was never taught.
//
// Over a REAL socket, not httptest.NewRequest. The two disagree exactly where
// this defect lives: a request built in-process is parsed with url.Parse and
// its dirty path is cleaned away before any handler sees it, so the same case
// that 302s on the wire is silently skipped in memory. Measured — the first
// version of this test passed with the guard deleted.
func TestARedirectNeverSignsATargetThatLeavesTheStudio(t *testing.T) {
	s := &Server{mux: newRecordingMux()}
	s.registerStudioLegacyRedirects()
	s.mux.HandleFunc("GET /", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	srv := httptest.NewServer(s.mux)
	defer srv.Close()

	for _, target := range []string{
		"/runs/%2e%2e/%2e%2e/api/server/info",
		"/runs/%2e%2e/evil",
		"/board/%2e%2e/%2e%2e/%2e%2e/login",
		"/runs/%2E%2E/%2E%2E/evil",
	} {
		t.Run(target, func(t *testing.T) {
			status, loc := rawGET(t, srv.URL, target)
			if status != http.StatusFound {
				return // refused outright, or normalised by the mux: nothing signed.
			}
			// The oracle has to be the BROWSER's rule, not net/url's.
			// url.ResolveReference collapses only literal dot segments and
			// leaves "%2e%2e" encoded, so using it as the oracle makes this
			// test green against the very defect it exists for — measured: the
			// first version passed with the guard deleted. The WHATWG parser
			// every browser ships decodes first and THEN removes dot segments,
			// which is what path.Clean over url.Parse's decoded Path does.
			ref, err := url.Parse(loc)
			if err != nil {
				t.Fatalf("Location %q does not parse: %v", loc, err)
			}
			got := path.Clean(ref.Path) // ref.Path is decoded: "%2e%2e" is ".." here
			if got != deeplink.StudioBase && !strings.HasPrefix(got, deeplink.StudioBase+"/") {
				t.Fatalf("GET %s → 302 Location %q, which a browser resolves to %q — outside %q, with the product vouching for it",
					target, loc, got, deeplink.StudioBase)
			}
		})
	}
}

// rawGET writes the request line verbatim, so the server parses the target the
// way it parses one off the network.
func rawGET(t *testing.T, serverURL, target string) (int, string) {
	t.Helper()
	conn, err := net.Dial("tcp", strings.TrimPrefix(serverURL, "http://"))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: iterion.test\r\nConnection: close\r\n\r\n", target); err != nil {
		t.Fatalf("write: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode, resp.Header.Get("Location")
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

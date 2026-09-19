package server

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestSPAHandler(t *testing.T) {
	fsys := fstest.MapFS{
		"index.html":            &fstest.MapFile{Data: []byte("<html>SHELL</html>")},
		"assets/app.js":         &fstest.MapFile{Data: []byte("console.log('hi')")},
		"favicon.ico":           &fstest.MapFile{Data: []byte("ICO")},
		"runs/static-file.json": &fstest.MapFile{Data: []byte(`{"ok":true}`)},
	}
	sub, err := fs.Sub(fsys, ".")
	if err != nil {
		t.Fatalf("sub: %v", err)
	}
	h := SPAHandler(sub, "")

	cases := []struct {
		name        string
		method      string
		path        string
		wantStatus  int
		wantBodySub string
	}{
		{"root serves index", "GET", "/", http.StatusOK, "SHELL"},
		{"existing asset served verbatim", "GET", "/assets/app.js", http.StatusOK, "console.log"},
		{"existing favicon served", "GET", "/favicon.ico", http.StatusOK, "ICO"},
		{"existing file under spa-like path served", "GET", "/runs/static-file.json", http.StatusOK, `"ok":true`},
		{"unknown spa route falls back to index", "GET", "/runs/abc-xyz", http.StatusOK, "SHELL"},
		{"deep unknown spa route falls back to index", "GET", "/projects/p1/runs/abc", http.StatusOK, "SHELL"},
		{"HEAD on unknown route returns 200 no body", "HEAD", "/runs/abc-xyz", http.StatusOK, ""},
		// Unrouted /api/* must be an authoritative 404, never the SPA shell —
		// else a JSON client JSON.parses index.html and crashes (the
		// /admin/orgs-in-local-mode bug). Covers GET and non-GET.
		{"unrouted GET /api is 404 not shell", "GET", "/api/admin/orgs", http.StatusNotFound, "no such API endpoint"},
		{"unrouted POST /api is 404 not shell", "POST", "/api/foo", http.StatusNotFound, "no such API endpoint"},
		// A hashed asset this build does not carry must 404, never the shell.
		// Answering 200 text/html for a .js turns "you reached a replica on
		// another build" into "disallowed MIME type", which no client can act
		// on — the prod symptom of 2026-09-15.
		{"missing hashed chunk is 404 not shell", "GET", "/assets/theme-D9hz6J7I.js", http.StatusNotFound, "404 page not found"},
		{"missing hashed stylesheet is 404 not shell", "GET", "/assets/index-CkfKu4qQ.css", http.StatusNotFound, "404 page not found"},
		// http.FileServer would render the directory index here: an HTML
		// listing of every chunk and sourcemap the build emitted.
		{"asset directory is 404 not a listing", "GET", "/assets/", http.StatusNotFound, "404 page not found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status: want %d got %d body=%q", tc.wantStatus, rec.Code, rec.Body.String())
			}
			if tc.wantBodySub != "" && !strings.Contains(rec.Body.String(), tc.wantBodySub) {
				t.Fatalf("body missing %q: got %q", tc.wantBodySub, rec.Body.String())
			}
			if tc.method == "HEAD" && rec.Body.Len() != 0 {
				t.Fatalf("HEAD body should be empty, got %q", rec.Body.String())
			}
		})
	}
}

// TestSPAHandlerAssetMissDoesNotServeShell pins the distinction both ways: a
// path under the build-asset prefix must 404 with no HTML body, while a
// client-side route sharing its shape must still get the shell. A status-only
// assertion would stay green if the handler ever answered 404 *with* the shell
// body, so the body and the content type are asserted too.
func TestSPAHandlerAssetMissDoesNotServeShell(t *testing.T) {
	fsys := fstest.MapFS{
		"index.html":    &fstest.MapFile{Data: []byte("<html>SHELL</html>")},
		"assets/app.js": &fstest.MapFile{Data: []byte("console.log('hi')")},
	}
	sub, _ := fs.Sub(fsys, ".")
	h := SPAHandler(sub, "")

	req := httptest.NewRequest("GET", "/assets/app-OTHERBUILD.js", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing asset: want 404 got %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "SHELL") {
		t.Fatalf("missing asset served the SPA shell: %q", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); strings.Contains(ct, "text/html") {
		t.Fatalf("missing asset answered as HTML (content-type %q) — the MIME-block bug", ct)
	}

	// The witness for the other direction: without it, a handler that 404s
	// every unknown path would pass the assertions above while breaking every
	// client-side route.
	req = httptest.NewRequest("GET", "/orgs/5f916212-a1bd-453a-b8cc-ebbe24abd8cc", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "SHELL") {
		t.Fatalf("client-side route lost the shell: status %d body %q", rec.Code, rec.Body.String())
	}

	// And the asset that DOES exist still resolves to its real bytes.
	req = httptest.NewRequest("GET", "/assets/app.js", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "console.log") {
		t.Fatalf("present asset broke: status %d body %q", rec.Code, rec.Body.String())
	}
}

// A content-addressed URL that 404s mid-rollout becomes VALID once the fleet
// converges. Without no-store an intermediary may keep the negative, and the
// client-side reload cannot repair it: index.html yields the same hash again.
func TestSPAHandlerAssetMissIsNotCacheable(t *testing.T) {
	fsys := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<html>SHELL</html>")}}
	sub, _ := fs.Sub(fsys, ".")
	rec := httptest.NewRecorder()
	SPAHandler(sub, "").ServeHTTP(rec, httptest.NewRequest("GET", "/assets/gone-XXXX.js", nil))

	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control: want no-store, got %q", got)
	}
}

// The workspace host and the desktop pane proxy are bare handlers with no
// ServeMux in front of them, so a path arrives exactly as the client wrote it.
// A browser does not collapse a doubled slash, and an uncleaned prefix test
// walks straight past the guard into the SPA shell.
func TestIsBuildAssetPathCleansItsInput(t *testing.T) {
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"/assets/app.js", true},
		{"//assets/app.js", true},
		{"/x/conn//assets/app.js", false}, // scoped form is stripped by the caller
		{"/assets/", true},
		{"/assets", true},
		{"/./assets/app.js", true},
		{"/runs/../assets/app.js", true},
		{"/orgs/5f916212", false},
		{"/assetsmith/app.js", false},
		{"/", false},
	} {
		if got := IsBuildAssetPath(tc.path); got != tc.want {
			t.Errorf("IsBuildAssetPath(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestSPAHandlerNoIndex(t *testing.T) {
	fsys := fstest.MapFS{
		"assets/app.js": &fstest.MapFile{Data: []byte("ok")},
	}
	sub, _ := fs.Sub(fsys, ".")
	h := SPAHandler(sub, "")
	req := httptest.NewRequest("GET", "/runs/abc", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 when index missing, got %d", rec.Code)
	}
}

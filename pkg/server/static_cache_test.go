package server

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
)

// staticCacheTree is the shape of the embedded studio tree: the shell, a
// content-hashed bundle under the asset prefix and unhashed statics at the
// root whose names survive a build.
func staticCacheTree() fstest.MapFS {
	return fstest.MapFS{
		"index.html":               &fstest.MapFile{Data: []byte("<html><head></head>SHELL</html>")},
		"assets/index-CkfKu4qQ.js": &fstest.MapFile{Data: []byte("console.log('hi')")},
		"assets/index-CkfKu4qQ.js.map": &fstest.MapFile{
			Data: []byte(`{"sources":["../../src/secret.tsx"]}`)},
		"manifest.json": &fstest.MapFile{Data: []byte(`{"name":"Iterion"}`)},
		"favicon.ico":   &fstest.MapFile{Data: []byte("ICO")},
	}
}

func fetchStatic(t *testing.T, h http.Handler, method, target string, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// A content-hashed artifact never changes behind its URL: the browser may
// keep it for a year without asking. The exact value is asserted — a
// max-age without `immutable` still sends a revalidation per navigation
// on some browsers, and `public` is what lets the ingress and a CDN store it.
func TestSPAHandlerHashedAssetIsImmutable(t *testing.T) {
	h := SPAHandler(staticCacheTree(), "")
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		rec := fetchStatic(t, h, method, "/assets/index-CkfKu4qQ.js")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d, want 200", method, rec.Code)
		}
		if got := rec.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
			t.Fatalf("%s: Cache-Control %q, want the immutable policy", method, got)
		}
	}
	if rec := fetchStatic(t, h, http.MethodGet, "/assets/index-CkfKu4qQ.js"); !strings.Contains(rec.Body.String(), "console.log") {
		t.Fatalf("asset bytes lost: %q", rec.Body.String())
	}
}

// Nothing but the bytes may carry the immutable policy: a 404 mid-rollout
// becomes valid once the fleet converges, and a directory under the prefix
// is a 404 with no listing. Both keep the negative out of every cache.
func TestSPAHandlerAssetMissIsNeverImmutable(t *testing.T) {
	h := SPAHandler(staticCacheTree(), "")
	for _, target := range []string{"/assets/missing.js", "/assets/", "/assets"} {
		rec := fetchStatic(t, h, http.MethodGet, target)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s: status %d, want 404", target, rec.Code)
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("%s: Cache-Control %q, want no-store", target, got)
		}
		if strings.Contains(rec.Body.String(), "index-CkfKu4qQ") {
			t.Fatalf("%s: enumerated the build: %q", target, rec.Body.String())
		}
	}
}

// http.FileServer can refuse a PRESENT artifact — If-Match that fails is a
// 412 — and the policy is decided by the status the file server commits,
// not by the path alone.
func TestSPAHandlerAssetRefusalIsNotImmutable(t *testing.T) {
	rec := fetchStatic(t, SPAHandler(staticCacheTree(), ""), http.MethodGet,
		"/assets/index-CkfKu4qQ.js", "If-Match", `"not-this-build"`)
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("status %d, want 412", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); strings.Contains(got, "immutable") {
		t.Fatalf("a 412 went out immutable: Cache-Control %q", got)
	}
}

// The shell is what names the current build: it is re-fetched on every
// navigation (no-cache) whether it answers "/" or a client-side route, and a
// path that cleans into it must not be mistaken for an asset.
func TestSPAHandlerShellStaysNoCache(t *testing.T) {
	h := SPAHandler(staticCacheTree(), "")
	for _, target := range []string{"/", "/runs/abc-xyz", "/assets/.."} {
		rec := fetchStatic(t, h, http.MethodGet, target)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "SHELL") {
			t.Fatalf("%s: %d %q, want the shell", target, rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
			t.Fatalf("%s: Cache-Control %q, want no-cache", target, got)
		}
		if got := rec.Header().Get("ETag"); got != "" {
			t.Fatalf("%s: the shell carries an ETag %q", target, got)
		}
	}
	// http.FileServer sends a direct /index.html to "/"; the redirect carries
	// neither policy, so the cached answer is always the one serveIndex gave.
	rec := fetchStatic(t, h, http.MethodGet, "/index.html")
	if rec.Code != http.StatusMovedPermanently || rec.Header().Get("Cache-Control") != "" || rec.Header().Get("ETag") != "" {
		t.Fatalf("/index.html: %d Cache-Control=%q ETag=%q", rec.Code, rec.Header().Get("Cache-Control"), rec.Header().Get("ETag"))
	}
}

// An unhashed static keeps its name across builds, so it is served with a
// strong ETag derived from its bytes and a short max-age: the client holds
// it for an hour, then revalidates and gets a 304 while the bytes stand.
func TestSPAHandlerUnhashedStaticRevalidates(t *testing.T) {
	h := SPAHandler(staticCacheTree(), "")
	rec := fetchStatic(t, h, http.MethodGet, "/manifest.json")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Iterion") {
		t.Fatalf("first fetch: %d %q", rec.Code, rec.Body.String())
	}
	etag := rec.Header().Get("ETag")
	if !strings.HasPrefix(etag, `"`) || !strings.HasSuffix(etag, `"`) || len(etag) < 4 {
		t.Fatalf("ETag %q, want a strong quoted tag", etag)
	}
	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=3600" {
		t.Fatalf("Cache-Control %q, want the revalidated policy", got)
	}

	for _, inm := range []string{etag, "W/" + etag, `"other", ` + etag} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			rec := fetchStatic(t, h, method, "/manifest.json", "If-None-Match", inm)
			if rec.Code != http.StatusNotModified {
				t.Fatalf("%s If-None-Match %s: status %d, want 304", method, inm, rec.Code)
			}
			if rec.Body.Len() != 0 {
				t.Fatalf("%s If-None-Match %s: a 304 carried a body %q", method, inm, rec.Body.String())
			}
			if rec.Header().Get("ETag") != etag || rec.Header().Get("Cache-Control") != "public, max-age=3600" {
				t.Fatalf("304 lost its validators: ETag=%q Cache-Control=%q", rec.Header().Get("ETag"), rec.Header().Get("Cache-Control"))
			}
		}
	}

	// A tag from another build is a miss: the bytes go out again.
	rec = fetchStatic(t, h, http.MethodGet, "/manifest.json", "If-None-Match", `"0000000000000000"`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Iterion") {
		t.Fatalf("stale tag: %d %q, want the bytes", rec.Code, rec.Body.String())
	}

	// Each static has its own tag, and a HEAD reports the same one as a GET.
	if ico := fetchStatic(t, h, http.MethodHead, "/favicon.ico"); ico.Header().Get("ETag") == etag || ico.Header().Get("ETag") == "" {
		t.Fatalf("favicon ETag %q, manifest ETag %q", ico.Header().Get("ETag"), etag)
	}
}

// The tag is a function of the bytes — the same name with other content is
// another tag, which is what makes a new build's favicon reach a client that
// cached the old one within the hour.
func TestStaticETagFollowsTheBytes(t *testing.T) {
	a := fetchStatic(t, SPAHandler(staticCacheTree(), ""), http.MethodGet, "/manifest.json").Header().Get("ETag")
	other := staticCacheTree()
	other["manifest.json"] = &fstest.MapFile{Data: []byte(`{"name":"Iterion","v":2}`)}
	b := fetchStatic(t, SPAHandler(other, ""), http.MethodGet, "/manifest.json").Header().Get("ETag")
	if a == "" || b == "" || a == b {
		t.Fatalf("ETag does not follow the bytes: %q vs %q", a, b)
	}
}

// A trailing slash on a file is a redirect from http.FileServer: nothing to
// tag, and no policy on a 301.
func TestStaticRedirectCarriesNoPolicy(t *testing.T) {
	rec := fetchStatic(t, newStaticFileServer(staticCacheTree()), http.MethodGet, "/manifest.json/")
	if rec.Code != http.StatusMovedPermanently {
		t.Fatalf("status %d, want 301", rec.Code)
	}
	if rec.Header().Get("ETag") != "" || rec.Header().Get("Cache-Control") != "" {
		t.Fatalf("redirect carries ETag=%q Cache-Control=%q", rec.Header().Get("ETag"), rec.Header().Get("Cache-Control"))
	}
}

// countingFS counts the opens of one file: the hash is computed on the first
// request and never again, so N requests open the file N times for the file
// server plus once for the tag.
type countingFS struct {
	fs.FS
	name  string
	opens atomic.Int32
}

func (c *countingFS) Open(name string) (fs.File, error) {
	if name == c.name {
		c.opens.Add(1)
	}
	return c.FS.Open(name)
}

func TestStaticETagIsHashedOnce(t *testing.T) {
	tree := &countingFS{FS: staticCacheTree(), name: "manifest.json"}
	h := newStaticFileServer(tree)
	const n = 5
	var first string
	for i := 0; i < n; i++ {
		rec := fetchStatic(t, h, http.MethodGet, "/manifest.json")
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: status %d", i, rec.Code)
		}
		if tag := rec.Header().Get("ETag"); i == 0 {
			first = tag
		} else if tag != first {
			t.Fatalf("request %d: ETag %q, first was %q", i, tag, first)
		}
	}
	if got := tree.opens.Load(); got != n+1 {
		t.Fatalf("manifest.json opened %d times for %d requests, want %d (one hash, then one per serve)", got, n, n+1)
	}
}

// The policy is set when the status is committed, not when the path is
// read: a failure from the file behind the path goes out uncached.
func TestCacheControlOnSuccess(t *testing.T) {
	for _, tc := range []struct {
		name  string
		serve func(w http.ResponseWriter)
		want  string
	}{
		{"200 explicit", func(w http.ResponseWriter) { w.WriteHeader(200); _, _ = w.Write([]byte("x")) }, "v"},
		{"200 implicit", func(w http.ResponseWriter) { _, _ = w.Write([]byte("x")) }, "v"},
		{"206 partial", func(w http.ResponseWriter) { w.WriteHeader(206) }, "v"},
		{"304 not modified", func(w http.ResponseWriter) { w.WriteHeader(304) }, "v"},
		{"301 redirect", func(w http.ResponseWriter) { w.WriteHeader(301) }, ""},
		{"404", func(w http.ResponseWriter) { http.Error(w, "gone", 404) }, ""},
		{"416 range", func(w http.ResponseWriter) { w.WriteHeader(416) }, ""},
		{"500", func(w http.ResponseWriter) { http.Error(w, "boom", 500) }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tc.serve(&cacheControlOnSuccess{ResponseWriter: rec, value: "v"})
			if got := rec.Header().Get("Cache-Control"); got != tc.want {
				t.Fatalf("Cache-Control %q, want %q", got, tc.want)
			}
		})
	}
	// Unwrap exposes the writer underneath to http.ResponseController.
	rec := httptest.NewRecorder()
	if (&cacheControlOnSuccess{ResponseWriter: rec}).Unwrap() != rec {
		t.Fatal("Unwrap does not return the wrapped writer")
	}
}

// The workspace host is the OTHER surface that serves the tree: the same
// policies, decided by the same handler.
func TestWorkspaceAssetCachePolicy(t *testing.T) {
	static := staticCacheTree()
	host := &WorkspaceHost{static: static, files: newStaticFileServer(static)}
	h := http.HandlerFunc(host.serveWorkspaceAsset)

	if rec := fetchStatic(t, h, http.MethodGet, "/assets/index-CkfKu4qQ.js"); rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Fatalf("hashed asset: %d Cache-Control=%q", rec.Code, rec.Header().Get("Cache-Control"))
	}
	if rec := fetchStatic(t, h, http.MethodGet, "/assets/missing.js"); rec.Code != http.StatusNotFound || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("missing asset: %d Cache-Control=%q", rec.Code, rec.Header().Get("Cache-Control"))
	}
	rec := fetchStatic(t, h, http.MethodGet, "/manifest.json")
	etag := rec.Header().Get("ETag")
	if rec.Code != http.StatusOK || etag == "" || rec.Header().Get("Cache-Control") != "public, max-age=3600" {
		t.Fatalf("unhashed static: %d ETag=%q Cache-Control=%q", rec.Code, etag, rec.Header().Get("Cache-Control"))
	}
	if rec := fetchStatic(t, h, http.MethodGet, "/manifest.json", "If-None-Match", etag); rec.Code != http.StatusNotModified {
		t.Fatalf("revalidation: status %d, want 304", rec.Code)
	}
	// The injected shell keeps its own, stricter policy.
	if rec := fetchStatic(t, h, http.MethodGet, "/runs/abc"); rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("ETag") != "" {
		t.Fatalf("workspace shell: %d Cache-Control=%q ETag=%q", rec.Code, rec.Header().Get("Cache-Control"), rec.Header().Get("ETag"))
	}
}

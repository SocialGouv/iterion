package server

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"sync"
)

// The studio tree is embedded, and an embedded file carries a zero modtime:
// http.FileServer alone sends neither Last-Modified nor ETag, so a browser
// cannot revalidate and refetches every module of the build on every visit —
// 78 modules, 1.4 MB, measured on iterion.cloud on 2026-11-05. Two policies
// cover the tree. index.html is under neither: the shell goes through
// serveIndex and ServeInjectedIndex, which send no-cache and no-store.

// immutableCacheControl is the policy under BuildAssetPrefix: the bundler
// writes a content hash into every file name there, so the bytes behind a URL
// never change and a client may keep them for a year without asking
// (RFC 8246). A new build is a new URL.
const immutableCacheControl = "public, max-age=31536000, immutable"

// revalidatedCacheControl is the policy for an unhashed static at the root —
// favicon, manifest, service worker, icons: the name survives a build, so a
// client keeps its copy for an hour and then revalidates it against the ETag,
// and a 304 costs no body.
const revalidatedCacheControl = "public, max-age=3600"

// staticFileServer is the one handler that hands a client the bytes of the
// embedded studio tree: http.FileServer, with the cache policy of the path
// decided here, so the server catch-all, the workspace host, the dispatcher
// and the desktop proxy — every surface that serves the tree — answer the
// same. Compression is not its job: the ingress gzips, and the strong ETag it
// sends is compared weakly on the way back (etagSatisfied, brand_routes.go).
//
// Under BuildAssetPrefix it owns the invariant "a file or a 404, never the
// shell and never a listing". http.FileServer answers a directory with an
// HTML index of every chunk and sourcemap the build emitted, which is false
// exactly where it is easiest to reach; the guard lives here rather than in
// each caller because the first version of it was copied into one of the two
// surfaces and not the other.
type staticFileServer struct {
	tree  fs.FS
	files http.Handler
	etags sync.Map // rel path → strong ETag, hashed once per process
}

func newStaticFileServer(tree fs.FS) *staticFileServer {
	return &staticFileServer{tree: tree, files: http.FileServer(http.FS(tree))}
}

func (s *staticFileServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	clean := path.Clean(r.URL.Path)
	rel := strings.TrimPrefix(clean, "/")
	if IsBuildAssetPath(clean) {
		info, err := fs.Stat(s.tree, rel)
		if errors.Is(err, fs.ErrNotExist) || (err == nil && info.IsDir()) {
			NotFoundBuildAsset(w, r)
			return
		}
		// Any other failure is the file server's to report, and the writer
		// keeps the year-long policy off a response that is not the bytes.
		s.files.ServeHTTP(&cacheControlOnSuccess{ResponseWriter: w, value: immutableCacheControl}, r)
		return
	}
	// A path http.FileServer answers with a redirect — a trailing slash on a
	// file — carries no bytes to tag. The check reads the raw path because
	// that is what the file server reads to decide.
	if etag, ok := s.etag(rel); ok && !strings.HasSuffix(r.URL.Path, "/") {
		// Set before delegating: http.FileServer evaluates If-None-Match
		// against the response's own ETag and writes the 304 itself, with the
		// weak comparison an ingress that gzips requires.
		w.Header().Set("ETag", etag)
		w = &cacheControlOnSuccess{ResponseWriter: w, value: revalidatedCacheControl}
	}
	s.files.ServeHTTP(w, r)
}

// etag returns the strong ETag of the file at rel, hashing its bytes the
// first time the file is asked for and never again: the tree is embedded, so
// a tag cannot go stale within a process — a new build is a new binary.
// index.html gets none: it is the shell, served by serveIndex and
// ServeInjectedIndex, and http.FileServer redirects a direct request for it
// to its directory.
func (s *staticFileServer) etag(rel string) (string, bool) {
	if rel == "" || path.Base(rel) == "index.html" {
		return "", false
	}
	if tag, ok := s.etags.Load(rel); ok {
		return tag.(string), true
	}
	data, err := fs.ReadFile(s.tree, rel)
	if err != nil {
		// A missing, unreadable or directory path has no bytes to tag; the
		// file server answers it, with the 404, listing or error it deserves.
		return "", false
	}
	sum := sha256.Sum256(data)
	tag := `"` + hex.EncodeToString(sum[:8]) + `"`
	s.etags.Store(rel, tag)
	return tag, true
}

// cacheControlOnSuccess sets Cache-Control on the response if, and only if,
// the handler behind it commits a status that carries the resource (2xx) or
// confirms the client's copy of it (304). http.FileServer still answers a
// present file with a 412, a 416 or a 500 when the request calls for one, and
// a year-long immutable policy on anything but the bytes would pin that
// failure in every cache on the path.
type cacheControlOnSuccess struct {
	http.ResponseWriter
	value     string
	committed bool
}

func (w *cacheControlOnSuccess) WriteHeader(code int) {
	if !w.committed {
		w.committed = true
		if (code >= 200 && code < 300) || code == http.StatusNotModified {
			w.Header().Set("Cache-Control", w.value)
		}
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *cacheControlOnSuccess) Write(b []byte) (int, error) {
	if !w.committed {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach Flush and Hijack on the writer
// underneath, as every wrapper in this package must.
func (w *cacheControlOnSuccess) Unwrap() http.ResponseWriter { return w.ResponseWriter }

package server

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/SocialGouv/iterion/bots"
	"github.com/SocialGouv/iterion/internal/httpx"
	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unparse"
	"github.com/SocialGouv/iterion/pkg/dsl/workflowfile"
)

// --- File management types ---

type listFilesResponse struct {
	Files []fileEntry `json:"files"`
}

type fileEntry struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

type openFileRequest struct {
	Path string `json:"path"`
}

type saveFileRequest struct {
	Path     string          `json:"path"`
	Document json.RawMessage `json:"document"`
}

type saveFileResponse struct {
	Path   string `json:"path"`
	Source string `json:"source"`
}

// --- Helpers ---

func readJSON(r *http.Request, v any) error {
	return httpx.DecodeJSON(r, v)
}

// decodeJSON reads+unmarshals the request body into *dst, writing a 400
// "invalid request: %v" on failure. Returns true on success, false if it
// already wrote an error response. Intended for the dominant handler-boilerplate
// pattern; handlers that emit a different status/message, use httpErrorFor, or
// do extra validation between decode and error should keep the explicit form.
func decodeJSON[T any](w http.ResponseWriter, r *http.Request, dst *T) bool {
	if err := readJSON(r, dst); err != nil {
		httpError(w, http.StatusBadRequest, "invalid request: %v", err)
		return false
	}
	return true
}

// decodeJSONCapped reads+unmarshals the request body into *dst, bounding
// it to capBytes and writing a tenant-aware 400 "invalid body: %v" on
// failure. Returns true on success, false if it already wrote an error
// response.
func decodeJSONCapped[T any](s *Server, w http.ResponseWriter, r *http.Request, dst *T, capBytes int64) bool {
	r.Body = http.MaxBytesReader(w, r.Body, capBytes)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid body: %v", err)
		return false
	}
	return true
}

// IsAllowedOrigin reports whether the given Origin header value matches the
// loopback set the studio server accepts. It is exposed as a method so test
// code (and a future config flag) can extend the allowlist without rewriting
// every handler. Empty Origin (same-origin request, curl, etc.) is allowed
// because the browser CORS layer is not involved in that case.
func (s *Server) isAllowedOrigin(origin string) bool {
	if origin == "" {
		return true
	}
	for _, allowed := range s.allowedOrigins() {
		if origin == allowed {
			return true
		}
	}
	return false
}

// isAllowedOriginReq is the request-aware origin check used by the HTTP CORS
// path (requireSafeOrigin, reflectAllowedOrigin, the OPTIONS preflight). It
// accepts, in order:
//   - an empty Origin (non-browser caller: curl, server-to-server),
//   - a same-origin request — the SPA dialing the host that served it. This
//     is what makes the deployed/cloud studio work behind any proxy or on
//     any public host WITHOUT configuring its URL, and mirrors the WebSocket
//     upgrader's sameOrigin policy (see hub.go). Without it, every
//     state-changing POST from a non-loopback studio is rejected with 403,
//     while reads (no Origin) and the WS (already same-origin-aware) work —
//     the exact asymmetry that broke "Dispatch existing board items" in prod.
//   - an Origin in the static allowlist (loopback, wails, configured
//     PublicURL) — covers proxies that rewrite the Host header so the
//     same-origin check above can't fire.
func (s *Server) isAllowedOriginReq(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	if sameOrigin(origin, r) {
		return true
	}
	return s.isAllowedOrigin(origin)
}

func (s *Server) allowedOrigins() []string {
	origins := []string{
		fmt.Sprintf("http://localhost:%d", s.cfg.Port),
		fmt.Sprintf("http://127.0.0.1:%d", s.cfg.Port),
		fmt.Sprintf("http://[::1]:%d", s.cfg.Port),
	}
	// Desktop mode: the studio SPA is hosted on the Wails AssetServer
	// (wails:// on Mac/Linux, http://wails.localhost on Windows) so that
	// `window.go.main.App.*` bindings + `/wails/runtime.js` injection are
	// available. HTTP API calls reach the local server via Wails' reverse
	// proxy (which rewrites Origin to the loopback target), but the
	// studio's WebSocket clients dial the local server DIRECTLY (Wails'
	// AssetServer returns 501 on WS upgrade). The dialer therefore arrives
	// with the SPA's true origin in the upgrade handshake; without these
	// entries the upgrader's CheckOrigin would reject every cross-origin
	// WS handshake from the desktop window. Token-bearing requests are
	// already authenticated by the auth middleware; origin allow-listing
	// is defense-in-depth.
	origins = append(origins,
		"wails://wails",
		"http://wails.localhost",
	)
	// Cloud / proxied deployments: the studio SPA is served from (and dials)
	// the operator's public host, not loopback. The configured PublicURL is
	// that origin; including it lets requests survive even when a reverse
	// proxy rewrites the Host header (so the same-origin check in
	// isAllowedOriginReq can't match). Normalised to scheme://host — the
	// shape a browser Origin header carries (no path, no trailing slash).
	if s.cfg.PublicURL != "" {
		if u, err := url.Parse(s.cfg.PublicURL); err == nil && u.Scheme != "" && u.Host != "" {
			origins = append(origins, u.Scheme+"://"+u.Host)
		}
	}
	return origins
}

// reflectAllowedOrigin sets ACAO to the request's Origin if (and only if) it
// is in the allowlist. Callers should always set Vary: Origin so caches don't
// poison the response across origins.
func (s *Server) reflectAllowedOrigin(w http.ResponseWriter, r *http.Request) {
	// nil request = a server-internal replay (the webhook defer sweep);
	// there is no Origin to reflect.
	if r == nil {
		return
	}
	origin := r.Header.Get("Origin")
	if origin != "" && s.isAllowedOriginReq(r) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Vary", "Origin")
	}
}

// writeJSON encodes v as JSON without touching the status code: callers rely
// on the implicit 200 from the first body write, or have already called
// WriteHeader themselves.
func writeJSON(w http.ResponseWriter, v any) {
	httpx.EncodeJSON(w, v)
}

// writeJSONFor is the request-aware variant of writeJSON: it also reflects an
// allowlisted Origin header so legitimate browser callers receive ACAO.
func (s *Server) writeJSONFor(w http.ResponseWriter, r *http.Request, v any) {
	s.reflectAllowedOrigin(w, r)
	httpx.EncodeJSON(w, v)
}

func httpError(w http.ResponseWriter, code int, format string, args ...any) {
	httpx.WriteJSON(w, code, map[string]string{"error": fmt.Sprintf(format, args...)})
}

// httpErrorFor is the request-aware variant: reflects allowlisted Origin so
// browser code can read the error body when same-origin or loopback.
func (s *Server) httpErrorFor(w http.ResponseWriter, r *http.Request, code int, format string, args ...any) {
	s.reflectAllowedOrigin(w, r)
	httpx.WriteJSON(w, code, map[string]string{"error": fmt.Sprintf(format, args...)})
}

// requireSafeOrigin gates state-changing endpoints. Any request whose Origin
// header is set and not in the allowlist is rejected with 403 BEFORE the
// handler runs — preventing a malicious page in another tab from POSTing
// into the local studio's filesystem-write endpoints. Same-origin and
// non-browser callers (no Origin header) pass through.
func (s *Server) requireSafeOrigin(w http.ResponseWriter, r *http.Request) bool {
	if s.isAllowedOriginReq(r) {
		return true
	}
	httpx.WriteJSON(w, http.StatusForbidden, map[string]string{
		"error": "cross-origin request rejected: origin not allowed (must be same-origin, loopback, or the configured public URL)",
	})
	return false
}

// safePath resolves relPath against WorkDir and ensures the result stays within
// WorkDir AFTER symlink resolution. The previous implementation used only
// filepath.Abs + prefix check, which lets a symlink at any depth in the
// workdir point at /etc, /home/$USER/.ssh, etc. — combined with the
// unauthenticated /api/files/open and /api/files/save endpoints, that gave
// any caller on an allowlisted origin (or the same machine, before B5) a
// path-traversal primitive.
//
// Strategy:
//  1. Compute the workdir's canonical (symlink-resolved) absolute path once;
//     use it as the containment root.
//  2. Resolve the requested path's canonical form. If the file does not yet
//     exist (legitimate Save case for new files), resolve the longest
//     existing ancestor and append the remaining components. We refuse the
//     path if any existing ancestor is itself a symlink that escapes the
//     root, OR if the final composed path is not under the root.
//  3. As a defence-in-depth on Save, refuse if the immediate parent
//     directory or any intermediate path component is a symlink — a
//     pre-planted symlink at parent dir would otherwise let WriteFile
//     follow it through.
func (s *Server) safePath(relPath string) (string, error) {
	// Snapshot WorkDir under the read lock so a concurrent
	// /api/projects/switch can't intersect baseAbs and baseReal
	// derivations against two different roots. Without the snapshot,
	// the containment check below could be satisfied by an OLD workdir
	// while the actual write lands under the NEW one — or vice versa.
	s.stateMu.RLock()
	workDir := s.cfg.WorkDir
	s.stateMu.RUnlock()
	return safePathWithin(workDir, relPath)
}

// safePathWithin resolves relPath against base with the same strict,
// symlink-aware containment used by Save: the returned absolute path is
// guaranteed to live inside base after resolving symlinks on the longest
// existing prefix. It is the single audited path-traversal boundary shared
// by the studio-workdir Save (safePath) and the run-worktree file editor
// (handleGetRunFileContent / handleSaveRunFileContent), so a fix here covers
// both surfaces.
func safePathWithin(base, relPath string) (string, error) {
	if base == "" {
		return "", fmt.Errorf("no working directory configured")
	}
	baseAbs, err := filepath.Abs(base)
	if err != nil {
		return "", fmt.Errorf("workdir abs: %w", err)
	}
	baseReal, err := filepath.EvalSymlinks(baseAbs)
	if err != nil {
		return "", fmt.Errorf("workdir resolve: %w", err)
	}

	// Compute the requested absolute path (without symlink resolution yet).
	// Idempotent on absolute inputs: handleResumeRun passes the
	// runMeta.FilePath value, which was already canonicalised at launch
	// time. Naively re-joining baseAbs with an already-absolute path
	// duplicates the workdir prefix (e.g. "/foo/bar" joined with "/foo/bar/x"
	// yields "/foo/bar/foo/bar/x"). The containment check below remains
	// the security boundary, so taking absolute inputs as-is is safe —
	// any path that escapes baseReal will still be rejected.
	var abs string
	if filepath.IsAbs(relPath) {
		abs = filepath.Clean(relPath)
	} else {
		abs = filepath.Join(baseAbs, filepath.Clean("/"+relPath))
	}
	abs, err = filepath.Abs(abs)
	if err != nil {
		return "", err
	}

	// Resolve symlinks for the longest existing prefix; keep the trailing
	// not-yet-existing components verbatim. This supports legitimate Save of
	// a brand-new file inside an existing directory.
	resolved, err := evalSymlinksLongestPrefix(abs)
	if err != nil {
		return "", err
	}

	if !pathContains(baseReal, resolved) {
		return "", fmt.Errorf("path escapes working directory")
	}
	return resolved, nil
}

// pathContains reports whether target is base or a path under base, after
// canonicalisation. Both paths must be absolute.
func pathContains(base, target string) bool {
	if base == target {
		return true
	}
	sep := string(filepath.Separator)
	if !strings.HasSuffix(base, sep) {
		base += sep
	}
	return strings.HasPrefix(target, base)
}

// evalSymlinksLongestPrefix walks abs from the root, finding the longest
// existing prefix and resolving it via filepath.EvalSymlinks; it then
// re-attaches any remaining (not-yet-existing) trailing components. If any
// existing component on the path is a symlink, EvalSymlinks resolves it —
// callers that want to refuse all symlinks in the chain (e.g. Save) should
// gate via a separate check. Returns the canonicalised absolute path.
func evalSymlinksLongestPrefix(abs string) (string, error) {
	// If the full path exists, resolve it directly.
	if _, err := os.Lstat(abs); err == nil {
		return filepath.EvalSymlinks(abs)
	}
	// Walk up until we find an existing ancestor.
	dir, leaf := filepath.Split(abs)
	dir = strings.TrimSuffix(dir, string(filepath.Separator))
	if dir == "" || dir == abs {
		return abs, nil
	}
	parent, err := evalSymlinksLongestPrefix(dir)
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, leaf), nil
}

// --- File management handlers ---

func (s *Server) handleListFiles(w http.ResponseWriter, _ *http.Request) {
	s.stateMu.RLock()
	workDir := s.cfg.WorkDir
	s.stateMu.RUnlock()
	if workDir == "" {
		writeJSON(w, listFilesResponse{Files: []fileEntry{}})
		return
	}
	var files []fileEntry
	// Per-entry errors are handled in the callback; a root-level walk failure
	// yields the partial (possibly empty) list, which this read-only file
	// browser degrades to gracefully.
	_ = filepath.WalkDir(workDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if isSkippedDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !isWorkflowFile(d.Name()) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(workDir, path)
		files = append(files, fileEntry{Name: rel, Size: info.Size()})
		return nil
	})
	if files == nil {
		files = []fileEntry{}
	}
	writeJSON(w, listFilesResponse{Files: files})
}

func (s *Server) handleOpenFile(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	var req openFileRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	absPath, err := s.safePath(req.Path)
	if err != nil {
		httpError(w, http.StatusBadRequest, "invalid path: %v", err)
		return
	}
	data, err := os.ReadFile(absPath)
	if err != nil {
		// Embedded-recipe fallback: the bot picker sets currentFilePath
		// to "bots/<name>" (legacy "examples/<name>") after loading a
		// binary-embedded recipe (see studio Toolbar handlePickFile).
		// When the project has no matching on-disk file, every later
		// flow that re-reads via /files/open (LaunchView's pre-launch
		// document fetch, the file watcher, hot-reload) would 404
		// without this fallback. Strip the prefix and resolve the
		// remainder (e.g. "feature_dev/main.bot") against the embed.
		for _, prefix := range []string{"bots/", "examples/"} {
			if rest := strings.TrimPrefix(req.Path, prefix); rest != req.Path {
				if embedded, ok := bots.Get(rest); ok {
					data = embedded
					err = nil
				}
				break
			}
		}
	}
	if err != nil {
		httpError(w, http.StatusNotFound, "file not found: %s", req.Path)
		return
	}
	pr := parser.Parse(req.Path, string(data))
	var diags []string
	for _, d := range pr.Diagnostics {
		diags = append(diags, d.Error())
	}
	if pr.File == nil {
		writeJSON(w, parseResponse{Diagnostics: diags})
		return
	}
	docJSON, err := ast.MarshalFile(pr.File)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "marshal error: %v", err)
		return
	}
	writeJSON(w, struct {
		Source      string          `json:"source"`
		Document    json.RawMessage `json:"document"`
		Diagnostics []string        `json:"diagnostics,omitempty"`
		Path        string          `json:"path"`
	}{
		Source:      string(data),
		Document:    json.RawMessage(docJSON),
		Diagnostics: diags,
		Path:        req.Path,
	})
}

func (s *Server) handleSaveFile(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	var req saveFileRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !workflowfile.IsWorkflowFile(req.Path) {
		httpError(w, http.StatusBadRequest, "filename must end in .bot")
		return
	}
	absPath, err := s.safePath(req.Path)
	if err != nil {
		httpError(w, http.StatusBadRequest, "invalid path: %v", err)
		return
	}
	f, err := ast.UnmarshalFile(req.Document)
	if err != nil {
		httpError(w, http.StatusBadRequest, "invalid document: %v", err)
		return
	}
	source := unparse.Unparse(f)
	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		httpError(w, http.StatusInternalServerError, "cannot create directory: %v", err)
		return
	}
	if s.watcher != nil {
		s.watcher.IgnorePath(absPath)
	}
	if err := os.WriteFile(absPath, []byte(source), 0o644); err != nil {
		httpError(w, http.StatusInternalServerError, "write error: %v", err)
		return
	}
	writeJSON(w, saveFileResponse{Path: req.Path, Source: source})
}

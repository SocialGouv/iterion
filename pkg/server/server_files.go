package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/SocialGouv/iterion/internal/httpx"
	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/canon"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
	"github.com/SocialGouv/iterion/pkg/dsl/workflowfile"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
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
	Path       string          `json:"path"`
	Document   json.RawMessage `json:"document"`
	CreateOnly bool            `json:"create_only,omitempty"`
	// Revision is the unit revision the document was opened at (the open
	// response's unit.revision); required for a bot in several files.
	Revision string `json:"revision,omitempty"`
}

type saveFileResponse struct {
	Path              string `json:"path"`
	Source            string `json:"source"`
	ConfirmedDiskPath string `json:"confirmed_disk_path,omitempty"`
	// Revision is the unit's revision after the save, Files the files the
	// save rewrote (from the unit's root); both for a bot in several files.
	Revision string   `json:"revision,omitempty"`
	Files    []string `json:"files,omitempty"`
}

// --- Helpers ---

func readJSON(r *http.Request, v any) error {
	return httpx.DecodeJSON(r, v)
}

// readJSONStrict refuses a field the destination does not declare, and names
// the ones it does. For requests whose parameters are consumed long after the
// 200 — a launch above all — where a dropped field is read back as the
// workflow's own default and the payload gives no sign.
func readJSONStrict(r *http.Request, v any) error {
	return httpx.DecodeJSONStrict(r, v)
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
			origins = append(origins, normalizeOrigin(u))
		}
	}
	// A deployment can be reached on more than one public host, and PublicURL
	// names exactly one. iterion's own production serves both iterion.cloud
	// and iterion.fabrique.social.gouv.fr; the second passes only because the
	// ingress forwards Host unchanged, so the same-origin branch matches. That
	// is a working setup resting on an unstated assumption — add a rewrite, or
	// a proxy that normalises Host, and the second host starts failing every
	// state-changing request with a 403 that names no cause.
	//
	// ITERION_ALLOWED_ORIGINS (comma-separated) states the extra origins, so
	// the multi-host mount is declared rather than inferred.
	origins = append(origins, s.extraOrigins...)
	return origins
}

// loadExtraAllowedOrigins resolves ITERION_ALLOWED_ORIGINS and reports every
// entry it had to drop. It is called once per Server (New, BrowserGuard) so
// the value is settled at construction rather than re-read per request — and,
// unlike a process-wide memo, it leaves the env→gate chain exercisable by a
// test that builds a Server.
//
// A malformed entry is named rather than dropped in silence: functionally it
// is identical to an absent one (the origin gets refused either way), which is
// the shape of a guard that looks configured and matches nothing.
func loadExtraAllowedOrigins(logger *iterlog.Logger) []string {
	valid, malformed := splitAllowedOrigins(os.Getenv("ITERION_ALLOWED_ORIGINS"))
	for _, entry := range malformed {
		logger.Warn("ITERION_ALLOWED_ORIGINS: ignoring %q — expected an origin of the form scheme://host with no path; requests from it will be refused", logSafe(entry))
	}
	return valid
}

// normalizeOrigin renders u the way a browser serialises an Origin header
// (RFC 6454): lowercase scheme and host, default port omitted.
//
// isAllowedOrigin matches with ==, so anything that keeps its author's
// capitalisation ("https://Studio.Example") or an explicit default port
// ("https://host:443") is accepted by a shape check and then never matches a
// real request — configured, silent and inert. sameOrigin already compares
// with EqualFold; this is the allowlist half of the same rule.
func normalizeOrigin(u *url.URL) string {
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Host)
	// LastIndex is IPv6-safe here: "[fe80::443]:443" cuts at the port colon,
	// and a bracketed host with no port cannot end in ":443".
	if (scheme == "https" && strings.HasSuffix(host, ":443")) ||
		(scheme == "http" && strings.HasSuffix(host, ":80")) {
		host = host[:strings.LastIndex(host, ":")]
	}
	return scheme + "://" + host
}

func splitAllowedOrigins(raw string) (valid, malformed []string) {
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		u, err := url.Parse(entry)
		// A bare "/" is accepted and dropped: an Origin header carries no
		// path, but "https://host/" is how a human writes one and there is
		// nothing ambiguous to resolve. A real path is refused — it means the
		// author expected path-scoping the gate does not do, so silently
		// widening the whole host would grant more than they asked for.
		//
		// A '*' is refused for the same reason and it is the important one:
		// "https://*.example.com" parses perfectly, so without this it would
		// be accepted, never warned about, and never match — an operator
		// believing a whole subdomain tree was allowed while every request
		// from it is refused. The gate matches exact origins, by design.
		if err != nil || u.Scheme == "" || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || strings.Contains(u.Host, "*") {
			malformed = append(malformed, entry)
			continue
		}
		valid = append(valid, normalizeOrigin(u))
	}
	return valid, malformed
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
	// Unconditional, and that is the point: this response depends on Origin
	// whether or not the origin is allowed, because ACAO is present in one case
	// and absent in the other. Declaring the dimension only on the allowed path
	// lets a cache store the ACAO-less variant under the bare URL and hand it
	// to an allowlisted origin, whose browser then blocks a request that should
	// have succeeded.
	httpx.AddVary(w, "Origin")
	origin := r.Header.Get("Origin")
	if origin != "" && s.isAllowedOriginReq(r) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
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

// httpErrorCode is httpErrorFor with a stable machine-readable `error_code`
// beside the message — the key the studio client and the other coded
// refusals of these endpoints use — for a refusal a client acts on rather
// than displays (an author document named where a workflow was expected:
// `author_document`).
func (s *Server) httpErrorCode(w http.ResponseWriter, r *http.Request, status int, code, format string, args ...any) {
	s.reflectAllowedOrigin(w, r)
	httpx.WriteJSON(w, status, map[string]string{"error": fmt.Sprintf(format, args...), "error_code": code})
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
	// The kill switch is honoured HERE, not only in originGateAllows, because
	// ~70 handlers call this function directly (runs_control, runs_merge,
	// projects, platform_settings, bot_sources, marketplace, …). Read only by
	// the middleware, it left the documented emergency rollback half-working:
	// an operator setting it during an incident recovers the routes gated only
	// by the middleware and keeps collecting unexplained 403s on run
	// cancel/merge and every other handler-gated write. A rollback that works
	// for some routes is worse than none — it burns the incident on the wrong
	// hypothesis.
	//
	// Checked after the allowlist so the normal path is untouched: this costs
	// a getenv only on a request already being refused.
	if os.Getenv("ITERION_REQUIRE_ORIGIN") == "0" {
		return true
	}
	// A refusal is logged because the 403 goes to the caller and nowhere
	// else. Without this line a deployment cannot answer "is the gate
	// refusing anything it should not?" — the question that matters after
	// widening a gate from 70 hand-picked handlers to every /api/ route.
	// The honest check for a client believed to send no Origin (the
	// board-MCP HTTP transport, a desktop build, a second public host) is
	// to look; before this, looking found nothing whether or not anything
	// had been refused, which reads identically to "all good".
	//
	// Info, deliberately, and NOT Warn. pkg/log dispatches its hook at warn
	// and above, and errtrack's hook turns a warn into a Sentry BREADCRUMB on
	// the process-wide hub — a ring of 100. The gate runs before auth, so at
	// warn an unauthenticated caller could evict the whole breadcrumb trail
	// that the next captured error would have carried, with about a hundred
	// requests. Bounding the line does not bound that: it is the RECORD COUNT
	// that evicts, not its size. Info stays below the hook threshold and above
	// the default level (info), so the line is visible in production without
	// letting a stranger degrade everyone's error context.
	//
	// It is also the honest level. A refused cross-origin request is the gate
	// working, not an anomaly; the anomaly is a LEGITIMATE client among them,
	// which no level can distinguish on its own.
	//
	// Unthrottled, though. Suppressing under load is the tempting fix and it
	// is the wrong one: a flood would then hide the single legitimate refusal
	// this log exists to surface, which is the silence being fixed. Each line
	// is bounded instead (logSafe), and request rate belongs to the ingress.
	// Every value here is chosen by the caller being refused, so each goes
	// through logSafe and none through %q: %q would ALSO escape a CRLF, which
	// sounds like belt-and-braces but masks whether the sanitiser works — a
	// test aimed at a %q-rendered value passes with logSafe removed. One
	// stated mechanism, uniformly applied, is the one that stays checkable.
	// Field forging is logSafe's job too, not the field order's: putting the
	// origin last protects only the origin, while the path sits in the middle
	// and can impersonate the field after it. logSafe neutralises the
	// separator for every value, which is what actually holds.
	s.logger.Info("origin gate: refused %s %s from origin %s", logSafe(r.Method), logSafe(r.URL.Path), logSafe(r.Header.Get("Origin")))
	httpx.WriteJSON(w, http.StatusForbidden, map[string]string{
		"error": "cross-origin request rejected: origin not allowed (must be same-origin, loopback, or the configured public URL)",
	})
	return false
}

// logSafeMax bounds an attacker-chosen value in a log line. Long enough for
// any real origin or API path, short enough that a crafted header cannot push
// surrounding context out of a viewer.
const logSafeMax = 128

// logSafe renders an untrusted string as one line of log output: control
// characters (CR/LF above all, which would otherwise let a crafted Origin
// forge whole log records) become '.', and the result is truncated.
func logSafe(v string) string {
	if v == "" {
		return ""
	}
	truncated := false
	if len(v) > logSafeMax {
		v, truncated = v[:logSafeMax], true
	}
	b := []byte(v)
	for i, c := range b {
		// The space goes too, and it is not decoration: the refusal line is
		// space-delimited and r.URL.Path is the DECODED path, so a request to
		// "/api/x%20from%20origin%20https://studio.example" would otherwise
		// log a second, fabricated "from origin" field. Neutralising CR/LF
		// only stops a value becoming another RECORD; a value can equally
		// impersonate the next FIELD of its own line, which is what a grep
		// over this log actually reads.
		if c < 0x20 || c == 0x7f || c == ' ' {
			b[i] = '.'
		}
	}
	if truncated {
		return string(b) + "…"
	}
	return string(b)
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
	confirmedDiskPath := ""
	if err == nil {
		confirmedDiskPath = absPath
	}
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
				src, ok, embedErr := embeddedRecipe(rest)
				if embedErr != nil {
					httpError(w, http.StatusInternalServerError, "%v", embedErr)
					return
				}
				if ok {
					data = []byte(src)
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
	if confirmedDiskPath != "" && len(pr.File.Imports) > 0 {
		// A bot in several files opens as its unit: the fragments read
		// beside the main, merged into one document whose every declaration
		// names its file, with the unit's revision for the save to present.
		u := unit.LoadDirWithMain(absPath, absPath, data)
		diags = diags[:0]
		for _, d := range u.Diagnostics {
			diags = append(diags, d.Error())
		}
		if u.Merged == nil {
			writeJSON(w, parseResponse{Diagnostics: diags})
			return
		}
		docJSON, err := ast.MarshalFileWithProvenance(u.Merged, u.Root)
		if err != nil {
			httpError(w, http.StatusInternalServerError, "marshal error: %v", err)
			return
		}
		// A unit that does not LOAD stays writable, deliberately: saveUnit
		// refuses the write server-side, naming the fragment at fault, and
		// the document is marshalled with provenance, which Save As refuses.
		//
		// A main that did not PARSE is another matter, and this branch is
		// reached for one — a syntax error whose `import` line survived the
		// salvage. The verdict is the main's own parse, the way
		// /api/examples answers for the same file: the merged document is
		// then a salvage too, and Download, Copy source and the Source view
		// have to be told, or they hand the author a program missing what
		// the parser could not read.
		writeJSON(w, unitOpenResponse{Source: string(data), Document: json.RawMessage(docJSON), Diagnostics: diags, Path: req.Path, ConfirmedDiskPath: confirmedDiskPath, Unit: unitInfoOf(u, req.Path), Bindable: !parseHasErrors(pr.Diagnostics)})
		return
	}
	docJSON, err := ast.MarshalFile(pr.File)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "marshal error: %v", err)
		return
	}
	writeJSON(w, openFileResponse{
		Source:            string(data),
		Document:          json.RawMessage(docJSON),
		Diagnostics:       diags,
		Path:              req.Path,
		ConfirmedDiskPath: confirmedDiskPath,
		// The document is what the parser SALVAGED when the parse left
		// errors — the file minus the region it could not read.
		Bindable: !parseHasErrors(pr.Diagnostics),
	})
}

// openFileResponse is the open response of a bot in one file. The path is
// answered whether or not the file parses — the editor is ABOUT that file,
// and the watcher, the tab binding, the validation scope and the assistant's
// perimeter all read it. Bindable false says the document is the salvage, so
// the three sites that write a document refuse it; it lifts when a parse of
// the buffer comes back whole.
type openFileResponse struct {
	Source            string          `json:"source"`
	Document          json.RawMessage `json:"document"`
	Diagnostics       []string        `json:"diagnostics,omitempty"`
	Path              string          `json:"path,omitempty"`
	ConfirmedDiskPath string          `json:"confirmed_disk_path,omitempty"`
	Bindable          bool            `json:"bindable"`
}

// parseHasErrors reports whether a parse left errors — the state in which the
// AST is what the parser could salvage rather than what the file holds.
func parseHasErrors(diags []parser.Diagnostic) bool {
	for _, d := range diags {
		if d.Severity == parser.SeverityError {
			return true
		}
	}
	return false
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
	if s.isMaterializedBotDependencyPath(absPath) {
		httpError(w, http.StatusForbidden, "shared bot bundles under .botz are read-only; edit the source bundle and run iterion bots sync")
		return
	}
	f, err := ast.UnmarshalFile(req.Document)
	if err != nil {
		httpError(w, http.StatusBadRequest, "invalid document: %v", err)
		return
	}
	// A bot in several files saves through its unit: each declaration to
	// the file its provenance names. A document with provenance headed for
	// a file that is not such a bot's main would fold every file into one.
	if current, readErr := os.ReadFile(absPath); readErr == nil && !req.CreateOnly {
		if on := parser.Parse(absPath, string(current)); on.File != nil && len(on.File.Imports) > 0 {
			s.saveUnit(w, r, req, absPath, f, current)
			return
		}
	}
	if hasProvenance(f) {
		httpError(w, http.StatusUnprocessableEntity, "the document was opened from a bot in several files and names those files: it saves to that bot's main, not to %s", req.Path)
		return
	}
	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		httpError(w, http.StatusInternalServerError, "cannot create directory: %v", err)
		return
	}
	locks, err := acquireAuthoringLocalLocks(r.Context(), []string{absPath})
	if err != nil {
		s.authoringError(w, r, err)
		return
	}
	defer closeAuthoringLocalLocks(locks)
	// Revalidate the route's authorized path after a possibly blocked lock.
	freshPath, err := s.safePath(req.Path)
	if err != nil || freshPath != absPath {
		httpError(w, http.StatusConflict, "file path changed while waiting to save")
		return
	}

	// A document may not lower the profile of the file it replaces. The
	// canvas carries the profile it was opened with, so a lower one comes
	// from a client that dropped the header — an older studio build — and
	// the file would be rewritten in profile 1, its strings read otherwise
	// at the next parse, with Verify none the wiser (it holds the text to
	// the document, never to the file).
	// Only "it is not there" is an absence. Every other read failure — the
	// file changed while opening, it is not regular, it exceeds the read
	// limit — would otherwise read as "no before" and silently disarm BOTH
	// guards below, which is how a save stops being checked without anyone
	// seeing it.
	current, _, currentErr := locks[0].parent.read(filepath.Base(absPath), math.MaxInt64-1)
	if currentErr != nil {
		if !errors.Is(currentErr, fs.ErrNotExist) {
			s.authoringError(w, r, currentErr)
			return
		}
		current = nil
	}
	if current != nil {
		if on := parser.ReadPreamble(parser.NormalizeSource(string(current))).Profile; on > f.EffectiveProfile() {
			httpError(w, http.StatusUnprocessableEntity, "%s is written in dsl profile %d and the document would save it in profile %d: reopen the file in the studio (the document carries no profile — an older client dropped it)", req.Path, on, f.EffectiveProfile())
			return
		}
	}
	// The file written must be the document saved: a value the serialiser
	// could not carry (or a construct it does not know) would otherwise land
	// on disk as a different program, and the next parse would run THAT.
	// canon.Text carries the other half: the same program with a value the
	// author wrote over several lines folded onto one is not the same FILE,
	// which `iterion fmt` has refused to write since #1612 — the studio's
	// save is the path that never asked.
	source, err := canon.Text(req.Path, f, current)
	if err != nil {
		if errors.Is(err, canon.ErrRefused) {
			httpError(w, http.StatusUnprocessableEntity, "%s cannot be saved from the studio: %s. Leave the file as it is, or edit it directly", req.Path, canonReason(err))
			return
		}
		httpError(w, http.StatusUnprocessableEntity, "the document cannot be saved as .bot source without changing it: %v", err)
		return
	}
	var ignore func(string)
	s.stateMu.RLock()
	if s.watcher != nil {
		ignore = s.watcher.IgnorePath
	}
	s.stateMu.RUnlock()
	writeErr := locks[0].writeEditorFile([]byte(source), req.CreateOnly, ignore)
	if req.CreateOnly && errors.Is(writeErr, fs.ErrExist) {
		httpError(w, http.StatusConflict, "file already exists")
		return
	}
	if writeErr != nil {
		httpError(w, http.StatusInternalServerError, "write error: %v", writeErr)
		return
	}
	writeJSON(w, saveFileResponse{
		Path:              req.Path,
		Source:            source,
		ConfirmedDiskPath: absPath,
	})
}

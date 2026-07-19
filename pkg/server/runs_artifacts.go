package server

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// handleListAllArtifacts returns the latest published artifact per node
// for a run (the centralized, label-grouped Artifacts view). Tenant-scoped
// like handleListArtifacts.
func (s *Server) handleListAllArtifacts(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing run id")
		return
	}
	if _, err := s.runs.LoadRunCtx(r.Context(), id); err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "run not found: %v", err)
		return
	}
	out, err := s.runs.ListAllArtifacts(id)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "list artifacts: %v", err)
		return
	}
	if out == nil {
		out = []runview.RunArtifactSummary{}
	}
	s.writeJSONFor(w, r, map[string]any{"artifacts": out})
}

func (s *Server) handleListArtifacts(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	node := r.PathValue("node")
	if id == "" || node == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing id or node")
		return
	}
	// Tenant scoping: load the run under the caller's context first so
	// the mongo tenant filter rejects cross-tenant requests before we
	// touch the filesystem-backed ListArtifacts (which has no tenant
	// awareness of its own).
	if _, err := s.runs.LoadRunCtx(r.Context(), id); err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "run not found: %v", err)
		return
	}
	out, err := s.runs.ListArtifacts(id, node)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "list artifacts: %v", err)
		return
	}
	if out == nil {
		out = []runview.ArtifactSummary{}
	}
	s.writeJSONFor(w, r, map[string]any{"artifacts": out})
}

func (s *Server) handleGetArtifact(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	node := r.PathValue("node")
	versionStr := r.PathValue("version")
	if id == "" || node == "" || versionStr == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing id, node, or version")
		return
	}
	version, err := strconv.Atoi(versionStr)
	if err != nil || version < 0 {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid version")
		return
	}
	a, err := s.runs.LoadArtifactCtx(r.Context(), id, node, version)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "artifact not found: %v", err)
		return
	}
	s.writeJSONFor(w, r, a)
}

// handleGetToolBlob streams a slice of a per-tool-call I/O sidecar
// blob (written by the hooks layer when an input/output exceeded the
// inline threshold). Used by the studio's Tools tab to lazy-fetch
// large bodies on demand: events carry only a 4 KB preview + a ref,
// the rest is served paginated from here.
//
// Query params:
//   - offset (int64, default 0): byte offset to start at
//   - limit  (int64, default 0 = "all from offset"): cap bytes returned
//
// Response: raw bytes (Content-Type: text/plain; charset=utf-8) plus
//   - X-Tool-Total-Size: full blob size in bytes
//   - X-Tool-Eof: "true" when offset+len(body) == total, "false" otherwise
//
// Errors:
//   - 400 missing id/toolUseID/kind or kind not in {input,output}
//   - 404 blob not found (call never produced one — i.e. fit inline)
//   - 503 store doesn't satisfy ToolBlobStore (both filesystem and Mongo do)
func (s *Server) handleGetToolBlob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	toolUseID := r.PathValue("toolUseID")
	kind := r.PathValue("kind")
	if id == "" || toolUseID == "" || kind == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing id, toolUseID, or kind")
		return
	}
	if kind != "input" && kind != "output" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "kind must be input or output")
		return
	}
	// Tenant scoping (see handleListArtifacts): reject cross-tenant
	// before reading the blob. Dormant today (mongo lacks ToolBlobStore
	// → 503 below) but keeps the guard uniform for when it lands.
	if _, err := s.runs.LoadRunCtx(r.Context(), id); err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "run not found: %v", err)
		return
	}
	q := r.URL.Query()
	var offset, limit int64
	if v := q.Get("offset"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			s.httpErrorFor(w, r, http.StatusBadRequest, "invalid offset")
			return
		}
		offset = n
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			s.httpErrorFor(w, r, http.StatusBadRequest, "invalid limit")
			return
		}
		limit = n
	}
	body, total, eof, err := s.runs.ReadToolBlobCtx(r.Context(), id, toolUseID, kind, offset, limit)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.httpErrorFor(w, r, http.StatusNotFound, "tool blob not found")
			return
		}
		if strings.Contains(err.Error(), "unavailable for this store") {
			s.httpErrorFor(w, r, http.StatusServiceUnavailable, "tool blobs unavailable in this backend")
			return
		}
		s.httpErrorFor(w, r, http.StatusInternalServerError, "read tool blob: %v", err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Tool-Total-Size", strconv.FormatInt(total, 10))
	if eof {
		w.Header().Set("X-Tool-Eof", "true")
	} else {
		w.Header().Set("X-Tool-Eof", "false")
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	_, _ = w.Write(body)
}

// handleListArtifactFiles returns the manifest of tool-produced files
// (run reports, SBOMs, …) dropped under runs/<id>/artifact_files by
// in-sandbox tools. Returns an empty array (not 404) when the run
// produced no files — distinguishes "valid run, nothing to download"
// from "no such run", which the studio's Artifacts panel renders as
// an empty state.
func (s *Server) handleListArtifactFiles(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing run id")
		return
	}
	// Tenant gate: the S3 read path keys on runID only (no tenant prefix),
	// so cross-tenant isolation MUST be enforced here by loading the run
	// under the caller's tenant ctx first — mirrors handleGetToolBlob.
	// Without it a caller could list another team's artifact files.
	if _, err := s.runs.LoadRunCtx(r.Context(), id); err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "run not found: %v", err)
		return
	}
	files, err := s.runs.ListArtifactFilesCtx(r.Context(), id)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "list artifact files: %v", err)
		return
	}
	if files == nil {
		files = []store.RunFileInfo{}
	}
	s.writeJSONFor(w, r, map[string]any{"files": files})
}

// handleGetArtifactFile streams one tool-produced file by relative
// path. Path-traversal guards live in the store layer; this handler
// just unwraps the wildcard path component and sets a Content-
// Disposition + best-effort Content-Type. Errors map to 404 to keep
// path-probing attacks from distinguishing missing-file vs traversal-
// rejected vs non-RunFilesStore (cloud) backends.
func (s *Server) handleGetArtifactFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	relPath := r.PathValue("path")
	if id == "" || relPath == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing run id or file path")
		return
	}
	// Tenant gate before the tenant-blind S3 read (see handleListArtifactFiles).
	if _, err := s.runs.LoadRunCtx(r.Context(), id); err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "artifact file not found")
		return
	}
	rc, info, err := s.runs.OpenArtifactFileCtx(r.Context(), id, relPath)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "artifact file not found")
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", artifactFileContentType(info.Path))
	if info.Size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(info.Size, 10))
	}
	// Disposition: `inline` by default lets browsers preview .md /
	// .json / images directly; `?download=1` switches to `attachment`
	// for the studio's Download button (the HTML5 `download` attribute
	// alone is unreliable across embedded WebViews + same-origin
	// previewable types). Filename hint is the basename of the path.
	disposition := "inline"
	if r.URL.Query().Get("download") == "1" {
		disposition = "attachment"
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf(`%s; filename=%q`, disposition, filepath.Base(info.Path)))
	if _, copyErr := io.Copy(w, rc); copyErr != nil {
		// Body partially written by now — can't surface a clean error
		// status. Log via the standard server error path; the client
		// will see a truncated response.
		s.logger.Warn("artifact file copy failed for run %s path %s: %v", id, info.Path, copyErr)
	}
}

// artifactFileContentType picks a sensible MIME type by extension.
// Conservative — falls back to application/octet-stream for unknown
// extensions to keep browsers from auto-executing untrusted payloads
// (an in-sandbox tool could emit anything; the recipe's name doesn't
// guarantee semantic content).
func artifactFileContentType(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".md", ".markdown":
		return "text/markdown; charset=utf-8"
	case ".json":
		return "application/json; charset=utf-8"
	case ".txt", ".log":
		return "text/plain; charset=utf-8"
	case ".csv":
		return "text/csv; charset=utf-8"
	case ".html", ".htm":
		return "text/html; charset=utf-8"
	case ".yaml", ".yml":
		return "application/yaml; charset=utf-8"
	case ".png":
		return "image/png"
	case ".svg":
		return "image/svg+xml"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".pdf":
		return "application/pdf"
	// Audio/video produced by media pipelines — without these the studio's
	// inline players get application/octet-stream and refuse to render.
	case ".wav":
		return "audio/wav"
	case ".mp3":
		return "audio/mpeg"
	case ".ogg", ".oga":
		return "audio/ogg"
	case ".flac":
		return "audio/flac"
	case ".m4a":
		return "audio/mp4"
	case ".opus":
		return "audio/opus"
	case ".mp4", ".m4v":
		return "video/mp4"
	case ".webm":
		return "video/webm"
	case ".mov":
		return "video/quicktime"
	case ".mkv":
		return "video/x-matroska"
	default:
		return "application/octet-stream"
	}
}

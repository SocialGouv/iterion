package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"

	gitlib "github.com/SocialGouv/iterion/pkg/git"
)

// handleGetRunFilePreview serves an image directly from a run's exact live
// work_dir. This is intentionally narrower than the text file-content
// endpoint: it is used as an <img> source by the pipeline review UI, so only
// the image formats that UI knows how to render are exposed.
//
// The run is loaded through LoadRunCtx before its filesystem metadata is used,
// preserving the store's tenant boundary. resolveRunWorktreePath then rejects
// absent/gc'd worktrees and applies the shared symlink-aware containment guard,
// so neither traversal segments nor an escaping symlink can reach files
// outside run.WorkDir.
func (s *Server) handleGetRunFilePreview(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	relPath := r.PathValue("path")
	if id == "" || relPath == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing run id or preview path")
		return
	}
	if err := gitlib.ValidateRelPath(relPath); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid preview path: %v", err)
		return
	}

	run, err := s.runs.LoadRunCtx(r.Context(), id)
	if err != nil {
		// Keep tenant-denied and genuinely missing runs indistinguishable.
		s.httpErrorFor(w, r, http.StatusNotFound, "run file preview not found")
		return
	}

	contentType, ok := workspaceImageExts[strings.ToLower(filepath.Ext(relPath))]
	if !ok {
		// The endpoint is not a generic arbitrary-file download surface.
		s.httpErrorFor(w, r, http.StatusNotFound, "run file preview not found")
		return
	}

	absPath, ok := s.resolveRunWorktreePath(w, r, run, relPath)
	if !ok {
		return
	}
	// #nosec G304 — absPath is the output of safePathWithin, which resolves
	// symlinks and verifies containment against the persisted run.WorkDir.
	file, err := os.Open(absPath)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "run file preview not found")
		return
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		s.httpErrorFor(w, r, http.StatusNotFound, "run file preview not found")
		return
	}

	s.reflectAllowedOrigin(w, r)
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Review images may be regenerated in place while a human node is paused.
	// Force revalidation so reopening the same URL shows the newest bytes.
	w.Header().Set("Cache-Control", "private, no-cache")
	http.ServeContent(w, r, filepath.Base(absPath), info.ModTime(), file)
}

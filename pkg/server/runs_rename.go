package server

import (
	"errors"
	"github.com/SocialGouv/iterion/pkg/store"
	"net/http"
	"strings"
)

// renameRunRequest is the body of POST /api/runs/{id}/rename. The id
// stays stable; only the friendly Name field on the run header
// changes. Studio uses this to let users relabel runs ("nightly
// canary", "claw vs codex bake-off", …) without having to remember a
// generated slug.
type renameRunRequest struct {
	Name string `json:"name"`
}

// handleRenameRun answers POST /api/runs/{id}/rename. Cross-store runs
// are read-only (rejectCrossStoreWrite); the local store path updates
// run.Name via the runview service.
func (s *Server) handleRenameRun(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	if s.rejectCrossStoreWrite(w, r) {
		return
	}
	id := r.PathValue("id")
	if id == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing run id")
		return
	}
	var req renameRunRequest
	if err := readJSON(r, &req); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid request: %v", err)
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "name must not be empty")
		return
	}
	if len(name) > 200 {
		s.httpErrorFor(w, r, http.StatusBadRequest, "name too long (max 200 chars)")
		return
	}
	run, err := s.runs.RenameRunCtx(r.Context(), id, name)
	if err != nil {
		code := http.StatusNotFound
		if errors.Is(err, store.ErrRunConflict) {
			code = http.StatusConflict
		}
		s.httpErrorFor(w, r, code, "rename failed: %v", err)
		return
	}
	s.writeJSONFor(w, r, map[string]any{
		"run_id": run.ID,
		"name":   run.Name,
	})
}

package server

import (
	"net/http"

	"github.com/SocialGouv/iterion/pkg/store"
)

// tagsRequest is the body of PUT /api/runs/{id}/tags — the full replacement
// tag set. It's a whole-list overwrite (not a merge): the studio sends the
// complete chip list after each add/remove.
type tagsRequest struct {
	Tags []string `json:"tags"`
}

// handleGetRunTags answers GET /api/runs/{id}/tags. Returns the run's
// operator-assigned tags (an empty array — not 404 — for a valid run with
// none, so the studio's chip row renders a clean empty state). Tenant-
// scoped like the other run sub-resource handlers: load the run under the
// caller's context first so the mongo tenant filter rejects cross-tenant
// requests.
func (s *Server) handleGetRunTags(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing run id")
		return
	}
	if _, err := s.runs.LoadRunCtx(r.Context(), id); err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "run not found: %v", err)
		return
	}
	tags, err := s.runs.GetRunTagsCtx(r.Context(), id)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "get tags: %v", err)
		return
	}
	if tags == nil {
		tags = []string{}
	}
	s.writeJSONFor(w, r, map[string]any{"tags": tags})
}

// handleSetRunTags answers PUT /api/runs/{id}/tags. Normalizes the
// submitted list (trim/dedup/limit) and replaces the run's full tag set.
// A tag over the length cap or too many tags is a 400 (NormalizeTags),
// never a silent truncation. Mutating, so it goes through the same
// safe-origin + cross-store-write guards as rename/merge.
func (s *Server) handleSetRunTags(w http.ResponseWriter, r *http.Request) {
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
	var req tagsRequest
	if err := readJSON(r, &req); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid request: %v", err)
		return
	}
	tags, err := store.NormalizeTags(req.Tags)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid tags: %v", err)
		return
	}
	// Tenant gate: confirm the run exists under the caller's context before
	// the (tenant-blind on run_id) write, mirroring the other sub-resource
	// handlers.
	if _, err := s.runs.LoadRunCtx(r.Context(), id); err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "run not found: %v", err)
		return
	}
	if err := s.runs.SetRunTagsCtx(r.Context(), id, tags); err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "set tags: %v", err)
		return
	}
	s.writeJSONFor(w, r, map[string]any{"tags": tags})
}

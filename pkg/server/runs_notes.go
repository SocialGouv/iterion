package server

import (
	"net/http"
	"strings"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/store"
)

// addNoteRequest is the POST /api/runs/{id}/notes body. author is
// optional — the handler falls back to the caller's identity (cloud
// mode) or "operator" (local mode) when it's blank.
type addNoteRequest struct {
	Body   string `json:"body"`
	Author string `json:"author,omitempty"`
}

// handleListNotes returns the run's freeform operator notes in
// chronological order (filesystem runs/<id>/notes/ or the Mongo
// run_notes collection in cloud mode). Returns an empty array (not 404)
// for a valid run with no notes so the studio run header renders a clean
// empty state. Tenant-scoped like the other run sub-resource handlers:
// load the run under the caller's context first so the Mongo tenant
// filter rejects cross-tenant requests.
func (s *Server) handleListNotes(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing run id")
		return
	}
	if _, err := s.runs.LoadRunCtx(r.Context(), id); err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "run not found: %v", err)
		return
	}
	notes, err := s.runs.ListRunNotesCtx(r.Context(), id)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "list notes: %v", err)
		return
	}
	if notes == nil {
		notes = []store.RunNote{}
	}
	s.writeJSONFor(w, r, map[string]any{"notes": notes})
}

// handleAddNote appends a freeform operator note to the run and returns
// the created note (201). Notes are immutable once created — this first
// cut has no edit/delete. Tenant-scoped: the run is loaded under the
// caller's context so a cross-tenant POST is rejected before any write.
func (s *Server) handleAddNote(w http.ResponseWriter, r *http.Request) {
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
	var req addNoteRequest
	if err := readJSON(r, &req); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid request: %v", err)
		return
	}
	if strings.TrimSpace(req.Body) == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "body is required")
		return
	}
	// Tenant gate: reject a cross-tenant write before persisting.
	if _, err := s.runs.LoadRunCtx(r.Context(), id); err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "run not found: %v", err)
		return
	}
	author := strings.TrimSpace(req.Author)
	if author == "" {
		author = noteAuthorFromContext(r)
	}
	note, err := s.runs.AddRunNoteCtx(r.Context(), id, author, req.Body)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "add note: %v", err)
		return
	}
	w.WriteHeader(http.StatusCreated)
	s.writeJSONFor(w, r, note)
}

// noteAuthorFromContext derives a display author for a note from the
// authenticated caller (cloud mode), preferring the email, then the
// user id, and finally "operator" for an unauthenticated local run.
func noteAuthorFromContext(r *http.Request) string {
	if idn, ok := auth.FromContext(r.Context()); ok {
		if idn.Email != "" {
			return idn.Email
		}
		if idn.UserID != "" {
			return idn.UserID
		}
	}
	return "operator"
}

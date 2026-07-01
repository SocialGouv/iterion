package server

import (
	"net/http"

	"github.com/SocialGouv/iterion/pkg/auth"
)

// handleDeleteRun permanently removes a run and ALL of its data — the run
// document, events, seq counter, interactions, queued messages, artifacts
// and attachments (see store.RunStore.DeleteRun). Backs the studio
// "Delete run" action + programmatic cleanup of obsolete runs.
//
// Authz: tenant-scoped. DeleteRunCtx LoadRuns the run under the request's
// tenant first, so a caller can only delete a run in their active team's
// scope; a run outside it surfaces as 404 (never a cross-tenant delete).
func (s *Server) handleDeleteRun(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	// Delete mutates the store; reject on a read-only cross-store view.
	if s.rejectCrossStoreWrite(w, r) {
		return
	}
	id := r.PathValue("id")
	if id == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing run id")
		return
	}
	if err := s.runs.DeleteRunCtx(r.Context(), id); err != nil {
		// LoadRun-not-found (gone or outside the caller's tenant) → 404.
		s.httpErrorFor(w, r, http.StatusNotFound, "delete run: %v", err)
		return
	}
	ident, _ := auth.FromContext(r.Context())
	s.auditTenant(r, ident.TeamID, "run.deleted", "run", id, nil)
	w.WriteHeader(http.StatusNoContent)
}

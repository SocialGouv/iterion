package server

import (
	"errors"
	"net/http"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/runview"
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
	// A request the gate admitted on its expected birth deletes that run
	// only: the load and the store's delete are bound to it. When another
	// delete of the admitted run lands between the two, the answer is the
	// store's: the filesystem store refuses (ErrRunDeleted, answered 409
	// run_origin_lost), the Mongo store, whose tombstone keeps the birth,
	// answers the idempotent success. A delete that fails part-way after its
	// bound check leaves the admitted run deleted by this request: its own
	// failure is the answer (writeDeleteFailure).
	a := s.admittedAction(r, id)
	ctx, ok := s.actionContext(w, r, a)
	if !ok {
		return
	}
	if err := s.runs.DeleteRunCtx(ctx, id); err != nil {
		if s.writeDeleteFailure(w, r, a, err) {
			return
		}
		// A lifecycle refusal (the run exists and is ALIVE) is a 409 —
		// answering 404 told the API/MCP caller the exact opposite of the
		// refusal's own reason. LoadRun-not-found (gone or outside the
		// caller's tenant) stays 404.
		if errors.Is(err, runview.ErrRunNotDeletable) {
			s.httpErrorFor(w, r, http.StatusConflict, "delete run: %v", err)
			return
		}
		s.httpErrorFor(w, r, http.StatusNotFound, "delete run: %v", err)
		return
	}
	ident, _ := auth.FromContext(r.Context())
	s.auditTenant(r, ident.TeamID, "run.deleted", "run", id, nil)
	w.WriteHeader(http.StatusNoContent)
}

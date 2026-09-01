package server

import (
	"net/http"

	"github.com/SocialGouv/iterion/pkg/store"
)

// handleListRouteDecisions serves GET /api/runs/{id}/route-decisions —
// the outcome router's audit surface: every decision the router took
// about this run (one row per terminal episode, newest first), with the
// contract hash that decided, the action's outcome and the attempt
// count. Escalate is the router's DEFAULT decision; this endpoint is
// what makes a decision readable after the fact (the ops alert is the
// push half).
func (s *Server) handleListRouteDecisions(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "missing run id")
		return
	}
	// Visibility rides the run's own tenancy: a caller who cannot load
	// the run cannot read its decisions.
	if _, err := s.runs.LoadRunCtx(r.Context(), id); err != nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "run not found: %v", err)
		return
	}
	rds := store.AsRouteDecisionStore(s.cfg.Store)
	if rds == nil {
		// A store without the registry has, truthfully, no decisions.
		s.writeJSONFor(w, r, []store.RouteDecision{})
		return
	}
	ds, err := rds.ListRouteDecisions(r.Context(), id)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "list route decisions: %v", err)
		return
	}
	if ds == nil {
		ds = []store.RouteDecision{}
	}
	s.writeJSONFor(w, r, ds)
}

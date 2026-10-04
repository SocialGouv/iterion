// Admin team console: the super-admin surface for team-scoped settings an
// org admin must not touch. The sovereign runner-pool mapping lives here —
// a boundary field, not a preference, so it is written by exactly one route
// and audited on every write.
package server

import (
	"net/http"

	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/queue"
)

// registerAdminTeamRoutes wires the super-admin team console.
func (s *Server) registerAdminTeamRoutes() {
	s.mux.Handle("PUT /api/admin/teams/{id}/runner-pool", s.requireSuperAdmin(http.HandlerFunc(s.handleAdminSetTeamRunnerPool)))
}

// handleAdminSetTeamRunnerPool maps (or unmaps) a team onto a sovereign
// runner pool. The ONLY writer of Team.RunnerPool: any other writer is a
// whole-document replace whose stale read would silently unmap the team.
// Body: {"runner_pool": ""} to unmap, {"runner_pool": "<name>"} to map.
// Empty means the shared default pool. Audited on every write.
func (s *Server) handleAdminSetTeamRunnerPool(w http.ResponseWriter, r *http.Request) {
	teamID := r.PathValue("id")
	var req struct {
		RunnerPool *string `json:"runner_pool"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.RunnerPool == nil {
		httpError(w, http.StatusBadRequest, "runner_pool is required (a string; empty clears the mapping)")
		return
	}
	pool := *req.RunnerPool
	if pool != "" && !queue.ValidPoolName(pool) {
		httpError(w, http.StatusUnprocessableEntity, "runner_pool %q invalid (want 1–31 chars [a-z0-9-], starting alphanumeric)", pool)
		return
	}
	cur, err := s.authStore().GetTeam(r.Context(), teamID)
	if err != nil {
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	updated, err := s.authStore().PatchTeam(r.Context(), teamID, identity.TeamPatch{RunnerPool: &pool})
	if err != nil {
		httpError(w, mapAuthErrorStatus(err), "%s", err.Error())
		return
	}
	// A boundary field's trail must answer what the team WAS, not only what
	// it became. Scoped like every sibling team event: tenant trail for the
	// org's own audit view, org mirror so the org admin sees the change that
	// will refuse their resumes and launches.
	s.auditTenant(r, teamID, "team.runner_pool_set", "team", teamID, map[string]any{
		"runner_pool": pool, "previous": cur.RunnerPool,
	})
	if cur.OrgID != "" {
		s.auditOrg(r, cur.OrgID, "team.runner_pool_set", "team", teamID, map[string]any{
			"runner_pool": pool, "previous": cur.RunnerPool,
		})
	}
	writeJSON(w, toTeamSummaryView(updated))
}

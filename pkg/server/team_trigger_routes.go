package server

import (
	"context"
	"net/http"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
)

// teamScopeKey carries the team a /api/teams/{id}/… route names, once its
// membership is checked, to triggerTenant.
type teamScopeKey struct{}

// teamScopedTenant is the team a team-family route was authorized for; ok
// is false on every other route.
func teamScopedTenant(ctx context.Context) (string, bool) {
	t, ok := ctx.Value(teamScopeKey{}).(string)
	return t, ok
}

// teamTriggerAuth gates the /api/teams/{id}/… family: an authenticated
// identity that may view the path's team reads it, one holding the member
// rung there writes — a viewer reads and writes nothing. The team then
// scopes the handler (triggerTenant), whatever team the credential's
// session has active.
func (s *Server) teamTriggerAuth(next http.Handler) http.Handler {
	return s.requireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, _ := auth.FromContext(r.Context())
		// {id} is the team, canonicalized to its id by requireAuth (#1931).
		team := r.PathValue("id")
		if team == "" || !s.canViewTeam(r.Context(), id, team) {
			httpError(w, http.StatusForbidden, "not a member")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			allowed, err := s.teamRoleOrFail(r.Context(), id, team, identity.RoleMember)
			if err != nil {
				httpError(w, http.StatusInternalServerError, "%s", err.Error())
				return
			}
			if !allowed {
				httpError(w, http.StatusForbidden, "a member's rung is required to change this team's triggers")
				return
			}
		}
		scoped := r.WithContext(context.WithValue(r.Context(), teamScopeKey{}, team))
		// The shared handlers read the row's id as {id}: hand them {sub},
		// the team now travelling in the context.
		if sub := r.PathValue("sub"); sub != "" {
			scoped.SetPathValue("id", sub)
		}
		next.ServeHTTP(w, scoped)
	}))
}

// registerTeamTriggerRoutes mounts the trigger, intake and pair families
// under /api/teams/{id}/… (ADR-168 §6): the handlers of /api/v1/…, whose
// scope is the request's active team, scoped here by the path's.
func (s *Server) registerTeamTriggerRoutes() {
	s.mux.Handle("GET /api/teams/{id}/sources", s.requireAuth(http.HandlerFunc(s.handleTeamListSources)))
	s.mux.Handle("GET /api/teams/{id}/triggers", s.teamTriggerAuth(http.HandlerFunc(s.handleListTriggers)))
	s.mux.Handle("POST /api/teams/{id}/triggers", s.teamTriggerAuth(http.HandlerFunc(s.handleCreateTrigger)))
	s.mux.Handle("POST /api/teams/{id}/triggers/preview", s.teamTriggerAuth(http.HandlerFunc(s.handleTriggerPreview)))
	s.mux.Handle("GET /api/teams/{id}/triggers/{sub}", s.teamTriggerAuth(http.HandlerFunc(s.handleGetTrigger)))
	s.mux.Handle("PUT /api/teams/{id}/triggers/{sub}", s.teamTriggerAuth(http.HandlerFunc(s.handleUpdateTrigger)))
	s.mux.Handle("DELETE /api/teams/{id}/triggers/{sub}", s.teamTriggerAuth(http.HandlerFunc(s.handleDeleteTrigger)))
	s.mux.Handle("POST /api/teams/{id}/bots/{name}/triggers/from-invocation", s.teamTriggerAuth(http.HandlerFunc(s.handleTriggerFromInvocation)))
	if s.connectorIntakes != nil {
		s.mux.Handle("GET /api/teams/{id}/intakes", s.teamTriggerAuth(http.HandlerFunc(s.handleListIntakes)))
		s.mux.Handle("GET /api/teams/{id}/intakes/{sub}", s.teamTriggerAuth(http.HandlerFunc(s.handleGetIntake)))
		s.mux.Handle("POST /api/teams/{id}/intakes/{sub}/replay", s.teamTriggerAuth(http.HandlerFunc(s.handleReplayIntake)))
		s.mux.Handle("GET /api/teams/{id}/pairs", s.teamTriggerAuth(http.HandlerFunc(s.handleListPairs)))
		s.mux.Handle("POST /api/teams/{id}/pairs/{sub}/replay", s.teamTriggerAuth(http.HandlerFunc(s.handleReplayPair)))
	}
}

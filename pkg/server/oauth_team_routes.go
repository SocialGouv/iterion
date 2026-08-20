package server

import (
	"net/http"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// registerOAuthTeamRoutes wires the team/org-scoped mirror of the
// per-user OAuth-forfait endpoints. An org credential is stored as an
// ordinary OAuthRecord under secrets.OrgOwnerKey(teamID) and is used by
// the cloud publisher as a FALLBACK for runs whose owner has no personal
// forfait (webhook/dispatcher/cron). Mutations require team-admin; the
// list is viewer-visible.
//
// ToS note: an org-shared forfait is a dev/test convenience, not a
// production-automation credential — the studio surfaces a warning at
// connect time and the docs spell it out. The server does not gate on it
// (it's the operator's call) but keeps the path admin-only.
func (s *Server) registerOAuthTeamRoutes() {
	s.mux.Handle("GET /api/teams/{id}/oauth/connections", s.requireAuth(http.HandlerFunc(s.handleTeamListOAuth)))
	s.mux.Handle("POST /api/teams/{id}/oauth/{kind}/authorize/start", s.requireAuth(http.HandlerFunc(s.handleTeamStartOAuth)))
	s.mux.Handle("POST /api/teams/{id}/oauth/{kind}/authorize/complete", s.requireAuth(http.HandlerFunc(s.handleTeamCompleteOAuth)))
	s.mux.Handle("POST /api/teams/{id}/oauth/{kind}/credentials", s.requireAuth(http.HandlerFunc(s.handleTeamUploadOAuth)))
	s.mux.Handle("POST /api/teams/{id}/oauth/{kind}/refresh", s.requireAuth(http.HandlerFunc(s.handleTeamRefreshOAuth)))
	s.mux.Handle("DELETE /api/teams/{id}/oauth/{kind}", s.requireAuth(http.HandlerFunc(s.handleTeamDeleteOAuth)))
}

func (s *Server) handleTeamListOAuth(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canViewTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "forbidden")
		return
	}
	s.listOAuthForOwner(w, r, secrets.OrgOwnerKey(teamID))
}

func (s *Server) handleTeamStartOAuth(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canManageTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "forbidden")
		return
	}
	s.startOAuthForOwner(w, r, secrets.OrgOwnerKey(teamID), secrets.OAuthKind(r.PathValue("kind")))
}

func (s *Server) handleTeamCompleteOAuth(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canManageTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "forbidden")
		return
	}
	// Audit fires inside completeOAuthForOwner on success only.
	s.completeOAuthForOwner(w, r, secrets.OrgOwnerKey(teamID), secrets.OAuthKind(r.PathValue("kind")))
}

func (s *Server) handleTeamUploadOAuth(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canManageTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "forbidden")
		return
	}
	s.uploadOAuthForOwner(w, r, secrets.OrgOwnerKey(teamID), secrets.OAuthKind(r.PathValue("kind")))
}

func (s *Server) handleTeamRefreshOAuth(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canManageTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "forbidden")
		return
	}
	s.refreshOAuthForOwner(w, r, secrets.OrgOwnerKey(teamID), secrets.OAuthKind(r.PathValue("kind")))
}

func (s *Server) handleTeamDeleteOAuth(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canManageTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "forbidden")
		return
	}
	s.deleteOAuthForOwner(w, r, secrets.OrgOwnerKey(teamID), secrets.OAuthKind(r.PathValue("kind")))
}

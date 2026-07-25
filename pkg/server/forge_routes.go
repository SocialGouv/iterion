package server

import (
	"net/http"
)

func (s *Server) registerForgeRoutes() {
	s.mux.Handle("GET /api/teams/{id}/forge/repos", s.requireAuth(http.HandlerFunc(s.handleListTeamForgeRepos)))
	s.mux.Handle("POST /api/teams/{id}/forge/repos", s.requireAuth(http.HandlerFunc(s.handleCreateForgeRepo)))
	s.mux.Handle("GET /api/teams/{id}/forge/connections/{conn_id}/health", s.requireAuth(http.HandlerFunc(s.handleForgeConnectionHealth)))
	s.mux.Handle("POST /api/teams/{id}/forge/connections/{conn_id}/refresh", s.requireAuth(http.HandlerFunc(s.handleForgeConnectionRefresh)))
	s.mux.Handle("GET /api/teams/{id}/forge/connections", s.requireAuth(http.HandlerFunc(s.handleListForgeConnections)))
	s.mux.Handle("POST /api/teams/{id}/forge/connections", s.requireAuth(http.HandlerFunc(s.handleConnectForge)))
	s.mux.Handle("DELETE /api/teams/{id}/forge/connections/{conn_id}", s.requireAuth(http.HandlerFunc(s.handleDeleteForgeConnection)))
	s.mux.Handle("GET /api/teams/{id}/forge/connections/{conn_id}/repos", s.requireAuth(http.HandlerFunc(s.handleListForgeRepos)))
	// Public IdP redirect targets (see isPublicPath); authenticate via the
	// signed state + the agent-binding cookie.
	s.mux.HandleFunc("GET /api/forge/oauth/callback", s.handleForgeOAuthCallback)
	s.mux.HandleFunc("GET /api/forge/github/app/callback", s.handleForgeGitHubAppCallback)
}

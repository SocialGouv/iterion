package server

import (
	"net/http"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/auth/oidc"
)

// registerForgeGitHubOrgsRoutes wires a read:org OAuth that lists the signed-in
// user's GitHub orgs, so the "Create a GitHub App" form offers a dropdown of
// real orgs instead of a free-text field. It reuses the deployment's global
// GitHub OAuth app (the SSO-login connector); the result is persisted on the
// user and used ONLY as a UI hint (never an authorization input).
func (s *Server) registerForgeGitHubOrgsRoutes() {
	s.mux.Handle("GET /api/forge/github/orgs/start", s.requireAuth(http.HandlerFunc(s.handleStartGitHubOrgs)))
	// The callback is a SUBDIRECTORY of the SSO GitHub callback
	// (/api/auth/oidc/github/callback) on purpose: GitHub only accepts a
	// redirect_uri that's a subdirectory of the OAuth App's registered callback,
	// and that's the one the deployment's global GitHub OAuth app registers — so
	// no extra callback URL needs adding to the App. Public via the
	// /api/auth/oidc/ prefix in isPublicPath; authed by the signed state.
	s.mux.HandleFunc("GET /api/auth/oidc/github/callback/orgs", s.handleGitHubOrgsCallback)
	s.mux.Handle("GET /api/me/github-orgs", s.requireAuth(http.HandlerFunc(s.handleListMyGitHubOrgs)))
}

func (s *Server) githubOrgsRedirectURI() string {
	return strings.TrimRight(s.cfg.PublicURL, "/") + "/api/auth/oidc/github/callback/orgs"
}

// handleStartGitHubOrgs returns the GitHub authorize URL (read:org via the
// global OAuth app); the studio redirects the window there.
func (s *Server) handleStartGitHubOrgs(w http.ResponseWriter, r *http.Request) {
	if s.forgeStates == nil {
		httpError(w, http.StatusNotFound, "not enabled")
		return
	}
	id, _ := auth.FromContext(r.Context())
	conn, _, _, err := s.resolveConnector(r.Context(), "github")
	if err != nil {
		httpError(w, http.StatusBadRequest, "github sign-in is not configured on this server")
		return
	}
	state, _, _, err := oidc.GenerateStateAndPKCE()
	if err != nil {
		httpError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := s.forgeStates.put(forgePending{
		State:    state,
		UserID:   id.UserID,
		NextURL:  safeNext(r.URL.Query().Get("next")),
		IssuedAt: time.Now().UTC(),
	}); err != nil {
		if s.logger != nil {
			s.logger.Error("forge connect: %v", err)
		}
		httpError(w, http.StatusBadGateway, "could not persist OAuth state — try again")
		return
	}
	authURL, err := conn.AuthorizeURL(r.Context(), s.githubOrgsRedirectURI(), state, "")
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%v", err)
		return
	}
	writeJSON(w, map[string]any{"authorize_url": authURL})
}

// handleGitHubOrgsCallback exchanges the code, reads the user's GitHub orgs from
// the connector's group keys, persists them on the user, and redirects back.
func (s *Server) handleGitHubOrgsCallback(w http.ResponseWriter, r *http.Request) {
	if s.forgeStates == nil {
		httpError(w, http.StatusNotFound, "not enabled")
		return
	}
	state, code := r.URL.Query().Get("state"), r.URL.Query().Get("code")
	if state == "" || code == "" {
		httpError(w, http.StatusBadRequest, "missing state or code")
		return
	}
	pending, ok := s.forgeStates.take(state)
	if !ok {
		httpError(w, http.StatusBadRequest, "state expired or invalid")
		return
	}
	conn, _, _, err := s.resolveConnector(r.Context(), "github")
	if err != nil {
		httpError(w, http.StatusBadRequest, "github not configured")
		return
	}
	ext, err := conn.ExchangeCode(r.Context(), code, s.githubOrgsRedirectURI(), "")
	if err != nil {
		httpError(w, http.StatusBadGateway, "github exchange failed: %v", err)
		return
	}
	if st := s.authStore(); st != nil && pending.UserID != "" {
		if u, err := st.GetUser(r.Context(), pending.UserID); err == nil {
			u.GitHubOrgs = githubOrgsFromGroups(ext.Groups)
			u.UpdatedAt = time.Now().UTC()
			if err := st.UpdateUser(r.Context(), u); err != nil {
				httpError(w, http.StatusInternalServerError, "persist github orgs: %v", err)
				return
			}
		}
	}
	target := pending.NextURL
	if target == "" {
		target = "/"
	}
	http.Redirect(w, r, target, http.StatusFound)
}

// handleListMyGitHubOrgs returns the caller's persisted GitHub org logins.
func (s *Server) handleListMyGitHubOrgs(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	orgs := []string{}
	if st := s.authStore(); st != nil {
		if u, err := st.GetUser(r.Context(), id.UserID); err == nil && len(u.GitHubOrgs) > 0 {
			orgs = u.GitHubOrgs
		}
	}
	writeJSON(w, map[string]any{"orgs": orgs})
}

// githubOrgsFromGroups extracts the distinct org logins from the connector's
// "<org>/*" / "<org>/<team>" group keys.
func githubOrgsFromGroups(groups []string) []string {
	seen := map[string]struct{}{}
	out := []string{}
	for _, g := range groups {
		org := g
		if i := strings.IndexByte(g, '/'); i >= 0 {
			org = g[:i]
		}
		if org == "" {
			continue
		}
		if _, dup := seen[org]; dup {
			continue
		}
		seen[org] = struct{}{}
		out = append(out, org)
	}
	return out
}

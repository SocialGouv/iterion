package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/store"
)

// registerForgeOAuthAppRoutes wires the per-tenant forge OAuth-app credential
// CRUD. These replace the legacy process-global env map: an admin registers (or
// later auto-creates) an OAuth app per (provider, instance), and the connect
// flow resolves it from the store — no env var, no redeploy.
func (s *Server) registerForgeOAuthAppRoutes() {
	s.mux.Handle("GET /api/teams/{id}/forge/oauth-apps", s.requireAuth(http.HandlerFunc(s.handleListForgeOAuthApps)))
	s.mux.Handle("POST /api/teams/{id}/forge/oauth-apps", s.requireAuth(http.HandlerFunc(s.handleRegisterForgeOAuthApp)))
	s.mux.Handle("DELETE /api/teams/{id}/forge/oauth-apps/{app_id}", s.requireAuth(http.HandlerFunc(s.handleDeleteForgeOAuthApp)))
	s.registerForgeGitHubManifestRoutes()
	s.registerForgeGitHubOrgsRoutes()
}

// forgeOAuthAppReq registers an OAuth app for a (provider, instance). mode
// "manual" pastes an existing client_id/client_secret; "auto" calls the forge's
// create-app API with a pasted admin token; "auto_from_connection" reuses an
// existing PAT/OAuth connection's token to create the app (no re-paste).
type forgeOAuthAppReq struct {
	Provider     string `json:"provider"`
	ForgeBaseURL string `json:"forge_base_url,omitempty"`
	Mode         string `json:"mode,omitempty"`
	ClientID     string `json:"client_id,omitempty"`
	ClientSecret string `json:"client_secret,omitempty"`
	AdminToken   string `json:"admin_token,omitempty"`   // mode=auto
	ConnectionID string `json:"connection_id,omitempty"` // mode=auto_from_connection
	Next         string `json:"next,omitempty"`          // github-manifest: studio return path
	// GitHubOrg (github-manifest): create the App UNDER this org so it can be
	// installed org-wide. Empty = the user's personal account (a private App is
	// then installable only there — the cause of "only your personal account").
	GitHubOrg string `json:"github_org,omitempty"`
	// AllowRepoCreation (github-manifest): request administration:write on
	// the App so iterion can CREATE repositories in the installed org
	// (opt-in — the connect wizard surfaces it as a visible checkbox;
	// absent keeps the least-privilege baseline).
	AllowRepoCreation bool `json:"allow_repo_creation,omitempty"`
}

func (s *Server) handleListForgeOAuthApps(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canViewTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "not a member")
		return
	}
	apps, err := s.forgeOAuthApps.ListByTenant(store.WithTenant(r.Context(), teamID), teamID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "list oauth apps: %v", err)
		return
	}
	for i := range apps {
		// Installable = a manifest-created GitHub App whose private key we hold,
		// so it can be INSTALLED (least-privilege github_app), not only OAuth-used.
		apps[i].Installable = len(apps[i].SealedPrivateKey) > 0
		apps[i].SealedSecret = nil     // defensive — also json:"-"
		apps[i].SealedPrivateKey = nil // defensive — also json:"-"
	}
	writeJSON(w, map[string]any{"apps": apps})
}

func (s *Server) handleRegisterForgeOAuthApp(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canManageTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "admin or owner required")
		return
	}
	var req forgeOAuthAppReq
	if !decodeJSON(w, r, &req) {
		return
	}
	provider := forge.Provider(strings.TrimSpace(req.Provider))
	if !provider.Valid() {
		httpError(w, http.StatusBadRequest, "unknown provider %q", req.Provider)
		return
	}
	mode := strings.TrimSpace(req.Mode)
	if mode == "" {
		mode = "manual"
	}
	switch mode {
	case "manual":
		clientID := strings.TrimSpace(req.ClientID)
		clientSecret := strings.TrimSpace(req.ClientSecret)
		if clientID == "" || clientSecret == "" {
			httpError(w, http.StatusBadRequest, "client_id and client_secret are required for mode=manual")
			return
		}
		app, err := s.createForgeOAuthApp(r, teamID, id.UserID, provider, req.ForgeBaseURL, clientID, clientSecret, "", false, mode, githubAppFacts{})
		if err != nil {
			s.writeForgeOAuthAppError(w, err)
			return
		}
		writeJSON(w, app)
	case "auto", "auto_from_connection":
		s.autoCreateForgeOAuthApp(w, r, teamID, id.UserID, provider, mode, req)
	default:
		httpError(w, http.StatusBadRequest, "unknown mode %q", mode)
	}
}

// autoCreateForgeOAuthApp obtains an admin token (pasted, or reused from an
// existing connection), calls the forge's create-app API, then persists the
// returned credentials — so the operator never hand-creates the app on the
// forge or pastes its secret.
func (s *Server) autoCreateForgeOAuthApp(w http.ResponseWriter, r *http.Request, teamID, userID string, provider forge.Provider, mode string, req forgeOAuthAppReq) {
	baseURL := forge.CanonicalBaseURL(provider, req.ForgeBaseURL)
	var adminToken string
	switch mode {
	case "auto":
		adminToken = strings.TrimSpace(req.AdminToken)
		if adminToken == "" {
			httpError(w, http.StatusBadRequest, "admin_token is required for mode=auto")
			return
		}
	case "auto_from_connection":
		connID := strings.TrimSpace(req.ConnectionID)
		if connID == "" {
			httpError(w, http.StatusBadRequest, "connection_id is required for mode=auto_from_connection")
			return
		}
		conn, ok := s.forgeConnForTenant(w, r, teamID, connID)
		if !ok {
			return // forgeConnForTenant already wrote the 404
		}
		tok, err := forge.AdminTokenFor(s.sealer, conn)
		if err != nil {
			httpError(w, http.StatusInternalServerError, "open connection token: %v", err)
			return
		}
		adminToken = tok
		if strings.TrimSpace(req.ForgeBaseURL) == "" {
			baseURL = conn.BaseURL() // default to the connection's instance
		}
	}

	prov, err := s.forgeOAuthAppProvisioner(provider, baseURL, adminToken)
	if err != nil {
		httpError(w, http.StatusBadRequest, "%v", err)
		return
	}
	creds, err := prov.CreateOAuthApp(r.Context(), forge.OAuthAppSpec{
		Name:         "iterion",
		RedirectURI:  s.forgeOAuthRedirectURI(),
		Scopes:       forgeDefaultOAuthScopes(provider),
		Confidential: true,
	})
	if err != nil {
		s.writeForgeOAuthAppError(w, err)
		return
	}
	app, err := s.createForgeOAuthApp(r, teamID, userID, provider, baseURL, creds.ClientID, creds.ClientSecret, creds.ProviderAppID, true, mode, githubAppFacts{})
	if err != nil {
		s.writeForgeOAuthAppError(w, err)
		return
	}
	writeJSON(w, app)
}

func (s *Server) handleDeleteForgeOAuthApp(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canManageTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "admin or owner required")
		return
	}
	appID := r.PathValue("app_id")
	ctx := store.WithTenant(r.Context(), teamID)
	app, err := s.forgeOAuthApps.Get(ctx, appID)
	if err != nil || app.TenantID != teamID {
		httpError(w, http.StatusNotFound, "oauth app not found")
		return
	}
	// Refuse to strand a connection. Now that a Connection names the app whose
	// key mints its installation tokens, deleting that app out from under it
	// would leave a connection that cannot mint — and the failure would surface
	// later, in the background refresh worker, far from this action.
	if used, err := s.connectionsUsingOAuthApp(ctx, teamID, appID); err != nil {
		httpError(w, http.StatusInternalServerError, "check connections using this app: %v", err)
		return
	} else if len(used) > 0 {
		httpError(w, http.StatusConflict,
			"%d connection(s) still use this GitHub App (%s) — remove them first, then delete the app",
			len(used), strings.Join(used, ", "))
		return
	}
	if err := s.forgeOAuthApps.Delete(ctx, appID); err != nil {
		httpError(w, http.StatusInternalServerError, "delete oauth app: %v", err)
		return
	}
	s.auditTenant(r, teamID, "forge.oauth_app.deleted", "forge_oauth_app", appID, nil)
	w.WriteHeader(http.StatusNoContent)
}

// createForgeOAuthApp seals the client_secret, persists the app row, and audits
// it. Shared by the manual-register handler and (later) the auto-create modes —
// connectionsUsingOAuthApp returns the display names of a team's connections
// that are bound to an app record, so deletion can refuse with a message naming
// what would break rather than a bare conflict.
func (s *Server) connectionsUsingOAuthApp(ctx context.Context, teamID, appID string) ([]string, error) {
	if s.forgeConnections == nil {
		return nil, nil
	}
	conns, err := s.forgeConnections.ListByTenant(ctx, teamID)
	if err != nil {
		return nil, err
	}
	var used []string
	for _, c := range conns {
		if c.OAuthAppID == appID {
			name := c.DisplayName
			if name == "" {
				name = c.ID
			}
			used = append(used, name)
		}
	}
	return used, nil
}

// githubAppFacts carries the GitHub-App-only attributes of a forge app record.
// They travel together (a manifest conversion produces all of them at once, and
// every non-GitHub mode leaves all of them empty), so they are one parameter
// rather than four positional strings.
type githubAppFacts struct {
	// ManageURL deep-links the App's settings page for operator cleanup.
	ManageURL string
	// Slug is the github.com/apps/<slug> segment used to build the install URL.
	Slug string
	// PrivateKeyPEM is sealed on store; its presence is what makes an app
	// INSTALLABLE rather than merely OAuth-authorizable.
	PrivateKeyPEM string
	// OwnerLogin is the account the App belongs to — the discriminator once a
	// tenant holds one App per org.
	OwnerLogin string
}

// the latter pass the client_id/client_secret they got back from the forge.
// Returns the stored app with SealedSecret nilled, ready to serialise.
func (s *Server) createForgeOAuthApp(r *http.Request, teamID, userID string, provider forge.Provider, rawBaseURL, clientID, clientSecret, providerAppID string, autoCreated bool, mode string, gh githubAppFacts) (forge.ForgeOAuthApp, error) {
	appManageURL, appSlug, privateKeyPEM := gh.ManageURL, gh.Slug, gh.PrivateKeyPEM
	baseURL := forge.CanonicalBaseURL(provider, rawBaseURL)
	appID := uuid.NewString()
	sealed, err := forge.SealOAuthAppSecret(s.sealer, appID, clientSecret)
	if err != nil {
		return forge.ForgeOAuthApp{}, fmt.Errorf("seal secret: %w", err)
	}
	// A manifest-created GitHub App also carries a private key — seal it (bound
	// to this record id) so the least-privilege github_app install path can mint
	// installation tokens. Other connect modes pass "".
	var sealedKey []byte
	if privateKeyPEM != "" {
		sealedKey, err = forge.SealForgeAppPrivateKey(s.sealer, appID, privateKeyPEM)
		if err != nil {
			return forge.ForgeOAuthApp{}, fmt.Errorf("seal app key: %w", err)
		}
	}
	now := time.Now().UTC()
	app := forge.ForgeOAuthApp{
		ID: appID, TenantID: teamID, Provider: provider, ForgeBaseURL: baseURL,
		ClientID: clientID, SealedSecret: sealed, RedirectURI: s.forgeOAuthRedirectURI(),
		ProviderAppID: providerAppID, AutoCreated: autoCreated, AppManageURL: appManageURL,
		AppSlug: appSlug, SealedPrivateKey: sealedKey, OwnerLogin: gh.OwnerLogin,
		CreatedBy: userID, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.forgeOAuthApps.Create(store.WithTenant(r.Context(), teamID), app); err != nil {
		return forge.ForgeOAuthApp{}, err
	}
	s.auditTenant(r, teamID, "forge.oauth_app.created", "forge_oauth_app", appID, map[string]any{
		"provider": provider, "mode": mode, "instance": baseURL, "auto_created": autoCreated,
	})
	app.SealedSecret = nil
	return app, nil
}

// writeForgeOAuthAppError maps store / provider errors to HTTP responses,
// including the auto-create scope errors used in a later step.
func (s *Server) writeForgeOAuthAppError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, forge.ErrOAuthAppExists):
		httpError(w, http.StatusConflict, "%v", err)
	case errors.Is(err, forge.ErrForbidden):
		writeJSONStatus(w, http.StatusForbidden, map[string]any{
			"error":  "insufficient_scope",
			"detail": "the token can't create an OAuth app on this instance — GitLab needs an instance-admin token; or paste an existing client_id/client_secret instead",
		})
	case errors.Is(err, forge.ErrUnauthorized):
		httpError(w, http.StatusBadRequest, "the token was rejected by the forge")
	default:
		httpError(w, http.StatusInternalServerError, "%v", err)
	}
}

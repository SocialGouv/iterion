package server

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/auth/oidc"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/forge"
	forgeforgejo "github.com/SocialGouv/iterion/pkg/forge/forgejo"
	forgegithub "github.com/SocialGouv/iterion/pkg/forge/github"
	forgegitlab "github.com/SocialGouv/iterion/pkg/forge/gitlab"
	"github.com/SocialGouv/iterion/pkg/internal/strutil"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ForgeGitHubAppConfig is the global GitHub-App identity (registered once on
// GitHub), used for the installation-token connect mode. Empty/unconfigured
// → the GitHub-App connect path is unavailable (OAuth/PAT still work).
type ForgeGitHubAppConfig struct {
	AppID      int64
	PrivateKey string // PEM
	AppSlug    string // for the install URL github.com/apps/<slug>/installations/new
}

func (c ForgeGitHubAppConfig) Configured() bool {
	return c.AppID != 0 && strings.TrimSpace(c.PrivateKey) != ""
}

func (s *Server) githubAppConfig() forgegithub.AppConfig {
	return forgegithub.AppConfig{AppID: s.forgeGitHubApp.AppID, PrivateKeyPEM: s.forgeGitHubApp.PrivateKey, AppSlug: s.forgeGitHubApp.AppSlug}
}

// forgeAgentBindingCookie is the per-flow CSRF-binding cookie for the
// forge OAuth connect flow (the analogue of oidcAgentBindingCookie).
const forgeAgentBindingCookie = "iterion_forge_agent"

// forgePending is the server-side state held between the forge OAuth
// /connect and /callback. Unlike oidc.PendingAuth it carries the tenant +
// forge base URL, because the callback (a public IdP redirect) resolves
// the team from the signed state, not from a path or JWT.
type forgePending struct {
	State        string
	CodeVerifier string
	Provider     forge.Provider
	ForgeBaseURL string
	TenantID     string
	UserID       string
	AgentBinding string
	NextURL      string
	IssuedAt     time.Time
}

// forgeStateBackend stores forgePending CSRF state keyed by State, with a
// one-time `take`. The in-memory impl is single-replica; the Valkey impl
// (forge_state_valkey.go) shares it across replicas so the OAuth/manifest
// /start and /callback can land on different pods.
type forgeStateBackend interface {
	put(p forgePending)
	take(state string) (forgePending, bool)
}

// forgeStateStore is the TTL-bounded in-memory backend, mirroring
// oidc.MemoryStateStore. Used in local/desktop and single-replica deployments.
type forgeStateStore struct {
	mu  sync.Mutex
	m   map[string]forgePending
	ttl time.Duration
}

func newForgeStateStore(ttl time.Duration) *forgeStateStore {
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	return &forgeStateStore{m: make(map[string]forgePending), ttl: ttl}
}

func (s *forgeStateStore) put(p forgePending) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[p.State] = p
}

func (s *forgeStateStore) take(state string) (forgePending, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.m[state]
	if !ok {
		return forgePending{}, false
	}
	delete(s.m, state)
	if time.Since(p.IssuedAt) > s.ttl {
		return forgePending{}, false
	}
	return p, true
}

func (s *Server) registerForgeRoutes() {
	s.mux.Handle("GET /api/teams/{id}/forge/connections", s.requireAuth(http.HandlerFunc(s.handleListForgeConnections)))
	s.mux.Handle("POST /api/teams/{id}/forge/connections", s.requireAuth(http.HandlerFunc(s.handleConnectForge)))
	s.mux.Handle("DELETE /api/teams/{id}/forge/connections/{conn_id}", s.requireAuth(http.HandlerFunc(s.handleDeleteForgeConnection)))
	s.mux.Handle("GET /api/teams/{id}/forge/connections/{conn_id}/repos", s.requireAuth(http.HandlerFunc(s.handleListForgeRepos)))
	// Public IdP redirect targets (see isPublicPath); authenticate via the
	// signed state + the agent-binding cookie.
	s.mux.HandleFunc("GET /api/forge/oauth/callback", s.handleForgeOAuthCallback)
	s.mux.HandleFunc("GET /api/forge/github/app/callback", s.handleForgeGitHubAppCallback)
}

// ---- factories (provider dispatch) ----

// forgeBotForge resolves a bot's manifest forge: block for the orchestrator.
func (s *Server) forgeBotForge(botID string) (*bundle.ForgeRequirements, error) {
	entry, ok, err := s.findBot(botID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("bot %q not found", botID)
	}
	return entry.Forge, nil
}

// forgeBotInvocations resolves a bot's manifest invocations for the
// orchestrator's command-map build (already the EffectiveInvocations set —
// explicit block, else synthetic from a legacy forge: block).
func (s *Server) forgeBotInvocations(botID string) ([]bundle.Invocation, error) {
	entry, ok, err := s.findBot(botID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("bot %q not found", botID)
	}
	return entry.Invocations, nil
}

// forgeAdminForToken builds an outbound admin client from a raw token
// (used at connect time before a Connection exists).
func (s *Server) forgeAdminForToken(provider forge.Provider, baseURL, token string) (forge.Admin, error) {
	switch provider {
	case forge.ProviderGitLab:
		return forgegitlab.New(s.httpClient, baseURL, token), nil
	case forge.ProviderGitHub:
		return forgegithub.New(s.httpClient, baseURL, token), nil
	case forge.ProviderForgejo:
		return forgeforgejo.New(s.httpClient, baseURL, token), nil
	default:
		return nil, fmt.Errorf("forge: provider %q is not yet supported", provider)
	}
}

// forgeAdminFor builds a connection's admin client (the orchestrator's
// AdminFor). A GitHub-App connection mints a fresh installation token from
// the App private key on demand; every other kind opens its sealed token.
func (s *Server) forgeAdminFor(ctx context.Context, conn forge.Connection) (forge.Admin, error) {
	if conn.Kind == forge.KindGitHubApp {
		cfg, ok := s.githubAppConfigForTenant(ctx, conn.TenantID)
		if !ok {
			return nil, fmt.Errorf("forge: no github app available for this connection")
		}
		return &forgegithub.AppClient{
			HTTP: s.httpClient, WebBaseURL: conn.BaseURL(),
			Cfg: cfg, InstallationID: conn.InstallationID,
		}, nil
	}
	token, err := forge.AdminTokenFor(s.sealer, conn)
	if err != nil {
		return nil, err
	}
	return s.forgeAdminForToken(conn.Provider, conn.BaseURL(), token)
}

// githubAppConfigForTenant returns the GitHub-App identity (app id + private key
// + slug) used to mint installation tokens for the least-privilege github_app
// path. It prefers the tenant's own manifest-created App (its sealed private
// key), falling back to the platform App (ITERION_FORGE_GITHUB_APP_*). ok is
// false when neither is available.
func (s *Server) githubAppConfigForTenant(ctx context.Context, tenantID string) (forgegithub.AppConfig, bool) {
	if s.forgeOAuthApps != nil {
		base := forge.CanonicalBaseURL(forge.ProviderGitHub, "")
		if app, err := s.forgeOAuthApps.GetByInstance(ctx, tenantID, forge.ProviderGitHub, base); err == nil && len(app.SealedPrivateKey) > 0 {
			if pem, err := forge.OpenForgeAppPrivateKey(s.sealer, app.ID, app.SealedPrivateKey); err == nil && pem != "" {
				if appID, _ := strconv.ParseInt(app.ProviderAppID, 10, 64); appID != 0 {
					return forgegithub.AppConfig{AppID: appID, PrivateKeyPEM: pem, AppSlug: app.AppSlug}, true
				}
			}
		}
	}
	if s.forgeGitHubApp.Configured() {
		return s.githubAppConfig(), true
	}
	return forgegithub.AppConfig{}, false
}

// forgeOAuthAppFor builds a provider's OAuth client for a (tenant, provider,
// instance) from the per-tenant OAuth-app store, or (nil,false) when no app is
// registered for that instance or its sealed secret can't be opened. baseURL
// is canonicalised so a connection's BaseURL() and a stored app key match.
func (s *Server) forgeOAuthAppFor(ctx context.Context, tenantID string, provider forge.Provider, baseURL string) (forge.OAuthExchanger, bool) {
	if s.forgeOAuthApps == nil {
		return nil, false
	}
	base := forge.CanonicalBaseURL(provider, baseURL)
	app, err := s.forgeOAuthApps.GetByInstance(ctx, tenantID, provider, base)
	if err != nil {
		return nil, false
	}
	secret, err := forge.OpenOAuthAppSecret(s.sealer, app.ID, app.SealedSecret)
	if err != nil {
		return nil, false
	}
	switch provider {
	case forge.ProviderGitLab:
		return &forgegitlab.OAuthApp{HTTP: s.httpClient, BaseURL: base, ClientID: app.ClientID, ClientSecret: secret}, true
	case forge.ProviderGitHub:
		return &forgegithub.OAuthApp{HTTP: s.httpClient, BaseURL: base, ClientID: app.ClientID, ClientSecret: secret}, true
	case forge.ProviderForgejo:
		return &forgeforgejo.OAuthApp{HTTP: s.httpClient, BaseURL: base, ClientID: app.ClientID, ClientSecret: secret}, true
	default:
		return nil, false
	}
}

func (s *Server) forgeOAuthRedirectURI() string {
	return strings.TrimRight(s.cfg.PublicURL, "/") + "/api/forge/oauth/callback"
}

// forgeOAuthAppProvisioner builds the create-app client for a provider from an
// admin token. The per-provider AdminClient implements OAuthAppProvisioner
// where the forge exposes a create-app API (GitLab, Forgejo); GitHub does not
// (its create path is the interactive App-Manifest flow), so it returns a clear
// "paste an existing app instead" error here.
func (s *Server) forgeOAuthAppProvisioner(provider forge.Provider, baseURL, adminToken string) (forge.OAuthAppProvisioner, error) {
	admin, err := s.forgeAdminForToken(provider, baseURL, adminToken)
	if err != nil {
		return nil, err
	}
	prov, ok := admin.(forge.OAuthAppProvisioner)
	if !ok {
		return nil, fmt.Errorf("auto-create is not available for %s — paste an existing client_id/client_secret instead", provider)
	}
	return prov, nil
}

// forgeDefaultOAuthScopes is the scope set an auto-created OAuth app requests,
// per provider (the same defaults the connect flow uses at authorize time).
func forgeDefaultOAuthScopes(p forge.Provider) []string {
	switch p {
	case forge.ProviderGitLab:
		return forgegitlab.DefaultScopes
	case forge.ProviderGitHub:
		return forgegithub.DefaultScopes
	case forge.ProviderForgejo:
		return forgeforgejo.DefaultScopes
	default:
		return nil
	}
}

// forgeRefresherFor returns the token refresher for a connection, or nil
// when it cannot/should-not refresh (PAT, GitHub-App, or a provider with no
// configured OAuth app). The per-provider OAuth clients implement both
// OAuthExchanger and TokenRefresher.
func (s *Server) forgeRefresherFor(conn forge.Connection) forge.TokenRefresher {
	if conn.Kind == forge.KindGitHubApp {
		cfg, ok := s.githubAppConfigForTenant(context.Background(), conn.TenantID)
		if !ok {
			return nil
		}
		return forgegithub.AppRefresher{HTTP: s.httpClient, Cfg: cfg}
	}
	if conn.Kind != forge.KindOAuthApp {
		return nil
	}
	app, ok := s.forgeOAuthAppFor(context.Background(), conn.TenantID, conn.Provider, conn.BaseURL())
	if !ok {
		return nil
	}
	r, _ := app.(forge.TokenRefresher)
	return r
}

// ---- handlers ----

func (s *Server) handleListForgeConnections(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canViewTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "not a member")
		return
	}
	list, err := s.forgeConnections.ListByTenant(r.Context(), teamID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	if list == nil {
		list = []forge.Connection{}
	}
	writeJSON(w, struct {
		Connections []forge.Connection `json:"connections"`
	}{Connections: list})
}

type forgeConnectReq struct {
	Provider     string `json:"provider"`
	Mode         string `json:"mode"` // "oauth" | "pat"
	ForgeBaseURL string `json:"forge_base_url,omitempty"`
	PAT          string `json:"pat,omitempty"`
	DisplayName  string `json:"display_name,omitempty"`
	Next         string `json:"next,omitempty"`
}

type forgeConnectResp struct {
	Connection   *forge.Connection `json:"connection,omitempty"`
	AuthorizeURL string            `json:"authorize_url,omitempty"`
	InstallURL   string            `json:"install_url,omitempty"`
}

func (s *Server) handleConnectForge(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canManageTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "admin or owner required")
		return
	}
	var req forgeConnectReq
	if !decodeJSON(w, r, &req) {
		return
	}
	provider := forge.Provider(strings.TrimSpace(req.Provider))
	if !provider.Valid() {
		httpError(w, http.StatusBadRequest, "unsupported provider %q (gitlab|github|forgejo)", req.Provider)
		return
	}
	baseURL := canonicalForgeBaseURL(req.ForgeBaseURL, provider)

	switch strings.ToLower(strings.TrimSpace(req.Mode)) {
	case "pat":
		s.connectForgePAT(w, r, teamID, id.UserID, provider, baseURL, req)
	case "oauth", "":
		s.connectForgeOAuth(w, r, teamID, id.UserID, provider, baseURL, req)
	case "app":
		s.connectForgeGitHubApp(w, r, teamID, id.UserID, provider, req)
	default:
		httpError(w, http.StatusBadRequest, "mode must be oauth, pat, or app")
	}
}

// connectForgeGitHubApp starts the GitHub-App install flow: it returns the
// App's install URL carrying a signed state, and stashes the pending tenant
// binding so the install callback can resolve the team.
func (s *Server) connectForgeGitHubApp(w http.ResponseWriter, r *http.Request, teamID, userID string, provider forge.Provider, req forgeConnectReq) {
	if provider != forge.ProviderGitHub {
		httpError(w, http.StatusBadRequest, "the app mode is GitHub-only")
		return
	}
	cfg, ok := s.githubAppConfigForTenant(r.Context(), teamID)
	if !ok {
		httpError(w, http.StatusBadRequest, "no GitHub App available — first create one (Register an OAuth app → Create a GitHub App), or use OAuth/PAT")
		return
	}
	state, _, _, err := oidc.GenerateStateAndPKCE()
	if err != nil {
		httpError(w, http.StatusInternalServerError, "internal error")
		return
	}
	binding, err := newAgentBindingToken()
	if err != nil {
		httpError(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.forgeStates.put(forgePending{
		State: state, Provider: forge.ProviderGitHub, ForgeBaseURL: forge.DefaultBaseURL(forge.ProviderGitHub),
		TenantID: teamID, UserID: userID, AgentBinding: binding,
		NextURL: safeNext(req.Next), IssuedAt: time.Now().UTC(),
	})
	s.setForgeAgentBindingCookie(w, binding)
	installURL := "https://github.com/apps/" + url.PathEscape(cfg.AppSlug) + "/installations/new?state=" + url.QueryEscape(state)
	writeJSON(w, forgeConnectResp{InstallURL: installURL})
}

func (s *Server) connectForgePAT(w http.ResponseWriter, r *http.Request, teamID, userID string, provider forge.Provider, baseURL string, req forgeConnectReq) {
	token := strings.TrimSpace(req.PAT)
	if token == "" {
		httpError(w, http.StatusBadRequest, "pat required for mode=pat")
		return
	}
	admin, err := s.forgeAdminForToken(provider, baseURL, token)
	if err != nil {
		httpError(w, http.StatusBadRequest, "%v", err)
		return
	}
	ident, err := admin.WhoAmI(r.Context())
	if err != nil {
		if errors.Is(err, forge.ErrUnauthorized) {
			httpError(w, http.StatusBadRequest, "the token was rejected by %s — check it has api scope", provider)
			return
		}
		httpError(w, http.StatusBadGateway, "could not reach %s: %v", provider, err)
		return
	}
	connID := uuid.NewString()
	sealed, err := forge.SealPAT(s.sealer, connID, token)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "seal token: %v", err)
		return
	}
	now := time.Now().UTC()
	conn := forge.Connection{
		ID: connID, TenantID: teamID, Provider: provider, Kind: forge.KindPAT,
		DisplayName: strutil.FirstNonBlank(req.DisplayName, ident.Login), ForgeBaseURL: baseURL,
		AccountLogin: ident.Login, AccountID: ident.ID, Namespace: ident.Namespace,
		Status: forge.StatusActive, SealedPayload: sealed,
		CreatedBy: userID, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.forgeConnections.Create(store.WithTenant(r.Context(), teamID), conn); err != nil {
		httpError(w, http.StatusInternalServerError, "persist connection: %v", err)
		return
	}
	s.auditTenant(r, teamID, "forge.connection.created", "forge_connection", connID, map[string]any{"provider": provider, "kind": "pat"})
	conn.SealedPayload = nil // never serialise
	writeJSON(w, forgeConnectResp{Connection: &conn})
}

func (s *Server) connectForgeOAuth(w http.ResponseWriter, r *http.Request, teamID, userID string, provider forge.Provider, baseURL string, req forgeConnectReq) {
	app, ok := s.forgeOAuthAppFor(r.Context(), teamID, provider, baseURL)
	if !ok {
		httpError(w, http.StatusBadRequest, "no OAuth app is registered for %s on this instance — register one in Integrations → OAuth apps, or paste a personal access token (mode=pat)", provider)
		return
	}
	state, verifier, challenge, err := oidc.GenerateStateAndPKCE()
	if err != nil {
		httpError(w, http.StatusInternalServerError, "internal error")
		return
	}
	binding, err := newAgentBindingToken()
	if err != nil {
		httpError(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.forgeStates.put(forgePending{
		State: state, CodeVerifier: verifier, Provider: provider, ForgeBaseURL: baseURL,
		TenantID: teamID, UserID: userID, AgentBinding: binding,
		NextURL: safeNext(req.Next), IssuedAt: time.Now().UTC(),
	})
	s.setForgeAgentBindingCookie(w, binding)
	authURL := app.AuthorizeURL(s.forgeOAuthRedirectURI(), state, challenge, nil)
	writeJSON(w, forgeConnectResp{AuthorizeURL: authURL})
}

func (s *Server) handleForgeOAuthCallback(w http.ResponseWriter, r *http.Request) {
	if s.forgeStates == nil {
		httpError(w, http.StatusNotFound, "forge integrations disabled")
		return
	}
	if oauthErr := r.URL.Query().Get("error"); oauthErr != "" {
		if s.logger != nil {
			s.logger.Warn("forge oauth callback error: %s", oauthErr)
		}
		httpError(w, http.StatusBadRequest, "oauth error: %s", oauthErr)
		return
	}
	state, code := r.URL.Query().Get("state"), r.URL.Query().Get("code")
	if state == "" || code == "" {
		httpError(w, http.StatusBadRequest, "missing state or code")
		return
	}
	pending, ok := s.forgeStates.take(state)
	clearForgeAgentBindingCookie(w, s.cfg.CookieDomain, s.cfg.CookieSecure)
	if !ok {
		httpError(w, http.StatusBadRequest, "state expired or invalid")
		return
	}
	if pending.AgentBinding != "" {
		c, cerr := r.Cookie(forgeAgentBindingCookie)
		if cerr != nil || subtle.ConstantTimeCompare([]byte(c.Value), []byte(pending.AgentBinding)) != 1 {
			httpError(w, http.StatusBadRequest, "agent binding mismatch")
			return
		}
	}
	app, ok := s.forgeOAuthAppFor(r.Context(), pending.TenantID, pending.Provider, pending.ForgeBaseURL)
	if !ok {
		httpError(w, http.StatusBadRequest, "oauth app no longer registered for %s", pending.Provider)
		return
	}
	tok, err := app.Exchange(r.Context(), code, s.forgeOAuthRedirectURI(), pending.CodeVerifier)
	if err != nil {
		httpError(w, http.StatusBadRequest, "token exchange failed: %v", err)
		return
	}
	admin, err := s.forgeAdminForToken(pending.Provider, pending.ForgeBaseURL, tok.AccessToken)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%v", err)
		return
	}
	ident, err := admin.WhoAmI(r.Context())
	if err != nil {
		httpError(w, http.StatusBadGateway, "could not read identity from %s: %v", pending.Provider, err)
		return
	}
	connID := uuid.NewString()
	sealed, err := forge.SealOAuthTokens(s.sealer, connID, tok.AccessToken, tok.RefreshToken, tok.ExpiresAt)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "seal token: %v", err)
		return
	}
	now := time.Now().UTC()
	conn := forge.Connection{
		ID: connID, TenantID: pending.TenantID, Provider: pending.Provider, Kind: forge.KindOAuthApp,
		DisplayName: ident.Login, ForgeBaseURL: pending.ForgeBaseURL,
		AccountLogin: ident.Login, AccountID: ident.ID, Namespace: ident.Namespace,
		Status: forge.StatusActive, SealedPayload: sealed, Scopes: tok.Scopes,
		CreatedBy: pending.UserID, CreatedAt: now, UpdatedAt: now,
	}
	if !tok.ExpiresAt.IsZero() {
		exp := tok.ExpiresAt
		conn.AccessTokenExpiresAt = &exp
	}
	if err := s.forgeConnections.Create(store.WithTenant(r.Context(), pending.TenantID), conn); err != nil {
		httpError(w, http.StatusInternalServerError, "persist connection: %v", err)
		return
	}
	s.auditTenant(r, pending.TenantID, "forge.connection.created", "forge_connection", connID, map[string]any{"provider": pending.Provider, "kind": "oauth_app"})
	target := pending.NextURL
	if target == "" {
		target = "/teams/" + pending.TenantID
	}
	http.Redirect(w, r, target, http.StatusFound)
}

// handleForgeGitHubAppCallback is the GitHub-App "Setup URL" target after an
// operator installs the App. GitHub appends installation_id + state; we
// resolve the team from the signed state (not the URL), mint an initial
// installation token, seal it, and persist a github_app connection.
func (s *Server) handleForgeGitHubAppCallback(w http.ResponseWriter, r *http.Request) {
	if s.forgeStates == nil {
		httpError(w, http.StatusNotFound, "github app not enabled")
		return
	}
	state := r.URL.Query().Get("state")
	instStr := r.URL.Query().Get("installation_id")
	if state == "" || instStr == "" {
		httpError(w, http.StatusBadRequest, "missing state or installation_id")
		return
	}
	pending, ok := s.forgeStates.take(state)
	clearForgeAgentBindingCookie(w, s.cfg.CookieDomain, s.cfg.CookieSecure)
	if !ok {
		httpError(w, http.StatusBadRequest, "state expired or invalid")
		return
	}
	if pending.AgentBinding != "" {
		c, cerr := r.Cookie(forgeAgentBindingCookie)
		if cerr != nil || subtle.ConstantTimeCompare([]byte(c.Value), []byte(pending.AgentBinding)) != 1 {
			httpError(w, http.StatusBadRequest, "agent binding mismatch")
			return
		}
	}
	installationID, err := strconv.ParseInt(instStr, 10, 64)
	if err != nil {
		httpError(w, http.StatusBadRequest, "invalid installation_id")
		return
	}
	cfg, ok := s.githubAppConfigForTenant(r.Context(), pending.TenantID)
	if !ok {
		httpError(w, http.StatusBadRequest, "no github app available for this org")
		return
	}
	base := forge.DefaultBaseURL(forge.ProviderGitHub)
	now := time.Now().UTC()
	tok, exp, err := forgegithub.MintInstallationToken(r.Context(), s.httpClient, forgegithub.APIBaseFor(base), cfg, installationID, now)
	if err != nil {
		httpError(w, http.StatusBadGateway, "could not mint installation token: %v", err)
		return
	}
	connID := uuid.NewString()
	// Seal the installation token like an OAuth access token (no refresh
	// token — the refresh worker re-mints from the App private key).
	sealed, err := forge.SealOAuthTokens(s.sealer, connID, tok, "", exp)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "seal token: %v", err)
		return
	}
	conn := forge.Connection{
		ID: connID, TenantID: pending.TenantID, Provider: forge.ProviderGitHub, Kind: forge.KindGitHubApp,
		DisplayName: cfg.AppSlug, ForgeBaseURL: base,
		AccountLogin: cfg.AppSlug + "[bot]", Namespace: cfg.AppSlug,
		InstallationID: installationID, AppSlug: cfg.AppSlug,
		Status: forge.StatusActive, SealedPayload: sealed, AccessTokenExpiresAt: &exp,
		CreatedBy: pending.UserID, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.forgeConnections.Create(store.WithTenant(r.Context(), pending.TenantID), conn); err != nil {
		httpError(w, http.StatusInternalServerError, "persist connection: %v", err)
		return
	}
	s.auditTenant(r, pending.TenantID, "forge.connection.created", "forge_connection", connID, map[string]any{"provider": "github", "kind": "github_app"})
	target := pending.NextURL
	if target == "" {
		target = "/teams/" + pending.TenantID
	}
	http.Redirect(w, r, target, http.StatusFound)
}

func (s *Server) handleDeleteForgeConnection(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canManageTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "admin or owner required")
		return
	}
	connID := r.PathValue("conn_id")
	ctx := store.WithTenant(r.Context(), teamID)
	if err := s.forgeOrchestrator.DeprovisionConnection(ctx, teamID, connID); err != nil {
		if errors.Is(err, forge.ErrConnectionNotFound) {
			httpError(w, http.StatusNotFound, "connection not found")
			return
		}
		httpError(w, http.StatusInternalServerError, "disconnect failed: %v", err)
		return
	}
	s.auditTenant(r, teamID, "forge.connection.deleted", "forge_connection", connID, nil)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListForgeRepos(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canManageTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "admin or owner required")
		return
	}
	conn, ok := s.forgeConnForTenant(w, r, teamID, r.PathValue("conn_id"))
	if !ok {
		return
	}
	admin, err := s.forgeAdminFor(r.Context(), conn)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%v", err)
		return
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	repos, err := admin.ListRepos(r.Context(), forge.RepoQuery{
		Search: r.URL.Query().Get("search"),
		Page:   page,
	})
	if err != nil {
		if errors.Is(err, forge.ErrUnauthorized) {
			httpError(w, http.StatusBadRequest, "connection credential rejected — reconnect")
			return
		}
		httpError(w, http.StatusBadGateway, "list repos: %v", err)
		return
	}
	if repos == nil {
		repos = []forge.RepoSummary{}
	}
	writeJSON(w, struct {
		Repos []forge.RepoSummary `json:"repos"`
	}{Repos: repos})
}

// forgeConnForTenant fetches a connection and asserts tenant ownership.
func (s *Server) forgeConnForTenant(w http.ResponseWriter, r *http.Request, teamID, connID string) (forge.Connection, bool) {
	conn, err := s.forgeConnections.Get(r.Context(), connID)
	if err != nil || conn.TenantID != teamID {
		httpError(w, http.StatusNotFound, "connection not found")
		return forge.Connection{}, false
	}
	return conn, true
}

// ---- helpers ----

// setForgeAgentBindingCookie issues the per-flow CSRF-binding cookie for a
// forge connect flow (the OAuth + GitHub-App callbacks verify it; the PAT
// path has no redirect and skips it). Mirrors clearForgeAgentBindingCookie.
func (s *Server) setForgeAgentBindingCookie(w http.ResponseWriter, binding string) {
	http.SetCookie(w, &http.Cookie{
		Name:     forgeAgentBindingCookie,
		Value:    binding,
		Path:     "/api/forge/",
		Domain:   s.cfg.CookieDomain,
		HttpOnly: true,
		Secure:   s.cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int((10 * time.Minute).Seconds()),
	})
}

func clearForgeAgentBindingCookie(w http.ResponseWriter, domain string, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     forgeAgentBindingCookie,
		Value:    "",
		Path:     "/api/forge/",
		Domain:   domain,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// canonicalForgeBaseURL normalises an operator-supplied forge base URL to
// scheme+host (https assumed when no scheme), or returns the provider's
// canonical SaaS host when empty.
func canonicalForgeBaseURL(raw string, provider forge.Provider) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return forge.DefaultBaseURL(provider)
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	s = strings.TrimRight(s, "/")
	return s
}

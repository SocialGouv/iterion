package server

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/auth/oidc"
	"github.com/SocialGouv/iterion/pkg/forge"
	forgegithub "github.com/SocialGouv/iterion/pkg/forge/github"
	"github.com/SocialGouv/iterion/pkg/internal/strutil"
	"github.com/SocialGouv/iterion/pkg/store"
)

type forgeConnectReq struct {
	Provider     string `json:"provider"`
	Mode         string `json:"mode"` // "oauth" | "pat"
	ForgeBaseURL string `json:"forge_base_url,omitempty"`
	PAT          string `json:"pat,omitempty"`
	DisplayName  string `json:"display_name,omitempty"`
	Next         string `json:"next,omitempty"`
	// OAuthAppID selects WHICH GitHub App to install when the team holds
	// several (one per owning org). Empty keeps the legacy behaviour: the
	// team's single app for that host.
	OAuthAppID string `json:"oauth_app_id,omitempty"`
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
	cfg, appID, _, ok := s.githubAppForInstall(r.Context(), teamID, req.OAuthAppID)
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
	if err := s.forgeStates.put(forgePending{
		State: state, Provider: forge.ProviderGitHub, ForgeBaseURL: forge.DefaultBaseURL(forge.ProviderGitHub),
		TenantID: teamID, UserID: userID, AgentBinding: binding,
		NextURL: safeNext(req.Next), OAuthAppID: appID, IssuedAt: time.Now().UTC(),
	}); err != nil {
		if s.logger != nil {
			s.logger.Error("forge connect: %v", err)
		}
		httpError(w, http.StatusBadGateway, "could not persist OAuth state — try again")
		return
	}
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
	if err := s.forgeStates.put(forgePending{
		State: state, CodeVerifier: verifier, Provider: provider, ForgeBaseURL: baseURL,
		TenantID: teamID, UserID: userID, AgentBinding: binding,
		NextURL: safeNext(req.Next), IssuedAt: time.Now().UTC(),
	}); err != nil {
		if s.logger != nil {
			s.logger.Error("forge connect: %v", err)
		}
		httpError(w, http.StatusBadGateway, "could not persist OAuth state — try again")
		return
	}
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
	http.Redirect(w, r, appendQueryParam(target, "connected", connID), http.StatusFound)
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
	// GitHub redirects here with setup_action=update (and NO state) when an
	// org owner edits the installation's repo list from GitHub's own
	// settings page — outside iterion's install flow. There is nothing to
	// persist (the live scope is re-probed on demand via InstallationInfo),
	// so send the operator back to Integrations instead of a bare 400.
	if r.URL.Query().Get("setup_action") == "update" && state == "" {
		http.Redirect(w, r, "/integrations", http.StatusFound)
		return
	}
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
	// Resolve the app the flow was STARTED with (pinned in the signed state),
	// not "the team's app for this host" — with several apps per host the
	// latter can pick the wrong private key, which cannot mint for this
	// installation at all.
	cfg, appRecordID, shared, ok := s.githubAppForInstall(r.Context(), pending.TenantID, pending.OAuthAppID)
	if !ok {
		httpError(w, http.StatusBadRequest, "no github app available for this org")
		return
	}
	base := forge.DefaultBaseURL(forge.ProviderGitHub)

	// Installation-ownership check (IDOR guard). installation_id is an
	// enumerable integer taken verbatim from the callback URL; the signed
	// state only proves the flow was started by this team, NOT that the team
	// owns this installation. The SHARED platform App's key can mint a token
	// for ANY installation, so without a check an attacker could substitute a
	// victim org's installation_id and capture its repos. When user-auth OAuth
	// creds are configured we verify the completing user actually has access to
	// the installation (mandatory — an attacker can't bypass by dropping the
	// code, because a missing/invalid code fails closed). Per-tenant Apps
	// (shared==false) are key-scoped and can't reach another tenant's
	// installation, so they skip this. See VerifyInstallationOwnership.
	if shared {
		if cfg.UserAuthConfigured() {
			code := r.URL.Query().Get("code")
			if verr := forgegithub.VerifyInstallationOwnership(r.Context(), s.forgeHTTPClient(), base, cfg, code, installationID); verr != nil {
				if errors.Is(verr, forgegithub.ErrInstallationNotOwned) {
					httpError(w, http.StatusForbidden, "installation is not owned by the authorizing user")
					return
				}
				httpError(w, http.StatusBadGateway, "could not verify installation ownership: %v", verr)
				return
			}
		} else if s.logger != nil {
			// Opt-in hardening not configured: the shared-App install path
			// cannot verify ownership. Log loudly so operators enable it
			// (ITERION_FORGE_GITHUB_APP_CLIENT_ID/_CLIENT_SECRET + "Request
			// user authorization during installation" on the App).
			s.logger.Warn("forge: github-app install accepted WITHOUT installation-ownership verification (shared platform app, no user-auth client creds) — set ITERION_FORGE_GITHUB_APP_CLIENT_ID/_CLIENT_SECRET to close the installation_id IDOR")
		}
	}

	now := time.Now().UTC()
	// Least-privilege: the initial connect-time token carries only iterion's
	// minimal permission set (no repositories scope yet — none provisioned;
	// the refresh worker re-scopes to the provisioned repo set thereafter).
	tok, exp, err := forgegithub.MintInstallationToken(r.Context(), s.forgeHTTPClient(), forgegithub.APIBaseFor(base), cfg, installationID, now,
		&forgegithub.InstallationTokenOptions{Permissions: forgegithub.RuntimeInstallationPermissions()})
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
	// Capture what the owner actually APPROVED at install. The mint reads it
	// to ask only for permissions that exist (asking for a missing one fails
	// the whole call), so recording it here is what lets an App created with
	// delivery permissions and one created without share a single code path.
	// Best-effort: an unknown grant set falls back to the historical baseline.
	var granted map[string]string
	if inst, ierr := forgegithub.InstallationInfo(r.Context(), s.forgeHTTPClient(),
		forgegithub.APIBaseFor(base), cfg, installationID, time.Now().UTC()); ierr == nil {
		granted = inst.Permissions
	} else if s.logger != nil {
		s.logger.Warn("forge: read installation %d permissions at connect: %v", installationID, ierr)
	}
	conn := forge.Connection{
		ID: connID, TenantID: pending.TenantID, Provider: forge.ProviderGitHub, Kind: forge.KindGitHubApp,
		DisplayName: cfg.AppSlug, ForgeBaseURL: base,
		AccountLogin: cfg.AppSlug + "[bot]", Namespace: cfg.AppSlug,
		InstallationID: installationID, AppSlug: cfg.AppSlug, OAuthAppID: appRecordID,
		GrantedPermissions: granted,
		Status:             forge.StatusActive, SealedPayload: sealed, AccessTokenExpiresAt: &exp,
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
	http.Redirect(w, r, appendQueryParam(target, "connected", connID), http.StatusFound)
}

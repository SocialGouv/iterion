package server

import (
	"crypto/subtle"
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
)

// registerForgeGitHubManifestRoutes wires the GitHub App-Manifest auto-create
// flow: GitHub has no create-OAuth-app REST endpoint, so iterion hands the SPA
// a pre-filled manifest to POST to GitHub; GitHub redirects back to the public
// callback with a temporary code that converts to the App's credentials.
func (s *Server) registerForgeGitHubManifestRoutes() {
	s.mux.Handle("POST /api/teams/{id}/forge/oauth-apps/github-manifest", s.requireAuth(http.HandlerFunc(s.handleStartGitHubManifest)))
	// Public callback (see isPublicPath); authenticated by the signed state +
	// the agent-binding cookie, like the OAuth callback.
	s.mux.HandleFunc("GET /api/forge/github/app-manifest/callback", s.handleGitHubManifestCallback)
}

// handleStartGitHubManifest returns the manifest JSON + the github.com POST
// target + a signed state; the SPA auto-submits a form to that target.
func (s *Server) handleStartGitHubManifest(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canManageTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "admin or owner required")
		return
	}
	var req forgeOAuthAppReq
	_ = readJSON(r, &req) // body optional: forge_base_url (GHE) + next
	baseURL := forge.CanonicalBaseURL(forge.ProviderGitHub, req.ForgeBaseURL)

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
		State: state, Provider: forge.ProviderGitHub, ForgeBaseURL: baseURL,
		TenantID: teamID, UserID: id.UserID, AgentBinding: binding,
		NextURL: safeNext(req.Next), SecurityReadOnly: req.SecurityReadOnly,
		IssuedAt: time.Now().UTC(),
	}); err != nil {
		if s.logger != nil {
			s.logger.Error("forge connect: %v", err)
		}
		httpError(w, http.StatusBadGateway, "could not persist OAuth state — try again")
		return
	}
	s.setForgeAgentBindingCookie(w, binding)

	home := strings.TrimRight(s.cfg.PublicURL, "/")
	redirectURL := home + "/api/forge/github/app-manifest/callback"
	// GitHub App names are globally unique. The prefix also makes the shape
	// readable on the org's Apps list, where a watch-only App sits next to
	// write-capable ones and only its name distinguishes them at a glance.
	prefix := "iterion-forge-"
	if req.SecurityReadOnly {
		prefix = "iterion-watch-"
	}
	name := prefix + uuid.NewString()[:8]
	manifest := forgegithub.BuildAppManifest(name, home, redirectURL,
		forgegithub.AppManifestOptions{
			SecurityReadOnly:  req.SecurityReadOnly,
			AllowRepoCreation: req.AllowRepoCreation,
			AllowAppDelivery:  req.AllowAppDelivery,
			AllowSecurityRead: req.AllowSecurityRead,
		})
	// Create the App UNDER the chosen org so a (private) App is installable
	// org-wide; empty org = the user's personal account.
	target := strings.TrimRight(baseURL, "/")
	if org := strings.TrimSpace(req.GitHubOrg); org != "" {
		target += "/organizations/" + url.PathEscape(org)
	}
	postURL := target + "/settings/apps/new?state=" + url.QueryEscape(state)
	writeJSON(w, map[string]any{"post_url": postURL, "manifest": manifest, "state": state})
}

// handleGitHubManifestCallback receives GitHub's temporary code, converts it to
// the created App's credentials, and persists the OAuth app for the tenant.
func (s *Server) handleGitHubManifestCallback(w http.ResponseWriter, r *http.Request) {
	if s.forgeStates == nil || s.forgeOAuthApps == nil {
		httpError(w, http.StatusNotFound, "forge integrations disabled")
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
	conv, err := forgegithub.ConvertManifest(r.Context(), s.forgeHTTPClient(), pending.ForgeBaseURL, code)
	if err != nil {
		httpError(w, http.StatusBadGateway, "%v", err)
		return
	}
	// Deep link to the App's settings/advanced page so the operator can delete
	// it on GitHub when they remove the OAuth app here (GitHub has no app-delete API).
	manageURL := forgegithub.AppManageURL(pending.ForgeBaseURL, conv.Owner.Login, conv.Owner.Type, conv.Slug)
	// The manifest travels through the operator's browser, so the App that got
	// created is not necessarily the one iterion asked for. Refuse to record a
	// watch-only label the created App does not actually carry: that label is
	// what suppresses its missing-permission warnings and what convinces an org
	// owner to install it on every repository.
	if pending.SecurityReadOnly && !conv.IsSecurityReadOnly() {
		httpError(w, http.StatusBadGateway,
			"the GitHub App that was created carries %v, not the watch-only profile %v — it was NOT recorded; delete it at %s and retry",
			conv.Permissions, forgegithub.SecurityReadInstallationPermissions(), manageURL)
		return
	}
	// Capture the App slug + private key (conv.PEM) so the App can be INSTALLED
	// (least-privilege github_app), not only OAuth-authorized.
	app, err := s.createForgeOAuthApp(r, pending.TenantID, pending.UserID, forge.ProviderGitHub, pending.ForgeBaseURL, conv.ClientID, conv.ClientSecret, strconv.FormatInt(conv.ID, 10), true, "github_manifest",
		githubAppFacts{ManageURL: manageURL, Slug: conv.Slug, PrivateKeyPEM: conv.PEM, OwnerLogin: conv.Owner.Login,
			SecurityReadOnly: pending.SecurityReadOnly})
	if err != nil {
		// The App EXISTS on GitHub by now, and its private key was returned
		// once and is gone with this request. Name where to delete it, or the
		// operator retries and leaves one more orphan behind each time.
		httpError(w, http.StatusConflict,
			"the GitHub App %q was created but could not be recorded: %v — delete it at %s, then retry",
			conv.Slug, err, manageURL)
		return
	}
	target := pending.NextURL
	if target == "" {
		target = "/teams/" + pending.TenantID
	}
	http.Redirect(w, r, appendQueryParam(target, "installed", app.ID), http.StatusFound)
}

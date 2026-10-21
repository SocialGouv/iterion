package server

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Platform forge OAuth apps — a forge instance's OAuth application registered
// ONCE for every team of the deployment, under the platform sentinel tenant
// (secrets.PlatformTenantID), instead of once per team by whoever holds the
// secret. The canonical case is a GitLab instance whose applications can only
// be created at group level by its administrators: one application, one
// registration, and every team's connect wizard offers "Continue with OAuth"
// on that instance. The connect flow and the token refresher resolve the
// team's own app first, then the platform's (forgeOAuthAppRecordFor).
//
// Super-admin only, manual mode only: the secret is pasted by whoever created
// the application on the forge, and never auto-created with an admin token —
// a platform-wide credential is a platform decision.
func (s *Server) registerAdminForgeRoutes() {
	if s.authSvc == nil || s.sealer == nil || s.forgeOAuthApps == nil {
		return
	}
	s.mux.Handle("GET /api/admin/forge/oauth-apps", s.requireSuperAdmin(http.HandlerFunc(s.handleAdminListForgeOAuthApps)))
	s.mux.Handle("POST /api/admin/forge/oauth-apps", s.requireSuperAdmin(http.HandlerFunc(s.handleAdminRegisterForgeOAuthApp)))
	s.mux.Handle("DELETE /api/admin/forge/oauth-apps/{app_id}", s.requireSuperAdmin(http.HandlerFunc(s.handleAdminDeleteForgeOAuthApp)))
}

// adminForgeOAuthAppReq registers a platform app: the pasted client_id and
// client_secret of an application created on the forge, the instance it
// belongs to, and the hook base its connections inherit when the instance
// only delivers to allowlisted hosts.
type adminForgeOAuthAppReq struct {
	Provider       string `json:"provider"`
	ForgeBaseURL   string `json:"forge_base_url,omitempty"`
	ClientID       string `json:"client_id"`
	ClientSecret   string `json:"client_secret"`
	WebhookBaseURL string `json:"webhook_base_url,omitempty"`
}

// listPlatformForgeOAuthApps is every platform app, flagged as such, with
// its secrets stripped. Shared with the team listing, which appends them.
func (s *Server) listPlatformForgeOAuthApps(ctx context.Context) ([]forge.ForgeOAuthApp, error) {
	if s.forgeOAuthApps == nil {
		return nil, nil
	}
	apps, err := s.forgeOAuthApps.ListByTenant(store.WithTenant(ctx, secrets.PlatformTenantID), secrets.PlatformTenantID)
	if err != nil {
		return nil, err
	}
	for i := range apps {
		apps[i].Platform = true
		apps[i].SealedSecret = nil
		apps[i].SealedPrivateKey = nil
	}
	return apps, nil
}

func (s *Server) handleAdminListForgeOAuthApps(w http.ResponseWriter, r *http.Request) {
	apps, err := s.listPlatformForgeOAuthApps(r.Context())
	if err != nil {
		httpError(w, http.StatusInternalServerError, "list platform oauth apps: %v", err)
		return
	}
	if apps == nil {
		apps = []forge.ForgeOAuthApp{}
	}
	writeJSON(w, map[string]any{"apps": apps})
}

func (s *Server) handleAdminRegisterForgeOAuthApp(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	var req adminForgeOAuthAppReq
	if !decodeJSON(w, r, &req) {
		return
	}
	provider := forge.Provider(strings.TrimSpace(req.Provider))
	if !provider.Valid() {
		httpError(w, http.StatusBadRequest, "unknown provider %q", req.Provider)
		return
	}
	clientID, clientSecret := strings.TrimSpace(req.ClientID), strings.TrimSpace(req.ClientSecret)
	if clientID == "" || clientSecret == "" {
		httpError(w, http.StatusBadRequest, "client_id and client_secret are required")
		return
	}
	webhookBase, err := canonicalWebhookBaseURL(req.WebhookBaseURL)
	if err != nil {
		httpError(w, http.StatusBadRequest, "%v", err)
		return
	}
	app, err := s.createForgeOAuthApp(r, secrets.PlatformTenantID, id.UserID, provider, req.ForgeBaseURL, clientID, clientSecret, "", false, "manual", githubAppFacts{WebhookBaseURL: webhookBase})
	if err != nil {
		s.writeForgeOAuthAppError(w, err)
		return
	}
	writeJSON(w, app)
}

// handleAdminDeleteForgeOAuthApp removes a platform app, refusing while any
// connection of any team still names it: such a connection refreshes its
// token through this app, and deleting it would silently end every one of
// those refreshes an hour later.
func (s *Server) handleAdminDeleteForgeOAuthApp(w http.ResponseWriter, r *http.Request) {
	appID := r.PathValue("app_id")
	ctx := store.WithTenant(r.Context(), secrets.PlatformTenantID)
	app, err := s.forgeOAuthApps.Get(ctx, appID)
	if err != nil || app.TenantID != secrets.PlatformTenantID {
		httpError(w, http.StatusNotFound, "platform oauth app not found")
		return
	}
	if s.forgeConnections != nil {
		used, err := s.forgeConnections.ListByOAuthApp(store.WithoutTenantFilter(r.Context()), appID)
		if err != nil {
			httpError(w, http.StatusInternalServerError, "check connections using this app: %v", err)
			return
		}
		if len(used) > 0 {
			teams := map[string]bool{}
			for _, c := range used {
				teams[c.TenantID] = true
			}
			httpError(w, http.StatusConflict,
				"%d connection(s) across %d team(s) still authenticate through this app — remove them first, then delete the app",
				len(used), len(teams))
			return
		}
	}
	if err := s.forgeOAuthApps.Delete(ctx, appID); err != nil {
		if errors.Is(err, forge.ErrOAuthAppNotFound) {
			httpError(w, http.StatusNotFound, "platform oauth app not found")
			return
		}
		httpError(w, http.StatusInternalServerError, "delete oauth app: %v", err)
		return
	}
	s.auditPlatform(r, secrets.PlatformTenantID, "forge.oauth_app.deleted", "forge_oauth_app", appID, map[string]any{
		"provider": app.Provider, "instance": app.ForgeBaseURL,
	})
	w.WriteHeader(http.StatusNoContent)
}

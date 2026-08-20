package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Platform LLM credentials — the DB-backed form of the runner-pod env
// fallback (ANTHROPIC_API_KEY / CLAUDE_CODE_OAUTH_TOKEN et al.), so
// rotating the deployment's own provider credential is one API/CLI call
// instead of a k8s secret edit + redeploy.
//
// Storage rides the existing stores under reserved scopes: API keys are
// ordinary ApiKey rows under secrets.PlatformTenantID, OAuth-forfait
// blobs ordinary OAuthRecords under secrets.PlatformOwnerKey — see the
// consts' docs. The cloud publisher consults them as the LAST tier
// (after tenant BYOK, OAuth and the mutualised pool); the runner env
// remains the final backstop.
//
// Super-admin only: these credentials fund every tenant with nothing of
// its own, so managing them is a platform decision.
func (s *Server) registerAdminLLMRoutes() {
	if s.authSvc == nil || s.sealer == nil {
		return
	}
	if s.apiKeys != nil {
		s.mux.Handle("GET /api/admin/llm/api-keys", s.requireSuperAdmin(http.HandlerFunc(s.handleAdminListPlatformApiKeys)))
		s.mux.Handle("POST /api/admin/llm/api-keys", s.requireSuperAdmin(http.HandlerFunc(s.handleAdminCreatePlatformApiKey)))
		s.mux.Handle("PATCH /api/admin/llm/api-keys/{key_id}", s.requireSuperAdmin(http.HandlerFunc(s.handleAdminUpdatePlatformApiKey)))
		s.mux.Handle("DELETE /api/admin/llm/api-keys/{key_id}", s.requireSuperAdmin(http.HandlerFunc(s.handleAdminDeletePlatformApiKey)))
	}
	if s.oauthStore != nil {
		s.mux.Handle("GET /api/admin/llm/oauth/connections", s.requireSuperAdmin(http.HandlerFunc(s.handleAdminListPlatformOAuth)))
		s.mux.Handle("POST /api/admin/llm/oauth/{kind}/credentials", s.requireSuperAdmin(http.HandlerFunc(s.handleAdminUploadPlatformOAuth)))
		s.mux.Handle("POST /api/admin/llm/oauth/{kind}/authorize/start", s.requireSuperAdmin(http.HandlerFunc(s.handleAdminStartPlatformOAuth)))
		s.mux.Handle("POST /api/admin/llm/oauth/{kind}/authorize/complete", s.requireSuperAdmin(http.HandlerFunc(s.handleAdminCompletePlatformOAuth)))
		s.mux.Handle("POST /api/admin/llm/oauth/{kind}/refresh", s.requireSuperAdmin(http.HandlerFunc(s.handleAdminRefreshPlatformOAuth)))
		s.mux.Handle("DELETE /api/admin/llm/oauth/{kind}", s.requireSuperAdmin(http.HandlerFunc(s.handleAdminDeletePlatformOAuth)))
	}
}

// platformKeyCtx scopes the store context to the sentinel platform tenant.
// The api-keys store derives tenant_id from the context on both write
// (stamps the row) and read (filters), so this — not the caller's active
// team requireAuth stamped — is what makes a platform key land under, and
// resolve from, secrets.PlatformTenantID.
func platformKeyCtx(r *http.Request) context.Context {
	return store.WithTenant(r.Context(), secrets.PlatformTenantID)
}

func (s *Server) handleAdminListPlatformApiKeys(w http.ResponseWriter, r *http.Request) {
	// Platform keys are always team-wide rows ("" requesting user →
	// user-scoped rows excluded, which the create path never writes).
	keys, err := s.apiKeys.ListByTeam(platformKeyCtx(r), secrets.PlatformTenantID, "")
	s.writeApiKeyList(w, keys, err)
}

func (s *Server) handleAdminCreatePlatformApiKey(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	var req createApiKeyReq
	if !decodeJSON(w, r, &req) {
		return
	}
	provider, err := secrets.ParseProvider(req.Provider)
	if err != nil {
		httpError(w, http.StatusBadRequest, "%s", err.Error())
		return
	}
	if req.Secret == "" || req.Name == "" {
		httpError(w, http.StatusBadRequest, "name + secret required")
		return
	}
	keyID := secrets.NewApiKeyID()
	sealed, err := secrets.SealAPIKey(s.sealer, keyID, []byte(req.Secret))
	if err != nil {
		httpError(w, http.StatusInternalServerError, "seal: %v", err)
		return
	}
	key := secrets.ApiKey{
		ID:           keyID,
		ScopeTeamID:  secrets.PlatformTenantID,
		Provider:     provider,
		Name:         req.Name,
		Last4:        secrets.Last4(req.Secret),
		SealedSecret: sealed,
		IsDefault:    req.IsDefault,
		CreatedBy:    id.UserID,
		CreatedAt:    time.Now().UTC(),
		Fingerprint:  secrets.FingerprintSHA256(req.Secret),
	}
	ctx := platformKeyCtx(r)
	if err := s.apiKeys.Create(ctx, key); err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	if req.IsDefault {
		if err := s.apiKeys.ClearDefault(ctx, secrets.PlatformTenantID, "", provider, keyID); err != nil {
			httpError(w, http.StatusInternalServerError, "key %s created but clearing previous default failed: %v", keyID, err)
			return
		}
	}
	s.auditPlatform(r, "", "platform.llm_key.created", "platform_llm_key", keyID, map[string]any{"name": key.Name, "provider": string(provider)})
	writeJSON(w, s.toApiKeyView(key))
}

// getPlatformApiKey fetches a key under the platform tenant and verifies
// the row really is a platform one. The Mongo store's tenant filter
// already guarantees it; the explicit ScopeTeamID check keeps the
// guarantee under stores that don't filter by context (memory), so a
// super-admin route can never mutate a tenant's row by id.
func (s *Server) getPlatformApiKey(w http.ResponseWriter, r *http.Request) (secrets.ApiKey, bool) {
	keyID := r.PathValue("key_id")
	key, err := s.apiKeys.Get(platformKeyCtx(r), keyID)
	if err != nil {
		if errors.Is(err, secrets.ErrApiKeyNotFound) {
			httpError(w, http.StatusNotFound, "key not found")
			return secrets.ApiKey{}, false
		}
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return secrets.ApiKey{}, false
	}
	if key.ScopeTeamID != secrets.PlatformTenantID {
		httpError(w, http.StatusNotFound, "key not found")
		return secrets.ApiKey{}, false
	}
	return key, true
}

func (s *Server) handleAdminUpdatePlatformApiKey(w http.ResponseWriter, r *http.Request) {
	key, ok := s.getPlatformApiKey(w, r)
	if !ok {
		return
	}
	var req updateApiKeyReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Name != nil {
		key.Name = *req.Name
	}
	if req.Secret != nil && *req.Secret != "" {
		sealed, err := secrets.SealAPIKey(s.sealer, key.ID, []byte(*req.Secret))
		if err != nil {
			httpError(w, http.StatusInternalServerError, "seal: %v", err)
			return
		}
		key.SealedSecret = sealed
		key.Last4 = secrets.Last4(*req.Secret)
		key.Fingerprint = secrets.FingerprintSHA256(*req.Secret)
	}
	if req.IsDefault != nil {
		key.IsDefault = *req.IsDefault
	}
	ctx := platformKeyCtx(r)
	if err := s.apiKeys.Update(ctx, key); err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	if key.IsDefault {
		if err := s.apiKeys.ClearDefault(ctx, secrets.PlatformTenantID, "", key.Provider, key.ID); err != nil {
			httpError(w, http.StatusInternalServerError, "key updated but clearing previous default failed: %v", err)
			return
		}
	}
	s.auditPlatform(r, "", "platform.llm_key.updated", "platform_llm_key", key.ID, map[string]any{"name": key.Name, "rotated": req.Secret != nil})
	writeJSON(w, s.toApiKeyView(key))
}

func (s *Server) handleAdminDeletePlatformApiKey(w http.ResponseWriter, r *http.Request) {
	key, ok := s.getPlatformApiKey(w, r)
	if !ok {
		return
	}
	if err := s.apiKeys.Delete(platformKeyCtx(r), key.ID); err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	s.auditPlatform(r, "", "platform.llm_key.deleted", "platform_llm_key", key.ID, map[string]any{"name": key.Name, "provider": string(key.Provider)})
	w.WriteHeader(http.StatusNoContent)
}

// ---- platform OAuth-forfait (claude_code / codex) ----
//
// Thin mirrors of the /api/me and /api/teams/{id} OAuth surfaces over the
// reserved secrets.PlatformOwnerKey — the shared *OAuthForOwner helpers do
// the actual work, so the stored shape, the browser code flow, and the
// refresh semantics are identical to a user/org forfait. The background
// refresh worker picks the platform record up like any other.

func (s *Server) handleAdminListPlatformOAuth(w http.ResponseWriter, r *http.Request) {
	s.listOAuthForOwner(w, r, secrets.PlatformOwnerKey)
}

func (s *Server) handleAdminUploadPlatformOAuth(w http.ResponseWriter, r *http.Request) {
	s.uploadOAuthForOwner(w, r, secrets.PlatformOwnerKey, secrets.OAuthKind(r.PathValue("kind")))
	s.auditPlatform(r, "", "platform.llm_oauth.connected", "platform_llm_oauth", r.PathValue("kind"), map[string]any{"flow": "paste"})
}

func (s *Server) handleAdminStartPlatformOAuth(w http.ResponseWriter, r *http.Request) {
	s.startOAuthForOwner(w, r, secrets.PlatformOwnerKey, secrets.OAuthKind(r.PathValue("kind")))
}

func (s *Server) handleAdminCompletePlatformOAuth(w http.ResponseWriter, r *http.Request) {
	s.completeOAuthForOwner(w, r, secrets.PlatformOwnerKey, secrets.OAuthKind(r.PathValue("kind")))
	s.auditPlatform(r, "", "platform.llm_oauth.connected", "platform_llm_oauth", r.PathValue("kind"), map[string]any{"flow": "browser"})
}

func (s *Server) handleAdminRefreshPlatformOAuth(w http.ResponseWriter, r *http.Request) {
	s.refreshOAuthForOwner(w, r, secrets.PlatformOwnerKey, secrets.OAuthKind(r.PathValue("kind")))
}

func (s *Server) handleAdminDeletePlatformOAuth(w http.ResponseWriter, r *http.Request) {
	s.deleteOAuthForOwner(w, r, secrets.PlatformOwnerKey, secrets.OAuthKind(r.PathValue("kind")))
	s.auditPlatform(r, "", "platform.llm_oauth.deleted", "platform_llm_oauth", r.PathValue("kind"), nil)
}

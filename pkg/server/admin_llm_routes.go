package server

import (
	"errors"
	"net/http"

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
// The handlers are thin wrappers: they scope the request to the platform
// sentinel, guard that the addressed row IS a platform one, and delegate
// to the shared byok/oauth handlers — audit routing to the platform log
// happens inside auditApiKey / the auditPlatform calls below.
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

// platformScopedReq re-scopes the store context to the sentinel platform
// tenant. The api-keys store derives tenant_id from the context on both
// write (stamps the row) and read (filters); the admin routes carry no
// {id} path value, so apiKeyTenantCtx falls through to this ctx and the
// shared byok handlers operate on the platform scope.
func (s *Server) platformScopedReq(r *http.Request) *http.Request {
	return r.WithContext(store.WithTenant(r.Context(), secrets.PlatformTenantID))
}

func (s *Server) handleAdminListPlatformApiKeys(w http.ResponseWriter, r *http.Request) {
	r = s.platformScopedReq(r)
	// Platform keys are always team-wide rows ("" requesting user →
	// user-scoped rows excluded, which the create path never writes).
	keys, err := s.apiKeys.ListByTeam(r.Context(), secrets.PlatformTenantID, "")
	s.writeApiKeyList(w, keys, err)
}

func (s *Server) handleAdminCreatePlatformApiKey(w http.ResponseWriter, r *http.Request) {
	s.handleCreateApiKey(w, s.platformScopedReq(r), secrets.PlatformTenantID, "")
}

// requirePlatformApiKey verifies the addressed row really is a platform
// one before the shared handler runs. The Mongo store's tenant filter
// already guarantees it; the explicit ScopeTeamID check keeps the
// guarantee under stores that don't filter by context (memory), so a
// super-admin route can never mutate a tenant's row by id.
func (s *Server) requirePlatformApiKey(w http.ResponseWriter, r *http.Request) bool {
	key, err := s.apiKeys.Get(r.Context(), r.PathValue("key_id"))
	if err != nil {
		if errors.Is(err, secrets.ErrApiKeyNotFound) {
			httpError(w, http.StatusNotFound, "key not found")
			return false
		}
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return false
	}
	if key.ScopeTeamID != secrets.PlatformTenantID {
		httpError(w, http.StatusNotFound, "key not found")
		return false
	}
	return true
}

func (s *Server) handleAdminUpdatePlatformApiKey(w http.ResponseWriter, r *http.Request) {
	r = s.platformScopedReq(r)
	if !s.requirePlatformApiKey(w, r) {
		return
	}
	s.handleUpdateApiKey(w, r)
}

func (s *Server) handleAdminDeletePlatformApiKey(w http.ResponseWriter, r *http.Request) {
	r = s.platformScopedReq(r)
	if !s.requirePlatformApiKey(w, r) {
		return
	}
	s.handleDeleteApiKey(w, r)
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

// The OAuth handlers carry no audit call: it fires inside the shared
// *OAuthForOwner helpers on the store-write success path (auditOAuthByOwner
// routes a PlatformOwnerKey mutation to the platform log), so a rejected
// connect/delete never forges an event.

func (s *Server) handleAdminUploadPlatformOAuth(w http.ResponseWriter, r *http.Request) {
	s.uploadOAuthForOwner(w, r, secrets.PlatformOwnerKey, secrets.OAuthKind(r.PathValue("kind")))
}

func (s *Server) handleAdminStartPlatformOAuth(w http.ResponseWriter, r *http.Request) {
	s.startOAuthForOwner(w, r, secrets.PlatformOwnerKey, secrets.OAuthKind(r.PathValue("kind")))
}

func (s *Server) handleAdminCompletePlatformOAuth(w http.ResponseWriter, r *http.Request) {
	s.completeOAuthForOwner(w, r, secrets.PlatformOwnerKey, secrets.OAuthKind(r.PathValue("kind")))
}

func (s *Server) handleAdminRefreshPlatformOAuth(w http.ResponseWriter, r *http.Request) {
	s.refreshOAuthForOwner(w, r, secrets.PlatformOwnerKey, secrets.OAuthKind(r.PathValue("kind")))
}

func (s *Server) handleAdminDeletePlatformOAuth(w http.ResponseWriter, r *http.Request) {
	s.deleteOAuthForOwner(w, r, secrets.PlatformOwnerKey, secrets.OAuthKind(r.PathValue("kind")))
}

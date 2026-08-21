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

// registerBYOKRoutes wires every /api/teams/:id/api-keys and
// /api/me/api-keys endpoint. Called from routes() when an
// ApiKeyStore is wired.
func (s *Server) registerBYOKRoutes() {
	s.mux.Handle("GET /api/teams/{id}/api-keys", s.requireAuth(http.HandlerFunc(s.handleListTeamApiKeys)))
	s.mux.Handle("POST /api/teams/{id}/api-keys", s.requireAuth(http.HandlerFunc(s.handleCreateTeamApiKey)))
	s.mux.Handle("PATCH /api/teams/{id}/api-keys/{key_id}", s.requireAuth(http.HandlerFunc(s.handleUpdateApiKey)))
	s.mux.Handle("DELETE /api/teams/{id}/api-keys/{key_id}", s.requireAuth(http.HandlerFunc(s.handleDeleteApiKey)))

	s.mux.Handle("GET /api/me/api-keys", s.requireAuth(http.HandlerFunc(s.handleListMyApiKeys)))
	s.mux.Handle("POST /api/me/api-keys", s.requireAuth(http.HandlerFunc(s.handleCreateMyApiKey)))
	s.mux.Handle("PATCH /api/me/api-keys/{key_id}", s.requireAuth(http.HandlerFunc(s.handleUpdateApiKey)))
	s.mux.Handle("DELETE /api/me/api-keys/{key_id}", s.requireAuth(http.HandlerFunc(s.handleDeleteApiKey)))
}

type apiKeyView struct {
	ID          string  `json:"id"`
	Provider    string  `json:"provider"`
	Name        string  `json:"name"`
	Last4       string  `json:"last4,omitempty"`
	Fingerprint string  `json:"fingerprint,omitempty"`
	IsDefault   bool    `json:"is_default"`
	ScopeUserID string  `json:"scope_user_id,omitempty"`
	CreatedAt   string  `json:"created_at"`
	LastUsedAt  *string `json:"last_used_at,omitempty"`
}

type createApiKeyReq struct {
	Provider  string `json:"provider"`
	Name      string `json:"name"`
	Secret    string `json:"secret"`
	IsDefault bool   `json:"is_default,omitempty"`
}

type updateApiKeyReq struct {
	Name      *string `json:"name,omitempty"`
	IsDefault *bool   `json:"is_default,omitempty"`
	Secret    *string `json:"secret,omitempty"` // rotate
}

func (s *Server) toApiKeyView(k secrets.ApiKey) apiKeyView {
	return apiKeyView{
		ID:          k.ID,
		Provider:    string(k.Provider),
		Name:        k.Name,
		Last4:       k.Last4,
		Fingerprint: k.Fingerprint,
		IsDefault:   k.IsDefault,
		ScopeUserID: k.ScopeUserID,
		CreatedAt:   k.CreatedAt.Format(time.RFC3339),
		LastUsedAt:  optRFC3339(k.LastUsedAt),
	}
}

// writeApiKeyList serialises a slice of ApiKey records into the
// {"keys":[...]} envelope shared by the team and user-scoped list
// endpoints. On error it writes a 500 and returns false so the
// caller can early-out.
func (s *Server) writeApiKeyList(w http.ResponseWriter, keys []secrets.ApiKey, err error) bool {
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return false
	}
	views := make([]apiKeyView, 0, len(keys))
	for _, k := range keys {
		views = append(views, s.toApiKeyView(k))
	}
	writeJSON(w, struct {
		Keys []apiKeyView `json:"keys"`
	}{Keys: views})
	return true
}

// apiKeyTenantCtx scopes the store context to the team a route names in its
// path, instead of the caller's ACTIVE team that requireAuth stamped.
//
// The api-keys store derives tenant_id from the context — on write it stamps
// the row, on read it filters. So a key created for a team other than the
// caller's active one used to land as (scope_team = target, tenant_id =
// caller's active team): listable from the context that created it, and
// INVISIBLE to the runs of the team it was meant to fund. Nothing failed —
// the run simply resolved no key and fell back to the platform credential,
// which is the one shape a credential bug must never take.
//
// Routes with no {id} (the /api/me family) keep the active team: there the
// caller's own tenant IS the scope.
func apiKeyTenantCtx(r *http.Request) context.Context {
	if teamID := r.PathValue("id"); teamID != "" {
		return store.WithTenant(r.Context(), teamID)
	}
	return r.Context()
}

// auditApiKey routes an api-key mutation to the right audit log: platform
// rows (ScopeTeamID == secrets.PlatformTenantID) are super-admin actions on
// the deployment's own fallback credentials and land in the PLATFORM log —
// a tenant-scoped row under the sentinel tenant would be readable by
// nobody. Every other row is ordinary tenant BYOK.
func (s *Server) auditApiKey(r *http.Request, teamID, suffix, keyID string, meta map[string]any) {
	if teamID == secrets.PlatformTenantID {
		s.auditPlatform(r, "", "platform.llm_key."+suffix, "platform_llm_key", keyID, meta)
		return
	}
	s.auditTenant(r, teamID, "byok."+suffix, "byok", keyID, meta)
}

func (s *Server) handleListTeamApiKeys(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canViewTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "not a member")
		return
	}
	// Team admins see all team-wide keys + their own user-scoped
	// keys (matches BYOK plan). Members only see what's visible.
	keys, err := s.apiKeys.ListByTeam(apiKeyTenantCtx(r), teamID, id.UserID)
	s.writeApiKeyList(w, keys, err)
}

func (s *Server) handleCreateTeamApiKey(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canManageTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "admin or owner required")
		return
	}
	s.handleCreateApiKey(w, r, teamID, "")
}

func (s *Server) handleListMyApiKeys(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	if id.TeamID == "" {
		httpError(w, http.StatusBadRequest, "no active team")
		return
	}
	keys, err := s.apiKeys.ListByUser(r.Context(), id.TeamID, id.UserID)
	s.writeApiKeyList(w, keys, err)
}

func (s *Server) handleCreateMyApiKey(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	if id.TeamID == "" {
		httpError(w, http.StatusBadRequest, "no active team")
		return
	}
	s.handleCreateApiKey(w, r, id.TeamID, id.UserID)
}

// handleCreateApiKey is the shared write path used by both team and
// user creation endpoints. teamID is always set; userID is "" for
// team-scoped keys.
func (s *Server) handleCreateApiKey(w http.ResponseWriter, r *http.Request, teamID, userID string) {
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
	now := time.Now().UTC()
	key := secrets.ApiKey{
		ID:           keyID,
		ScopeTeamID:  teamID,
		ScopeUserID:  userID,
		Provider:     provider,
		Name:         req.Name,
		Last4:        secrets.Last4(req.Secret),
		SealedSecret: sealed,
		IsDefault:    req.IsDefault,
		CreatedBy:    id.UserID,
		CreatedAt:    now,
		Fingerprint:  secrets.FingerprintSHA256(req.Secret),
	}
	ctx := apiKeyTenantCtx(r)
	if err := s.apiKeys.Create(ctx, key); err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	if req.IsDefault {
		if err := s.apiKeys.ClearDefault(ctx, teamID, userID, provider, keyID); err != nil {
			httpError(w, http.StatusInternalServerError, "key %s created but clearing previous default failed: %v", keyID, err)
			return
		}
	}
	s.auditApiKey(r, teamID, "created", keyID, map[string]any{"name": key.Name, "provider": string(provider), "user_scoped": userID != ""})
	writeJSON(w, s.toApiKeyView(key))
}

func (s *Server) handleUpdateApiKey(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	keyID := r.PathValue("key_id")
	ctx := apiKeyTenantCtx(r)
	key, err := s.apiKeys.Get(ctx, keyID)
	if err != nil {
		if errors.Is(err, secrets.ErrApiKeyNotFound) {
			httpError(w, http.StatusNotFound, "key not found")
			return
		}
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	// AuthZ: team-wide keys → admin/owner of that team. User-scoped
	// keys → only the owning user (or super-admin).
	if !s.canMutateApiKey(r.Context(), id, key) {
		httpError(w, http.StatusForbidden, "cannot mutate this key")
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
	if err := s.apiKeys.Update(ctx, key); err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	if key.IsDefault {
		if err := s.apiKeys.ClearDefault(ctx, key.ScopeTeamID, key.ScopeUserID, key.Provider, key.ID); err != nil {
			httpError(w, http.StatusInternalServerError, "key updated but clearing previous default failed: %v", err)
			return
		}
	}
	s.auditApiKey(r, key.ScopeTeamID, "updated", key.ID, map[string]any{"name": key.Name, "rotated": req.Secret != nil})
	writeJSON(w, s.toApiKeyView(key))
}

func (s *Server) handleDeleteApiKey(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	keyID := r.PathValue("key_id")
	ctx := apiKeyTenantCtx(r)
	key, err := s.apiKeys.Get(ctx, keyID)
	if err != nil {
		if errors.Is(err, secrets.ErrApiKeyNotFound) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	if !s.canMutateApiKey(r.Context(), id, key) {
		httpError(w, http.StatusForbidden, "cannot delete this key")
		return
	}
	if err := s.apiKeys.Delete(ctx, key.ID); err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	s.auditApiKey(r, key.ScopeTeamID, "deleted", key.ID, map[string]any{"name": key.Name, "provider": string(key.Provider)})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) canMutateApiKey(ctx context.Context, id auth.Identity, k secrets.ApiKey) bool {
	return s.canMutateScopedRecord(ctx, id, k.ScopeUserID, k.ScopeTeamID)
}

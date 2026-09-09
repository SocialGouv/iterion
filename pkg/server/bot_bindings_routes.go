package server

import (
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// registerBotBindingRoutes wires the per-org bot-secret-binding CRUD: a
// policy wrapper that makes a stored generic secret resolvable for a
// specific bot under the name its workflow declares.
func (s *Server) registerBotBindingRoutes() {
	s.mux.Handle("GET /api/teams/{id}/bots/{bot_id}/bindings", s.requireAuth(http.HandlerFunc(s.handleListBotBindings)))
	s.mux.Handle("POST /api/teams/{id}/bots/{bot_id}/bindings", s.requireAuth(http.HandlerFunc(s.handleCreateBotBinding)))
	s.mux.Handle("PATCH /api/teams/{id}/bots/{bot_id}/bindings/{binding_id}", s.requireAuth(http.HandlerFunc(s.handleUpdateBotBinding)))
	s.mux.Handle("DELETE /api/teams/{id}/bots/{bot_id}/bindings/{binding_id}", s.requireAuth(http.HandlerFunc(s.handleDeleteBotBinding)))
}

type botBindingReq struct {
	SecretID              *string  `json:"secret_id,omitempty"`
	SecretNameForWorkflow *string  `json:"secret_name_for_workflow,omitempty"`
	AllowedHosts          []string `json:"allowed_hosts,omitempty"`
}

// validateBindingSecret confirms the referenced generic secret is bindable
// for the route team. Bot bindings are shared org automation policy, so they
// may only reference team-scoped generic secrets in that team; personal
// (/api/me/secrets) credentials are intentionally not bindable by ID.
func (s *Server) validateBindingSecret(r *http.Request, teamID, secretID string) bool {
	if s.genericSecrets == nil {
		return true // can't validate; allow (binding still tenant-scoped)
	}
	sec, err := s.genericSecrets.Get(teamTenantCtx(r.Context(), teamID), secretID)
	if err != nil {
		return false
	}
	return sec.ScopeTeamID == teamID && sec.ScopeUserID == ""
}

func (s *Server) bindingForTenantBot(w http.ResponseWriter, r *http.Request, teamID, botID, bindingID string) (secrets.BotSecretBinding, bool) {
	b, err := s.botBindings.Get(teamTenantCtx(r.Context(), teamID), bindingID)
	if err != nil || b.TenantID != teamID || b.BotID != botID {
		httpError(w, http.StatusNotFound, "binding not found")
		return secrets.BotSecretBinding{}, false
	}
	return b, true
}

func (s *Server) handleListBotBindings(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID, botID := r.PathValue("id"), r.PathValue("bot_id")
	if !s.canViewTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "not a member")
		return
	}
	list, err := s.botBindings.ListByTenantBot(teamPathTenantCtx(r), teamID, botID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	if list == nil {
		list = []secrets.BotSecretBinding{}
	}
	writeJSON(w, struct {
		Bindings []secrets.BotSecretBinding `json:"bindings"`
	}{Bindings: list})
}

func (s *Server) handleCreateBotBinding(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID, botID := r.PathValue("id"), r.PathValue("bot_id")
	if !s.canManageTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "admin or owner required")
		return
	}
	var req botBindingReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.SecretID == nil || *req.SecretID == "" || req.SecretNameForWorkflow == nil || *req.SecretNameForWorkflow == "" {
		httpError(w, http.StatusBadRequest, "secret_id and secret_name_for_workflow required")
		return
	}
	if !s.validateBindingSecret(r, teamID, *req.SecretID) {
		httpError(w, http.StatusBadRequest, "secret_id is not a team-scoped secret in this org")
		return
	}
	now := time.Now().UTC()
	b := secrets.BotSecretBinding{
		ID:                    uuid.NewString(),
		TenantID:              teamID,
		BotID:                 botID,
		SecretID:              *req.SecretID,
		SecretNameForWorkflow: *req.SecretNameForWorkflow,
		AllowedHosts:          req.AllowedHosts,
		CreatedBy:             id.UserID,
		CreatedAt:             now,
		UpdatedAt:             now,
	}
	// The binding must land in the team it is created FOR — a binding
	// stamped with the caller's active tenant is invisible to that team's
	// runs, so the bot silently runs without the credential (#997).
	if err := s.botBindings.Create(teamPathTenantCtx(r), b); err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	s.auditTenant(r, teamID, "binding.created", "binding", b.ID, map[string]any{"bot_id": botID, "secret_name": b.SecretNameForWorkflow, "allowed_hosts": len(b.AllowedHosts)})
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, b)
}

func (s *Server) handleUpdateBotBinding(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID, botID := r.PathValue("id"), r.PathValue("bot_id")
	if !s.canManageTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "admin or owner required")
		return
	}
	b, ok := s.bindingForTenantBot(w, r, teamID, botID, r.PathValue("binding_id"))
	if !ok {
		return
	}
	var req botBindingReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.SecretID != nil && *req.SecretID != "" {
		if !s.validateBindingSecret(r, teamID, *req.SecretID) {
			httpError(w, http.StatusBadRequest, "secret_id is not a team-scoped secret in this org")
			return
		}
		b.SecretID = *req.SecretID
	}
	if req.SecretNameForWorkflow != nil && *req.SecretNameForWorkflow != "" {
		b.SecretNameForWorkflow = *req.SecretNameForWorkflow
	}
	if req.AllowedHosts != nil {
		b.AllowedHosts = req.AllowedHosts
	}
	b.UpdatedAt = time.Now().UTC()
	if err := s.botBindings.Update(teamPathTenantCtx(r), b); err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	s.auditTenant(r, teamID, "binding.updated", "binding", b.ID, map[string]any{"bot_id": botID, "secret_name": b.SecretNameForWorkflow})
	writeJSON(w, b)
}

func (s *Server) handleDeleteBotBinding(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID, botID := r.PathValue("id"), r.PathValue("bot_id")
	if !s.canManageTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "admin or owner required")
		return
	}
	if _, ok := s.bindingForTenantBot(w, r, teamID, botID, r.PathValue("binding_id")); !ok {
		return
	}
	if err := s.botBindings.Delete(teamPathTenantCtx(r), r.PathValue("binding_id")); err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	s.auditTenant(r, teamID, "binding.deleted", "binding", r.PathValue("binding_id"), map[string]any{"bot_id": botID})
	w.WriteHeader(http.StatusNoContent)
}

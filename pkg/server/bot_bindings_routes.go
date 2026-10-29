package server

import (
	"errors"
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
	// Kind is optional: absent means the secret kind, which is what every
	// client sent before kinds existed (ADR-147 D2).
	Kind                  *string                `json:"kind,omitempty"`
	SecretID              *string                `json:"secret_id,omitempty"`
	SecretNameForWorkflow *string                `json:"secret_name_for_workflow,omitempty"`
	AllowedHosts          []string               `json:"allowed_hosts,omitempty"`
	Context               *secrets.ContextSource `json:"context,omitempty"`
}

// BindingContextNotMaterialisedCode is the stable code a client reads when it
// creates a context binding before the materialisation slice lands: the only
// part of a 501 body that survives redaction.
const BindingContextNotMaterialisedCode = "context_binding_not_materialised"

// bindingStoreError maps what the store refuses onto what the caller is owed:
// a name already bound on that bot is a conflict, not a server fault. Both
// store twins return the same typed error, so this reads the same locally and
// in the cloud.
func bindingStoreError(w http.ResponseWriter, err error) {
	if errors.Is(err, secrets.ErrBindingNameConflict) {
		httpError(w, http.StatusConflict, "a binding for this workflow name already exists on this bot")
		return
	}
	httpError(w, http.StatusInternalServerError, "%s", err.Error())
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

// bindingCredentialsAreBindable checks EVERY generic secret a binding
// dereferences against the one tenancy predicate bindings have always used:
// the secret a `secret` kind exposes, and the read credential a `context`
// kind pins. A kind that skipped it would reopen the door
// bindableGenericSecretForBotBinding exists to close — a shared automation
// policy dereferencing somebody's personal credential.
func (s *Server) bindingCredentialsAreBindable(r *http.Request, teamID string, b secrets.BotSecretBinding) (string, bool) {
	if b.SecretID != "" && !s.validateBindingSecret(r, teamID, b.SecretID) {
		return "secret_id", false
	}
	if b.Context != nil && b.Context.SecretID != "" && !s.validateBindingSecret(r, teamID, b.Context.SecretID) {
		return "context.secret_id", false
	}
	return "", true
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
	now := time.Now().UTC()
	b := secrets.BotSecretBinding{
		ID:           uuid.NewString(),
		TenantID:     teamID,
		BotID:        botID,
		AllowedHosts: req.AllowedHosts,
		Context:      req.Context,
		CreatedBy:    id.UserID,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if req.Kind != nil {
		b.Kind = *req.Kind
	}
	if req.SecretID != nil {
		b.SecretID = *req.SecretID
	}
	if req.SecretNameForWorkflow != nil {
		b.SecretNameForWorkflow = *req.SecretNameForWorkflow
	}
	if err := b.Validate(); err != nil {
		httpError(w, http.StatusBadRequest, "%s", err.Error())
		return
	}
	if field, ok := s.bindingCredentialsAreBindable(r, teamID, b); !ok {
		httpError(w, http.StatusBadRequest, "%s is not a team-scoped secret in this org", field)
		return
	}
	// The context kind is stored and served, and nothing materialises it into
	// a run yet. Refusing it by name is the honest half of that state: a
	// binding accepted here would be an operator-visible no-op, which is the
	// very defect the kind exists to fix (#1908, ADR-147). The reason travels
	// as an error_code, the only field a 5xx body keeps (httpx.WriteErrorJSON
	// redacts the message), so a client can say what is missing.
	if b.EffectiveKind() == secrets.BindingKindContext {
		s.httpErrorCode(w, r, http.StatusNotImplemented, BindingContextNotMaterialisedCode,
			"a context binding is not materialised into runs yet — it arrives with the materialisation slice of #1908")
		return
	}
	// The binding must land in the team it is created FOR — a binding
	// stamped with the caller's active tenant is invisible to that team's
	// runs, so the bot silently runs without the credential (#997).
	if err := s.botBindings.Create(teamPathTenantCtx(r), b); err != nil {
		bindingStoreError(w, err)
		return
	}
	s.auditTenant(r, teamID, "binding.created", "binding", b.ID, map[string]any{"bot_id": botID, "kind": b.EffectiveKind(), "secret_name": b.SecretNameForWorkflow, "allowed_hosts": len(b.AllowedHosts)})
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
	if req.Context != nil {
		b.Context = req.Context
		if b.Context.SecretID != "" && !s.validateBindingSecret(r, teamID, b.Context.SecretID) {
			httpError(w, http.StatusBadRequest, "context.secret_id is not a team-scoped secret in this org")
			return
		}
	}
	// A patch cannot turn a binding into a shape its kind forbids: the stored
	// record is what the resolver reads, so it is validated after the merge,
	// not field by field.
	if err := b.Validate(); err != nil {
		httpError(w, http.StatusBadRequest, "%s", err.Error())
		return
	}
	b.UpdatedAt = time.Now().UTC()
	if err := s.botBindings.Update(teamPathTenantCtx(r), b); err != nil {
		bindingStoreError(w, err)
		return
	}
	s.auditTenant(r, teamID, "binding.updated", "binding", b.ID, map[string]any{"bot_id": botID, "kind": b.EffectiveKind(), "secret_name": b.SecretNameForWorkflow})
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

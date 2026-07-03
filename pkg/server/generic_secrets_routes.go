package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

func (s *Server) registerGenericSecretRoutes() {
	s.mux.Handle("GET /api/teams/{id}/secrets", s.requireAuth(http.HandlerFunc(s.handleListTeamSecrets)))
	s.mux.Handle("POST /api/teams/{id}/secrets", s.requireAuth(http.HandlerFunc(s.handleCreateTeamSecret)))
	s.mux.Handle("PATCH /api/teams/{id}/secrets/{secret_id}", s.requireAuth(http.HandlerFunc(s.handleUpdateGenericSecret)))
	s.mux.Handle("DELETE /api/teams/{id}/secrets/{secret_id}", s.requireAuth(http.HandlerFunc(s.handleDeleteGenericSecret)))

	s.mux.Handle("GET /api/me/secrets", s.requireAuth(http.HandlerFunc(s.handleListMySecrets)))
	s.mux.Handle("POST /api/me/secrets", s.requireAuth(http.HandlerFunc(s.handleCreateMySecret)))
	s.mux.Handle("PATCH /api/me/secrets/{secret_id}", s.requireAuth(http.HandlerFunc(s.handleUpdateGenericSecret)))
	s.mux.Handle("DELETE /api/me/secrets/{secret_id}", s.requireAuth(http.HandlerFunc(s.handleDeleteGenericSecret)))
}

type genericSecretView struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Last4       string  `json:"last4,omitempty"`
	Fingerprint string  `json:"fingerprint,omitempty"`
	ScopeUserID string  `json:"scope_user_id,omitempty"`
	CreatedAt   string  `json:"created_at"`
	LastUsedAt  *string `json:"last_used_at,omitempty"`
}

type createGenericSecretReq struct {
	Name   string `json:"name"`
	Secret string `json:"secret"`
}

type updateGenericSecretReq struct {
	Name   *string `json:"name,omitempty"`
	Secret *string `json:"secret,omitempty"`
}

func toGenericSecretView(rec secrets.GenericSecret) genericSecretView {
	return genericSecretView{
		ID:          rec.ID,
		Name:        rec.Name,
		Last4:       rec.Last4,
		Fingerprint: rec.Fingerprint,
		ScopeUserID: rec.ScopeUserID,
		CreatedAt:   rec.CreatedAt.Format(time.RFC3339),
		LastUsedAt:  optRFC3339(rec.LastUsedAt),
	}
}

func writeGenericSecretList(w http.ResponseWriter, records []secrets.GenericSecret, err error) bool {
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return false
	}
	views := make([]genericSecretView, 0, len(records))
	for _, rec := range records {
		views = append(views, toGenericSecretView(rec))
	}
	writeJSON(w, struct {
		Secrets []genericSecretView `json:"secrets"`
	}{Secrets: views})
	return true
}

func (s *Server) handleListTeamSecrets(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canViewTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "not a member")
		return
	}
	records, err := s.genericSecrets.ListByTeam(r.Context(), teamID, id.UserID)
	writeGenericSecretList(w, records, err)
}

func (s *Server) handleCreateTeamSecret(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canManageTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "admin or owner required")
		return
	}
	s.handleCreateGenericSecret(w, r, teamID, "")
}

func (s *Server) handleListMySecrets(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	if id.TeamID == "" {
		httpError(w, http.StatusBadRequest, "no active team")
		return
	}
	records, err := s.genericSecrets.ListByUser(r.Context(), id.TeamID, id.UserID)
	writeGenericSecretList(w, records, err)
}

func (s *Server) handleCreateMySecret(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	if id.TeamID == "" {
		httpError(w, http.StatusBadRequest, "no active team")
		return
	}
	s.handleCreateGenericSecret(w, r, id.TeamID, id.UserID)
}

func (s *Server) handleCreateGenericSecret(w http.ResponseWriter, r *http.Request, teamID, userID string) {
	id, _ := auth.FromContext(r.Context())
	var req createGenericSecretReq
	if !decodeJSON(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if !validGenericSecretName(name) || req.Secret == "" {
		httpError(w, http.StatusBadRequest, "name + secret required")
		return
	}
	secretID := secrets.NewGenericSecretID()
	sealed, err := secrets.SealGenericSecret(s.sealer, secretID, []byte(req.Secret))
	if err != nil {
		httpError(w, http.StatusInternalServerError, "seal: %v", err)
		return
	}
	now := time.Now().UTC()
	rec := secrets.GenericSecret{
		ID:           secretID,
		ScopeTeamID:  teamID,
		ScopeUserID:  userID,
		Name:         name,
		Last4:        secrets.Last4(req.Secret),
		SealedSecret: sealed,
		CreatedBy:    id.UserID,
		CreatedAt:    now,
		Fingerprint:  secrets.FingerprintSHA256(req.Secret),
	}
	if err := s.genericSecrets.Create(r.Context(), rec); err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	s.auditTenant(r, teamID, "secret.created", "secret", secretID, map[string]any{"name": name, "user_scoped": userID != ""})
	writeJSON(w, toGenericSecretView(rec))
}

func (s *Server) handleUpdateGenericSecret(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	secretID := r.PathValue("secret_id")
	rec, err := s.genericSecrets.Get(r.Context(), secretID)
	if err != nil {
		if errors.Is(err, secrets.ErrGenericSecretNotFound) {
			httpError(w, http.StatusNotFound, "secret not found")
			return
		}
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	if !s.canMutateGenericSecret(r.Context(), id, rec) {
		httpError(w, http.StatusForbidden, "cannot mutate this secret")
		return
	}
	var req updateGenericSecretReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if !validGenericSecretName(name) {
			httpError(w, http.StatusBadRequest, "invalid secret name")
			return
		}
		rec.Name = name
	}
	if req.Secret != nil && *req.Secret != "" {
		sealed, err := secrets.SealGenericSecret(s.sealer, rec.ID, []byte(*req.Secret))
		if err != nil {
			httpError(w, http.StatusInternalServerError, "seal: %v", err)
			return
		}
		rec.SealedSecret = sealed
		rec.Last4 = secrets.Last4(*req.Secret)
		rec.Fingerprint = secrets.FingerprintSHA256(*req.Secret)
	}
	if err := s.genericSecrets.Update(r.Context(), rec); err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	s.auditTenant(r, rec.ScopeTeamID, "secret.updated", "secret", rec.ID, map[string]any{"name": rec.Name, "rotated": req.Secret != nil && *req.Secret != ""})
	writeJSON(w, toGenericSecretView(rec))
}

func (s *Server) handleDeleteGenericSecret(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	secretID := r.PathValue("secret_id")
	rec, err := s.genericSecrets.Get(r.Context(), secretID)
	if err != nil {
		if errors.Is(err, secrets.ErrGenericSecretNotFound) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	if !s.canMutateGenericSecret(r.Context(), id, rec) {
		httpError(w, http.StatusForbidden, "cannot delete this secret")
		return
	}
	if err := s.genericSecrets.Delete(r.Context(), rec.ID); err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	s.auditTenant(r, rec.ScopeTeamID, "secret.deleted", "secret", rec.ID, map[string]any{"name": rec.Name})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) canMutateGenericSecret(ctx context.Context, id auth.Identity, rec secrets.GenericSecret) bool {
	return s.canMutateScopedRecord(ctx, id, rec.ScopeUserID, rec.ScopeTeamID)
}

// canMutateScopedRecord authorizes a mutation on a user/team-scoped record:
// super-admins always may; a user-scoped record requires ownership; a
// team-scoped record requires at least admin role on that team. Shared by
// canMutateApiKey and canMutateGenericSecret.
func (s *Server) canMutateScopedRecord(ctx context.Context, id auth.Identity, scopeUserID, scopeTeamID string) bool {
	if id.IsSuperAdmin {
		return true
	}
	if scopeUserID != "" {
		return scopeUserID == id.UserID
	}
	mb, err := s.authStore().GetMembership(ctx, id.UserID, scopeTeamID)
	if err != nil {
		return false
	}
	return mb.Role.AtLeast(identity.RoleAdmin)
}

func validGenericSecretName(name string) bool {
	if name == "" || len(name) > 128 || strings.ContainsAny(name, "\n\r\x00") {
		return false
	}
	for i, r := range name {
		switch {
		case r == '_':
		case r >= 'A' && r <= 'Z':
		case r >= 'a' && r <= 'z':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

package server

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/secrets"
)

// registerLocalSecretRoutes wires the local (non-cloud) secret management
// surface. Unlike the cloud generic-secret routes (/api/teams/... and
// /api/me/..., auth + tenant gated), these are unauthenticated single-operator
// routes for the desktop / local studio — mirroring how plugins and projects
// register without requireAuth. Gated in server_routes.go on local mode +
// a wired store + sealer.
//
// Values are AES-GCM sealed before persistence and never returned in any
// response (only last4 + fingerprint), exactly like the cloud handlers.
func (s *Server) registerLocalSecretRoutes() {
	s.mux.HandleFunc("GET /api/local/secrets", s.handleListLocalSecrets)
	s.mux.HandleFunc("POST /api/local/secrets", s.handleCreateLocalSecret)
	s.mux.HandleFunc("PATCH /api/local/secrets/{secret_id}", s.handleUpdateLocalSecret)
	s.mux.HandleFunc("DELETE /api/local/secrets/{secret_id}", s.handleDeleteLocalSecret)
}

type localSecretView struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Scope        string   `json:"scope"` // "global" | "project"
	Last4        string   `json:"last4,omitempty"`
	Fingerprint  string   `json:"fingerprint,omitempty"`
	AllowedHosts []string `json:"allowed_hosts,omitempty"`
	CreatedAt    string   `json:"created_at"`
	LastUsedAt   *string  `json:"last_used_at,omitempty"`
}

type createLocalSecretReq struct {
	Name         string   `json:"name"`
	Secret       string   `json:"secret"`
	Scope        string   `json:"scope,omitempty"` // "global" (default) | "project"
	AllowedHosts []string `json:"allowed_hosts,omitempty"`
}

type updateLocalSecretReq struct {
	Name         *string   `json:"name,omitempty"`
	Secret       *string   `json:"secret,omitempty"`
	AllowedHosts *[]string `json:"allowed_hosts,omitempty"`
}

func toLocalSecretView(rec secrets.GenericSecret, scope string) localSecretView {
	return localSecretView{
		ID:           rec.ID,
		Name:         rec.Name,
		Scope:        scope,
		Last4:        rec.Last4,
		Fingerprint:  rec.Fingerprint,
		AllowedHosts: rec.AllowedHosts,
		CreatedAt:    rec.CreatedAt.Format(time.RFC3339),
		LastUsedAt:   optRFC3339(rec.LastUsedAt),
	}
}

func (s *Server) handleListLocalSecrets(w http.ResponseWriter, r *http.Request) {
	scoped, err := s.localSecrets.ListScoped(r.Context(), secrets.LocalScopeTeam, "")
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	views := make([]localSecretView, 0, len(scoped))
	for _, sc := range scoped {
		views = append(views, toLocalSecretView(sc.Secret, sc.Scope))
	}
	writeJSON(w, struct {
		Secrets []localSecretView `json:"secrets"`
	}{Secrets: views})
}

func (s *Server) handleCreateLocalSecret(w http.ResponseWriter, r *http.Request) {
	var req createLocalSecretReq
	if !decodeJSON(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if !validGenericSecretName(name) || req.Secret == "" {
		httpError(w, http.StatusBadRequest, "name + secret required")
		return
	}
	// Resolve the effective scope: "project" degrades to "global" when no
	// project layer is active, so the stored record and the response label
	// match where the secret actually lands (never claim "project" for a
	// value written to the global file).
	scope := normalizeLocalScope(req.Scope)
	if scope == "project" {
		if _, active := s.localSecrets.Project(); !active {
			scope = "global"
		}
	}
	target := s.localSecrets.ForScope(scope)

	// Atomic upsert-by-name (create or rotate) under the store's cross-process
	// lock. On rotate, the egress host lock is overwritten only when the request
	// actually supplies allowed_hosts (nil = omitted → preserve), so a value
	// rotation never silently broadens egress.
	rec, _, err := target.UpsertByName(s.sealer, name, req.Secret, req.AllowedHosts, req.AllowedHosts != nil)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	writeJSON(w, toLocalSecretView(rec, scope))
}

func (s *Server) handleUpdateLocalSecret(w http.ResponseWriter, r *http.Request) {
	secretID := r.PathValue("secret_id")
	rec, target, scope, err := s.localFindByID(r, secretID)
	if err != nil {
		s.writeLocalSecretLookupError(w, err)
		return
	}
	var req updateLocalSecretReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if !validGenericSecretName(name) {
			httpError(w, http.StatusBadRequest, "invalid secret name")
			return
		}
		// Reject a rename onto an existing name in the same layer — two records
		// sharing a Name would make GetByName / ListScoped resolve one of them
		// nondeterministically.
		if other, ok := target.GetByName(name); ok && other.ID != rec.ID {
			httpError(w, http.StatusConflict, "a secret named %q already exists in this scope", name)
			return
		}
		rec.Name = name
	}
	if req.AllowedHosts != nil {
		rec.AllowedHosts = *req.AllowedHosts
	}
	if req.Secret != nil && *req.Secret != "" {
		if err := secrets.SealInto(s.sealer, &rec, *req.Secret); err != nil {
			httpError(w, http.StatusInternalServerError, "seal: %v", err)
			return
		}
	}
	if err := target.Update(r.Context(), rec); err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	writeJSON(w, toLocalSecretView(rec, scope))
}

func (s *Server) handleDeleteLocalSecret(w http.ResponseWriter, r *http.Request) {
	secretID := r.PathValue("secret_id")
	rec, target, _, err := s.localFindByID(r, secretID)
	if err != nil {
		if errors.Is(err, secrets.ErrGenericSecretNotFound) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	if err := target.Delete(r.Context(), rec.ID); err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- helpers -------------------------------------------------------------

func normalizeLocalScope(scope string) string {
	if strings.EqualFold(strings.TrimSpace(scope), "project") {
		return "project"
	}
	return "global"
}

// localFindByID locates a secret by ID across layers, returning the concrete
// owning file store and its scope so update/delete target the right layer.
func (s *Server) localFindByID(r *http.Request, id string) (secrets.GenericSecret, *secrets.FileGenericSecretStore, string, error) {
	if proj, active := s.localSecrets.Project(); active {
		if rec, err := proj.Get(r.Context(), id); err == nil {
			return rec, proj, "project", nil
		}
	}
	rec, err := s.localSecrets.Global().Get(r.Context(), id)
	return rec, s.localSecrets.Global(), "global", err
}

func (s *Server) writeLocalSecretLookupError(w http.ResponseWriter, err error) {
	if errors.Is(err, secrets.ErrGenericSecretNotFound) {
		httpError(w, http.StatusNotFound, "secret not found")
		return
	}
	httpError(w, http.StatusInternalServerError, "%s", err.Error())
}

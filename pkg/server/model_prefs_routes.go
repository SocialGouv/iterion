package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/modelprefs"
)

// The model preference for a long-lived surface — the assistant session being
// the one that motivated it, whose model was pinned in the .bot and overridable
// only by a server-start environment variable.
//
// The engine stays bot-agnostic: `key` is an OPAQUE scope string the caller
// chooses (the studio passes a bot id) and nothing here interprets it, so a
// second conversational bot needs no engine change.

type modelPrefRequest struct {
	// Key scopes the preference. Opaque to the server.
	Key string `json:"key"`
	// Model/Backend/Effort are the chosen dimensions. An empty one means "use
	// the bot's own default for that dimension".
	Model   string `json:"model,omitempty"`
	Backend string `json:"backend,omitempty"`
	Effort  string `json:"effort,omitempty"`
}

type modelPrefResponse struct {
	Key     string `json:"key"`
	Model   string `json:"model,omitempty"`
	Backend string `json:"backend,omitempty"`
	Effort  string `json:"effort,omitempty"`
	// Set distinguishes "no preference recorded" (fall back to the bot's
	// defaults) from "recorded, and it happens to be empty". A bare object of
	// empty strings cannot say which.
	Set bool `json:"set"`
}

func (s *Server) registerModelPrefRoutes() {
	s.mux.Handle("GET /api/v1/preferences/model", s.requireAuth(http.HandlerFunc(s.handleGetModelPref)))
	s.mux.Handle("PUT /api/v1/preferences/model", s.requireAuth(http.HandlerFunc(s.handlePutModelPref)))
	s.mux.Handle("DELETE /api/v1/preferences/model", s.requireAuth(http.HandlerFunc(s.handleDeleteModelPref)))
}

// modelPrefScope resolves the (tenant, user) the preference belongs to. In
// local mode both are empty — a single operator — which the stores key on
// verbatim.
func modelPrefScope(r *http.Request) (tenantID, userID string) {
	id, _ := auth.FromContext(r.Context())
	return id.TeamID, id.UserID
}

func (s *Server) handleGetModelPref(w http.ResponseWriter, r *http.Request) {
	if s.cfg.ModelPrefs == nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "model preferences are not available on this server")
		return
	}
	key, err := modelprefs.NormalizeKey(r.URL.Query().Get("key"))
	if err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "%v", err)
		return
	}
	tenantID, userID := modelPrefScope(r)
	p, err := s.cfg.ModelPrefs.Get(r.Context(), tenantID, userID, key)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "load model preference: %v", err)
		return
	}
	if p == nil {
		s.writeJSONFor(w, r, modelPrefResponse{Key: key})
		return
	}
	s.writeJSONFor(w, r, modelPrefResponse{
		Key: key, Model: p.Model, Backend: p.Backend, Effort: p.Effort, Set: true,
	})
}

func (s *Server) handlePutModelPref(w http.ResponseWriter, r *http.Request) {
	if s.cfg.ModelPrefs == nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "model preferences are not available on this server")
		return
	}
	var req modelPrefRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid preference payload: %v", err)
		return
	}
	key, err := modelprefs.NormalizeKey(req.Key)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "%v", err)
		return
	}
	// Same reason the launch endpoint validates it: the effort reaches the
	// provider verbatim, and a preference is re-applied on EVERY future
	// session — a typo stored here would keep breaking runs long after the
	// operator forgot they typed it.
	if req.Effort != "" && !ir.ValidReasoningEfforts[req.Effort] {
		s.httpErrorFor(w, r, http.StatusBadRequest,
			"%q is not a reasoning effort (valid: %s)", req.Effort, strings.Join(validEffortNames(), ", "))
		return
	}
	if req.Backend != "" && !validModelBackendNames[req.Backend] {
		s.httpErrorFor(w, r, http.StatusBadRequest,
			"%q is not a backend (valid: %s)", req.Backend, strings.Join(validBackendNames(), ", "))
		return
	}
	tenantID, userID := modelPrefScope(r)
	if err := s.cfg.ModelPrefs.Set(r.Context(), &modelprefs.Pref{
		TenantID: tenantID, UserID: userID, Key: key,
		Model: req.Model, Backend: req.Backend, Effort: req.Effort,
	}); err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "save model preference: %v", err)
		return
	}
	s.writeJSONFor(w, r, modelPrefResponse{
		Key: key, Model: req.Model, Backend: req.Backend, Effort: req.Effort, Set: true,
	})
}

func (s *Server) handleDeleteModelPref(w http.ResponseWriter, r *http.Request) {
	if s.cfg.ModelPrefs == nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "model preferences are not available on this server")
		return
	}
	key, err := modelprefs.NormalizeKey(r.URL.Query().Get("key"))
	if err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "%v", err)
		return
	}
	tenantID, userID := modelPrefScope(r)
	if err := s.cfg.ModelPrefs.Delete(r.Context(), tenantID, userID, key); err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "clear model preference: %v", err)
		return
	}
	s.writeJSONFor(w, r, modelPrefResponse{Key: key})
}

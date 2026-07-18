package server

import (
	"encoding/json"
	"net/http"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/configshare"
)

// registerConfigEditorRoutes wires the AUTHENTICATED (session) config-editor
// surface (ADR-078): a real user holding the config_editor capability on a team
// — or a team admin — edits the team's config-shares WITHOUT an iws_ token,
// through the normal cookie session. Distinct from the public capability-URL
// routes (registerConfigSharePublicRoutes), which keep their Bearer-only,
// cookie-less, CSRF-immune contract untouched. Same ProjectedRead/ApplyEdit
// service, so the projection + fail-closed allow-list are identical.
func (s *Server) registerConfigEditorRoutes() {
	s.mux.Handle("GET /api/teams/{id}/config-editor/shares", s.requireAuth(http.HandlerFunc(s.handleConfigEditorList)))
	s.mux.Handle("GET /api/teams/{id}/config-editor/shares/{sid}/config", s.requireAuth(http.HandlerFunc(s.handleConfigEditorGet)))
	s.mux.Handle("PATCH /api/teams/{id}/config-editor/shares/{sid}/config", s.requireAuth(http.HandlerFunc(s.handleConfigEditorPatch)))
}

// editorShareView is the REDUCED projection a config-editor sees — enough to
// render the editor menu, never the token metadata / audit surface the operator
// shareView carries (token_last4, fingerprint, deliveries).
func editorShareView(sh *configshare.Share) map[string]any {
	return map[string]any{
		"id": sh.ID, "bot_id": sh.BotID, "label": sh.Label,
		"category": sh.Category, "config_path": sh.ConfigPath, "read_only": sh.ReadOnly,
	}
}

// loadEditableShare gates on canEditConfigShares and loads the share, verifying
// it belongs to the path team.
func (s *Server) loadEditableShare(w http.ResponseWriter, r *http.Request) (*configshare.Share, bool) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canEditConfigShares(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "forbidden")
		return nil, false
	}
	sh, err := s.configShares.GetByID(r.Context(), r.PathValue("sid"))
	if err != nil || sh == nil || sh.TenantID != teamID || !sh.Enabled {
		httpError(w, http.StatusNotFound, "not found")
		return nil, false
	}
	return sh, true
}

func (s *Server) handleConfigEditorList(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canEditConfigShares(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "forbidden")
		return
	}
	rows, err := s.configShares.ListByTenant(r.Context(), teamID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	views := make([]map[string]any, 0, len(rows))
	for _, sh := range rows {
		if !sh.Enabled {
			continue
		}
		views = append(views, editorShareView(sh))
	}
	writeJSON(w, map[string]any{"shares": views})
}

func (s *Server) handleConfigEditorGet(w http.ResponseWriter, r *http.Request) {
	sh, ok := s.loadEditableShare(w, r)
	if !ok {
		return
	}
	fc, err := s.resolveShareFC(r.Context(), sh)
	if err != nil {
		httpError(w, http.StatusBadGateway, "config source unavailable")
		return
	}
	proj, sha, err := s.configShareSvc.ProjectedRead(r.Context(), fc, sh)
	if err != nil {
		httpError(w, http.StatusBadGateway, "read failed")
		return
	}
	writeJSON(w, map[string]any{
		"config": proj, "sha": sha,
		"bot_id": sh.BotID, "label": sh.Label, "category": sh.Category,
		"config_path": sh.ConfigPath, "read_only": sh.ReadOnly,
	})
}

func (s *Server) handleConfigEditorPatch(w http.ResponseWriter, r *http.Request) {
	sh, ok := s.loadEditableShare(w, r)
	if !ok {
		return
	}
	if sh.ReadOnly {
		httpError(w, http.StatusForbidden, "read_only")
		return
	}
	var req configSharePatchReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 128<<10)).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if len(req.Patch) == 0 {
		httpError(w, http.StatusBadRequest, "patch has no editable field")
		return
	}
	if req.SHA == "" {
		httpError(w, http.StatusBadRequest, "sha required")
		return
	}
	msg := "chore(config-share): edit " + sh.ConfigPath + " via config-editor"
	s.applyShareEditAndRespond(w, r, sh, req, msg)
}

package server

import (
	"net/http"
	"strings"

	"github.com/SocialGouv/iterion/pkg/skilllib"
)

// registerLocalSkillRoutes wires the local (non-cloud) skill-library management
// surface: an editable CRUD store of Claude-Code-style SKILL.md skills,
// referenceable from workflows via the DSL `skills:` field. Like the local
// secret + plugin routes these are unauthenticated single-operator routes for
// the desktop / local studio. Gated in server_routes.go on local mode.
//
// Skills are keyed by name (a single path segment), not an opaque id — the name
// is the DSL reference and the on-disk directory. No sealing: a skill is public
// guidance text, not a secret.
func (s *Server) registerLocalSkillRoutes() {
	s.mux.HandleFunc("GET /api/local/skills", s.handleListLocalSkills)
	s.mux.HandleFunc("POST /api/local/skills", s.handleCreateLocalSkill)
	s.mux.HandleFunc("GET /api/local/skills/{name}", s.handleGetLocalSkill)
	s.mux.HandleFunc("PUT /api/local/skills/{name}", s.handleUpdateLocalSkill)
	s.mux.HandleFunc("DELETE /api/local/skills/{name}", s.handleDeleteLocalSkill)
}

type localSkillReq struct {
	Name  string `json:"name,omitempty"` // create only; path {name} wins on update
	Body  string `json:"body"`
	Scope string `json:"scope,omitempty"` // "global" (default) | "project"
}

// localSkillStore builds the layered skill library from the server's current
// store dir under stateMu (a concurrent project switch rebuilds cfg.StoreDir).
func (s *Server) localSkillStore() *skilllib.Store {
	s.stateMu.RLock()
	storeDir := s.cfg.StoreDir
	s.stateMu.RUnlock()
	return skilllib.LocalStoreForProject(storeDir)
}

func (s *Server) handleListLocalSkills(w http.ResponseWriter, r *http.Request) {
	skills, err := s.localSkillStore().List()
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	if skills == nil {
		skills = []skilllib.LibrarySkill{}
	}
	writeJSON(w, struct {
		Skills []skilllib.LibrarySkill `json:"skills"`
	}{Skills: skills})
}

func (s *Server) handleGetLocalSkill(w http.ResponseWriter, r *http.Request) {
	sk, err := s.localSkillStore().Get(r.PathValue("name"))
	if err != nil {
		httpError(w, http.StatusNotFound, "skill not found")
		return
	}
	writeJSON(w, sk)
}

func (s *Server) handleCreateLocalSkill(w http.ResponseWriter, r *http.Request) {
	var req localSkillReq
	if !decodeJSON(w, r, &req) {
		return
	}
	s.putLocalSkill(w, strings.TrimSpace(req.Name), req)
}

func (s *Server) handleUpdateLocalSkill(w http.ResponseWriter, r *http.Request) {
	var req localSkillReq
	if !decodeJSON(w, r, &req) {
		return
	}
	s.putLocalSkill(w, r.PathValue("name"), req)
}

// putLocalSkill creates or overwrites a skill, resolving "project" scope down to
// "global" when no project layer is active so the response matches where the
// skill actually landed.
func (s *Server) putLocalSkill(w http.ResponseWriter, name string, req localSkillReq) {
	if err := skilllib.ValidName(name); err != nil {
		httpError(w, http.StatusBadRequest, "%s", err.Error())
		return
	}
	if strings.TrimSpace(req.Body) == "" {
		httpError(w, http.StatusBadRequest, "skill body required")
		return
	}
	st := s.localSkillStore()
	scope := normalizeLocalSkillScope(req.Scope)
	if scope == skilllib.ScopeProject && !st.HasProject() {
		scope = skilllib.ScopeGlobal
	}
	if err := st.Put(name, req.Body, scope); err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	sk, err := st.Get(name)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	writeJSON(w, sk)
}

func (s *Server) handleDeleteLocalSkill(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := skilllib.ValidName(name); err != nil {
		httpError(w, http.StatusBadRequest, "%s", err.Error())
		return
	}
	st := s.localSkillStore()
	scope := normalizeLocalSkillScope(r.URL.Query().Get("scope"))
	if scope == skilllib.ScopeProject && !st.HasProject() {
		scope = skilllib.ScopeGlobal
	}
	if err := st.Remove(name, scope); err != nil {
		// A missing skill deletes idempotently (204) rather than erroring.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func normalizeLocalSkillScope(scope string) string {
	if strings.EqualFold(strings.TrimSpace(scope), skilllib.ScopeProject) {
		return skilllib.ScopeProject
	}
	return skilllib.ScopeGlobal
}

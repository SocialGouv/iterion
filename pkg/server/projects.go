package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/server/projects"
	"github.com/SocialGouv/iterion/pkg/store"
)

// projectsEnabled reports whether project-registry endpoints make
// sense for this server's run mode. Cloud (multi-tenant) mode has no
// per-folder concept — the SPA short-circuits the switcher there.
func (s *Server) projectsEnabled() bool {
	return s.cfg.Mode != "cloud"
}

// registerInitialProject is called once at server boot to make sure
// the workdir the operator launched against is present in the shared
// registry (so it shows up in the switcher even when they've never
// used the desktop app). Mirrors AddProjectSilently in the desktop
// bindings: it does NOT trigger a hot-swap or broadcast.
func (s *Server) registerInitialProject() {
	if !s.projectsEnabled() || s.cfg.WorkDir == "" {
		return
	}
	abs, err := filepath.Abs(s.cfg.WorkDir)
	if err != nil {
		s.logger.Warn("projects: abs workdir %q: %v", s.cfg.WorkDir, err)
		return
	}
	cfg, err := projects.Load()
	if err != nil {
		s.logger.Warn("projects: load registry: %v", err)
		return
	}
	cfg.AddOrTouch(abs)
	if err := cfg.Save(); err != nil {
		s.logger.Warn("projects: save registry: %v", err)
	}
	s.cacheCurrentProjectID(cfg.CurrentProjectID)
}

// cacheCurrentProjectID stashes the latest current_project_id so
// handleServerInfo can surface it without touching disk on every poll.
// Safe to call under any lock state — it acquires stateMu itself.
func (s *Server) cacheCurrentProjectID(id string) {
	s.stateMu.Lock()
	s.currentProjectID = id
	s.stateMu.Unlock()
}

// CurrentProjectID returns the cached id of the active registry entry.
// Empty when the registry has never been written or the server is in
// cloud mode.
func (s *Server) CurrentProjectID() string {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	return s.currentProjectID
}

// projectResponse is the wire shape returned to the SPA. Mirrors
// editor/src/api/projects.ts.
type projectResponse struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Dir        string `json:"dir"`
	StoreDir   string `json:"store_dir,omitempty"`
	LastOpened string `json:"last_opened"`
	Color      string `json:"color,omitempty"`
}

func toProjectResponse(p projects.Project) projectResponse {
	return projectResponse{
		ID:         p.ID,
		Name:       p.Name,
		Dir:        p.Dir,
		StoreDir:   p.StoreDir,
		LastOpened: p.LastOpened.UTC().Format("2006-01-02T15:04:05.000Z"),
		Color:      p.Color,
	}
}

func toProjectResponses(ps []projects.Project) []projectResponse {
	out := make([]projectResponse, len(ps))
	for i, p := range ps {
		out[i] = toProjectResponse(p)
	}
	return out
}

func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	if !s.projectsEnabled() {
		s.writeJSONFor(w, r, []projectResponse{})
		return
	}
	cfg, err := projects.Load()
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "load projects: %v", err)
		return
	}
	s.writeJSONFor(w, r, toProjectResponses(cfg.RecentProjects))
}

func (s *Server) handleCurrentProject(w http.ResponseWriter, r *http.Request) {
	if !s.projectsEnabled() {
		s.writeJSONFor(w, r, nil)
		return
	}
	cfg, err := projects.Load()
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "load projects: %v", err)
		return
	}
	cur := cfg.Current()
	if cur == nil {
		s.writeJSONFor(w, r, nil)
		return
	}
	s.writeJSONFor(w, r, toProjectResponse(*cur))
}

type switchProjectRequest struct {
	ID string `json:"id"`
}

func (s *Server) handleSwitchProject(w http.ResponseWriter, r *http.Request) {
	if !s.projectsEnabled() {
		s.httpErrorFor(w, r, http.StatusBadRequest, "projects switching is not available in cloud mode")
		return
	}
	var req switchProjectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid body: %v", err)
		return
	}
	if req.ID == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "id is required")
		return
	}
	cfg, err := projects.Load()
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "load projects: %v", err)
		return
	}
	target := cfg.ByID(req.ID)
	if target == nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "project not found: %s", req.ID)
		return
	}
	if !cfg.SetCurrent(req.ID) {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "set current: race")
		return
	}
	if err := cfg.Save(); err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "save projects: %v", err)
		return
	}
	if err := s.swapWorkDir(r.Context(), target.Dir); err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "swap workdir: %v", err)
		return
	}
	s.cacheCurrentProjectID(cfg.CurrentProjectID)
	s.broadcastProjectSwitched(*target)
	s.writeJSONFor(w, r, toProjectResponse(*target))
}

type addProjectRequest struct {
	Dir string `json:"dir"`
}

func (s *Server) handleAddProject(w http.ResponseWriter, r *http.Request) {
	if !s.projectsEnabled() {
		s.httpErrorFor(w, r, http.StatusBadRequest, "projects are not available in cloud mode")
		return
	}
	var req addProjectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid body: %v", err)
		return
	}
	if req.Dir == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "dir is required")
		return
	}
	// swapWorkDir owns the abs + stat + IsDir validation; on its
	// failure we don't want to leak a half-registered project, so we
	// run the swap first, then commit the registry change.
	abs, err := filepath.Abs(req.Dir)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "abs path: %v", err)
		return
	}
	if err := s.swapWorkDir(r.Context(), abs); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "%v", err)
		return
	}
	cfg, err := projects.Load()
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "load projects: %v", err)
		return
	}
	p := cfg.AddOrTouch(abs)
	if err := cfg.Save(); err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "save projects: %v", err)
		return
	}
	s.cacheCurrentProjectID(cfg.CurrentProjectID)
	s.broadcastProjectSwitched(p)
	s.writeJSONFor(w, r, toProjectResponse(p))
}

func (s *Server) handleRemoveProject(w http.ResponseWriter, r *http.Request) {
	if !s.projectsEnabled() {
		s.httpErrorFor(w, r, http.StatusBadRequest, "projects are not available in cloud mode")
		return
	}
	id := r.PathValue("id")
	if id == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "id is required")
		return
	}
	cfg, err := projects.Load()
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "load projects: %v", err)
		return
	}
	wasCurrent := cfg.CurrentProjectID == id
	if !cfg.Remove(id) {
		s.httpErrorFor(w, r, http.StatusNotFound, "project not found: %s", id)
		return
	}
	if err := cfg.Save(); err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "save projects: %v", err)
		return
	}
	// If the removed project was current, hot-swap to the new MRU
	// head so the operator doesn't end up viewing a directory that's
	// no longer in the registry.
	if wasCurrent {
		if next := cfg.Current(); next != nil {
			if err := s.swapWorkDir(r.Context(), next.Dir); err != nil {
				s.httpErrorFor(w, r, http.StatusInternalServerError, "swap workdir: %v", err)
				return
			}
			s.cacheCurrentProjectID(cfg.CurrentProjectID)
			s.broadcastProjectSwitched(*next)
		} else {
			s.cacheCurrentProjectID("")
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// swapWorkDir is the hot-swap primitive. Builds a fresh runview.Service
// and Watcher for the new directory, then atomically replaces the
// server's references under stateMu. In-flight engine goroutines from
// the previous project keep their captured *Service reference and
// drain to the old store in the background — the SPA's reset on
// `project_switched` makes that invisible to the user.
func (s *Server) swapWorkDir(_ context.Context, newDir string) error {
	abs, err := filepath.Abs(newDir)
	if err != nil {
		return fmt.Errorf("abs: %w", err)
	}
	info, err := statDir(abs)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("not a directory: %s", abs)
	}

	// Build the new run-console service first; if construction fails
	// we abort before touching any live state.
	storeDir := store.ResolveStoreDir(abs, "")
	var newRuns *runview.Service
	if storeDir != "" {
		svcOpts := []runview.ServiceOption{
			runview.WithLogger(s.logger),
			runview.WithWorkDir(abs),
		}
		svc, svcErr := runview.NewService(storeDir, svcOpts...)
		if svcErr != nil {
			return fmt.Errorf("runview service: %w", svcErr)
		}
		newRuns = svc
	}

	// Build the new watcher. Best-effort: a NewWatcher failure is
	// downgraded to a warning (same as the boot path); the swap goes
	// through without file-event push.
	var newWatcher *Watcher
	if s.cfg.Mode != "cloud" {
		w, werr := NewWatcher(abs, s.hub, s.logger)
		if werr != nil {
			s.logger.Warn("projects: new watcher for %q: %v", abs, werr)
		} else {
			newWatcher = w
		}
	}

	// Swap under the write lock. Capture the previous watcher so we
	// can stop it after releasing the lock; the previous Service is
	// intentionally NOT drained — its captured engine goroutines
	// continue to write to their original store.
	s.stateMu.Lock()
	oldWatcher := s.watcher
	s.cfg.WorkDir = abs
	s.cfg.StoreDir = storeDir
	s.runs = newRuns
	s.watcher = newWatcher
	s.stateMu.Unlock()

	if oldWatcher != nil {
		oldWatcher.Stop()
	}
	if newWatcher != nil {
		go newWatcher.Start()
	}
	s.logger.Info("projects: swapped workdir to %s", abs)
	return nil
}

// broadcastProjectSwitched pushes a `project_switched` envelope onto
// the Hub's broadcast channel directly — we're in the same package so
// the unexported channel is reachable, and we don't widen the Hub API
// with a second event type the rest of the codebase doesn't need.
// SPA-side discriminator: the JSON `type` field, same as FileEvent.
func (s *Server) broadcastProjectSwitched(p projects.Project) {
	if s.hub == nil {
		return
	}
	type payload struct {
		Type    string          `json:"type"`
		Current projectResponse `json:"current"`
	}
	data, err := json.Marshal(payload{
		Type:    "project_switched",
		Current: toProjectResponse(p),
	})
	if err != nil {
		s.logger.Error("projects: marshal project_switched: %v", err)
		return
	}
	select {
	case s.hub.broadcast <- data:
	case <-s.hub.done:
	}
}

// statDir returns the FileInfo for an absolute directory path,
// translating ENOENT and permission errors into user-readable
// messages. Used by the add/switch handlers to fail fast before any
// state mutation.
func statDir(abs string) (os.FileInfo, error) {
	st, statErr := os.Stat(abs)
	if statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			return nil, fmt.Errorf("path does not exist: %s", abs)
		}
		if errors.Is(statErr, os.ErrPermission) {
			return nil, fmt.Errorf("permission denied: %s", abs)
		}
		return nil, fmt.Errorf("stat %s: %w", abs, statErr)
	}
	return st, nil
}

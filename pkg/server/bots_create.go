package server

import (
	"net/http"
	"os"
	"path/filepath"

	"github.com/SocialGouv/iterion/internal/httpx"
	"github.com/SocialGouv/iterion/pkg/botregistry"
	"github.com/SocialGouv/iterion/pkg/botscaffold"
)

// handleBotTemplates serves the builder's "start from a template"
// gallery. Static, embedded catalog — safe to expose wherever the bot
// list is.
func (s *Server) handleBotTemplates(w http.ResponseWriter, r *http.Request) {
	s.writeJSONFor(w, r, map[string]any{"templates": botscaffold.Templates()})
}

// handleBotCreate scaffolds a new bot bundle into the workspace's bots/
// directory from the studio builder's Spec. Local-mode only: the bundle
// lands on the operator's filesystem (cloud workspaces author bots
// through git, not this endpoint).
func (s *Server) handleBotCreate(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	if s.cfg.Mode == "cloud" {
		s.httpErrorFor(w, r, http.StatusForbidden, "bots: create is a local-mode operation")
		return
	}
	if s.cfg.WorkDir == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "bots: server has no workdir to create the bot in")
		return
	}
	var spec botscaffold.Spec
	if err := readJSON(r, &spec); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid request: %v", err)
		return
	}
	// Validate normalizes the spec (slug trimming included) in place.
	if err := spec.Validate(); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "%v", err)
		return
	}

	// Collision check against BOTH the discovery registry (a bot of that
	// name may live in any configured path) and the target directory.
	if _, exists, err := s.findBot(spec.Slug); err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "bots: %v", err)
		return
	} else if exists {
		s.httpErrorFor(w, r, http.StatusConflict, "bots: %q already exists", spec.Slug)
		return
	}
	dir := filepath.Join(s.cfg.WorkDir, botregistry.BotsDirName, spec.Slug)
	if _, err := os.Stat(dir); err == nil {
		s.httpErrorFor(w, r, http.StatusConflict, "bots: %s already exists on disk", dir)
		return
	}

	if _, err := botscaffold.Scaffold(dir, spec); err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "bots: scaffold: %v", err)
		return
	}
	s.regenCatalog(spec.Slug, "create")

	entry, ok, err := s.findBot(spec.Slug)
	if err != nil || !ok {
		s.httpErrorFor(w, r, http.StatusInternalServerError,
			"bots: created %s but discovery does not see it (err=%v)", dir, err)
		return
	}
	s.reflectAllowedOrigin(w, r)
	httpx.WriteJSON(w, http.StatusCreated, entry)
}

package server

import (
	"net/http"
	"strings"

	"github.com/SocialGouv/iterion/pkg/botregistry"
)

// BotsConfig configures the bot registry exposed at /api/v1/bots.
type BotsConfig struct {
	// Paths are walked to discover bots. Each entry may be a directory
	// (walked recursively for .bot/.iter files and bundle directories)
	// or a single .bot/.iter file. Missing paths are skipped silently.
	// When empty the resolver falls back to <WorkDir>/bots,
	// <WorkDir>/examples, <WorkDir>/.botz — the conventional locations.
	Paths []string
}

// effectivePaths returns the configured bot paths, falling back to the
// project-relative conventions when none are configured.
func (s *Server) effectivePaths() []string {
	if len(s.cfg.Bots.Paths) > 0 {
		return s.cfg.Bots.Paths
	}
	if s.cfg.WorkDir == "" {
		return nil
	}
	return []string{
		s.cfg.WorkDir + "/bots",
		s.cfg.WorkDir + "/examples",
		s.cfg.WorkDir + "/.botz",
	}
}

// handleBotsList returns the discovered bots' metadata + vars schema.
// The list payload always includes schemas — the studio's bot picker
// renders the typed form inline on selection, so a separate "lite"
// endpoint would just double the request count.
func (s *Server) handleBotsList(w http.ResponseWriter, r *http.Request) {
	paths := s.effectivePaths()
	if len(paths) == 0 {
		s.writeJSONFor(w, r, map[string]any{"bots": []any{}})
		return
	}
	entries, err := botregistry.ListWithSchema(botregistry.ListOptions{Paths: paths})
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "bots: %v", err)
		return
	}
	s.writeJSONFor(w, r, map[string]any{"bots": entries})
}

// handleBotsGet returns one bot with its full schema. Returns 404 when
// the bot name doesn't match any discovered entry.
func (s *Server) handleBotsGet(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "bots: missing name")
		return
	}
	paths := s.effectivePaths()
	entries, err := botregistry.ListWithSchema(botregistry.ListOptions{Paths: paths})
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "bots: %v", err)
		return
	}
	for _, e := range entries {
		if e.Name == name {
			s.writeJSONFor(w, r, e)
			return
		}
	}
	s.httpErrorFor(w, r, http.StatusNotFound, "bots: %q not found", name)
}

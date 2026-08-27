package server

import (
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"

	"github.com/SocialGouv/iterion/internal/httpx"
	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/botregistry"
	"github.com/SocialGouv/iterion/pkg/botscaffold"
	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/store"
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
		s.handleBotCreateCloud(w, r)
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
	if err := botregistry.EnsureNameFree(s.botListOptions(), spec.Slug); err != nil {
		if errors.Is(err, botregistry.ErrNameTaken) {
			s.httpErrorFor(w, r, http.StatusConflict, "%v", err)
			return
		}
		s.httpErrorFor(w, r, http.StatusInternalServerError, "bots: %v", err)
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

// handleBotCreateCloud is the cloud counterpart of handleBotCreate: it scaffolds
// the same botscaffold.Spec, but instead of writing files onto a workspace the
// cloud pod does not have, it materializes the bundle to a temp dir, reads it
// into a files map, and persists it to the team-authored bot store (pkg/botsource).
// The team then edits it in the studio editor exactly like a forked bot.
func (s *Server) handleBotCreateCloud(w http.ResponseWriter, r *http.Request) {
	if s.botSources == nil {
		s.httpErrorFor(w, r, http.StatusForbidden, "bots: bot editing is not enabled on this server")
		return
	}
	id, _ := auth.FromContext(r.Context())
	teamID := id.TeamID
	if teamID == "" || !s.canEditBots(r.Context(), id, teamID) {
		s.httpErrorFor(w, r, http.StatusForbidden, "bot editor, team admin, or owner required")
		return
	}
	var spec botscaffold.Spec
	if err := readJSON(r, &spec); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid request: %v", err)
		return
	}
	if err := spec.Validate(); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "%v", err)
		return
	}
	tmp, err := os.MkdirTemp("", "iterion-bot-create-*")
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "bots: scaffold: %v", err)
		return
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	dir := filepath.Join(tmp, spec.Slug)
	if _, err := botscaffold.Scaffold(dir, spec); err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "bots: scaffold: %v", err)
		return
	}
	files, err := readAllBundleFiles(dir)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "bots: read scaffold: %v", err)
		return
	}
	bs := botsource.BotSource{TenantID: teamID, Slug: spec.Slug, Files: files, CreatedBy: id.UserID}
	if _, err := s.botSources.Create(store.WithTenant(r.Context(), teamID), bs); err != nil {
		s.botSourceError(w, r, err)
		return
	}
	// Return the discovered entry (metadata + schema), marked editable, so the
	// builder's create → test loop can bind to it.
	for _, e := range s.tenantBotEntries(r.Context()) {
		if e.Name == spec.Slug {
			s.reflectAllowedOrigin(w, r)
			httpx.WriteJSON(w, http.StatusCreated, botEntryView{EntryWithSchema: e, Editable: true, Origin: "tenant"})
			return
		}
	}
	s.httpErrorFor(w, r, http.StatusInternalServerError, "bots: created %q but discovery does not see it", spec.Slug)
}

// readAllBundleFiles walks a bundle directory into a slash-keyed files map,
// skipping any .git tree. Used to lift a freshly scaffolded bundle off a temp
// dir into the tenant store.
func readAllBundleFiles(dir string) (map[string]string, error) {
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, rerr := filepath.Rel(dir, p)
		if rerr != nil {
			return rerr
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		files[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

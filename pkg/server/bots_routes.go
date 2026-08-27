package server

import (
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/SocialGouv/iterion/pkg/botinstall"
	"github.com/SocialGouv/iterion/pkg/botregistry"
	"github.com/SocialGouv/iterion/pkg/bundle"
)

// BotsConfig configures the bot registry exposed at /api/v1/bots. It
// is an alias of botregistry.Config so the server and dispatcher
// share one type for "where to look for bots".
type BotsConfig = botregistry.Config

// currentWorkDir snapshots cfg.WorkDir under the state lock — it is
// hot-swapped on a studio project switch (swapWorkDir).
func (s *Server) currentWorkDir() string {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	return s.cfg.WorkDir
}

// effectivePaths returns the configured bot paths, falling back to the
// project-relative conventions when none are configured. Only an
// explicit --bots-path pins the catalog; the default derives from the
// LIVE WorkDir on every call so bot discovery follows a project switch.
func (s *Server) effectivePaths() []string {
	return s.effectivePathsFor(s.currentWorkDir())
}

func (s *Server) effectivePathsFor(workDir string) []string {
	if len(s.cfg.Bots.Paths) > 0 {
		return s.cfg.Bots.Paths
	}
	if workDir == "" {
		return nil
	}
	return botregistry.DefaultPaths(workDir)
}

// botListOptions builds the discovery options for the bot endpoints,
// passing WorkDir so each Entry.Enabled reflects the workspace overlay
// (.iterion/bot-overrides.yaml) composed over the manifest default.
// Paths and Workdir derive from ONE WorkDir snapshot: two reads could
// straddle a concurrent project switch and compose one project's bots
// with the other's enabled-overlay.
func (s *Server) botListOptions() botregistry.ListOptions {
	workDir := s.currentWorkDir()
	return botregistry.ListOptions{Paths: s.effectivePathsFor(workDir), Workdir: workDir}
}

// handleBotsList returns the discovered bots' metadata + vars schema.
// The list payload always includes schemas — the studio's bot picker
// renders the typed form inline on selection, so a separate "lite"
// endpoint would just double the request count. Disabled bots are
// included (Enabled=false) so the studio can show them to flip back on.
// discovery_errors carries one entry per skipped malformed bundle: a bad
// manifest must not blank the gallery, but it must not vanish silently
// either.
func (s *Server) handleBotsList(w http.ResponseWriter, r *http.Request) {
	// The catalog can be empty (cloud with no baked path, or a bare workspace)
	// while the team still has authored bots — so don't short-circuit on
	// effectivePaths; mergedBotEntries returns [] catalog + the tenant bots.
	entries, diags, err := s.mergedBotEntries(r.Context())
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "bots: %v", err)
		return
	}
	resp := map[string]any{"bots": entries}
	if len(diags) > 0 {
		resp["discovery_errors"] = diags
	}
	s.writeJSONFor(w, r, resp)
}

// handleBotsGet returns one bot with its full schema. Returns 404 when
// the bot name doesn't match any discovered entry.
func (s *Server) handleBotsGet(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "bots: missing name")
		return
	}
	// A team-authored bot of this name overrides the catalog (same precedence
	// as the list + launch paths). Return it marked editable.
	for _, e := range s.tenantBotEntries(r.Context()) {
		if e.Name == name {
			s.writeJSONFor(w, r, botEntryView{EntryWithSchema: e, Editable: true, Origin: "tenant"})
			return
		}
	}
	entry, ok, err := s.findBot(name)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "bots: %v", err)
		return
	}
	if !ok {
		s.httpErrorFor(w, r, http.StatusNotFound, "bots: %q not found", name)
		return
	}
	s.writeJSONFor(w, r, botEntryView{EntryWithSchema: entry, Editable: false, Origin: "catalog"})
}

// botUpdateRequest is the wire body for PUT /api/v1/bots/{name}. Pointer
// fields distinguish "omitted (no change)" from "set to empty string".
// The bot's technical name is the URL pathvar and is NOT renamable here
// (rename cascades through the catalog, dispatcher routing, and ticket
// history — out of scope).
type botUpdateRequest struct {
	DisplayName *string   `json:"display_name,omitempty"`
	Icon        *string   `json:"icon,omitempty"`
	Description *string   `json:"description,omitempty"`
	Author      *string   `json:"author,omitempty"`
	Version     *string   `json:"version,omitempty"`
	WhenToUse   *string   `json:"when_to_use,omitempty"`
	Enabled     *bool     `json:"enabled,omitempty"`
	Triggers    *[]string `json:"triggers,omitempty"`
}

// handleBotsPut updates a bot's manifest.yaml in place (the studio Bot
// metadata panel). Bundle-only: a loose .bot file has no manifest to
// write, so it returns 409. On success it regenerates the orchestrator
// catalog so the edit reaches Nexie, then returns the refreshed entry.
func (s *Server) handleBotsPut(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "bots: missing name")
		return
	}
	entry, ok, err := s.findBot(name)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "bots: %v", err)
		return
	}
	if !ok {
		s.httpErrorFor(w, r, http.StatusNotFound, "bots: %q not found", name)
		return
	}
	if !entry.IsBundleDir {
		s.httpErrorFor(w, r, http.StatusConflict,
			"bots: %q is a loose .bot file; convert it to a bundle (manifest.yaml + main.bot) to edit metadata", name)
		return
	}
	var req botUpdateRequest
	if err := readJSON(r, &req); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid request: %v", err)
		return
	}

	manifestPath := filepath.Join(entry.Path, "manifest.yaml")
	patch := bundle.ManifestPatch{
		DisplayName: req.DisplayName,
		Icon:        req.Icon,
		Description: req.Description,
		Author:      req.Author,
		Version:     req.Version,
		WhenToUse:   req.WhenToUse,
		Enabled:     req.Enabled,
		Triggers:    req.Triggers,
	}
	if _, err := bundle.WriteManifest(manifestPath, patch); err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "bots: write manifest: %v", err)
		return
	}
	s.regenCatalog(name, "update")
	s.respondBot(w, r, name)
}

// botOverlayRequest is the wire body for PUT /api/v1/bots/{name}/overlay.
// A null/omitted enabled clears the workspace override (the manifest
// default stands again); true/false pins the bot on/off for this
// workspace without editing the (possibly git-tracked) manifest.
type botOverlayRequest struct {
	Enabled *bool `json:"enabled"`
}

// handleBotOverlay sets a bot's workspace-local catalog-visibility
// override (the studio Catalog manager quick-toggle), then regenerates
// the catalog and returns the refreshed entry.
func (s *Server) handleBotOverlay(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "bots: missing name")
		return
	}
	if s.cfg.WorkDir == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "bots: no workspace configured for the catalog overlay")
		return
	}
	var req botOverlayRequest
	if err := readJSON(r, &req); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid request: %v", err)
		return
	}
	if err := botregistry.SetOverlayEnabled(s.cfg.WorkDir, name, req.Enabled); err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "bots: overlay: %v", err)
		return
	}
	s.regenCatalog(name, "overlay")
	// respondBot re-resolves the entry; an unknown name 404s here (after a
	// harmless no-op overlay write — ResolveEnabled ignores unknown names).
	s.respondBot(w, r, name)
}

// findBot returns the schema-augmented entry for name (exact match).
func (s *Server) findBot(name string) (botregistry.EntryWithSchema, bool, error) {
	entries, err := botregistry.ListWithSchema(s.botListOptions())
	if err != nil {
		return botregistry.EntryWithSchema{}, false, err
	}
	for _, e := range entries {
		if e.Name == name {
			return e, true, nil
		}
	}
	return botregistry.EntryWithSchema{}, false, nil
}

// respondBot re-resolves name (post-mutation) and writes it as JSON. It carries
// the editable/origin fields like list/get/create so a PUT/overlay response
// doesn't drop them from the studio's cached entry (a tenant bot would otherwise
// lose its editable flag on an icon/visibility change).
func (s *Server) respondBot(w http.ResponseWriter, r *http.Request, name string) {
	for _, e := range s.tenantBotEntries(r.Context()) {
		if e.Name == name {
			s.writeJSONFor(w, r, botEntryView{EntryWithSchema: e, Editable: true, Origin: "tenant"})
			return
		}
	}
	entry, ok, err := s.findBot(name)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "bots: %v", err)
		return
	}
	if !ok {
		s.httpErrorFor(w, r, http.StatusNotFound, "bots: %q not found", name)
		return
	}
	s.writeJSONFor(w, r, botEntryView{EntryWithSchema: entry, Editable: false, Origin: "catalog"})
}

// regenCatalog refreshes the orchestrator-facing bot catalog after a
// metadata/overlay change. Best-effort: a failure must not fail the
// request (the runtime regenerates on Nexie's next run regardless).
func (s *Server) regenCatalog(name, action string) {
	if s.cfg.WorkDir == "" {
		return
	}
	if _, err := botregistry.RegenerateWhatsNextCatalog(s.cfg.WorkDir); err != nil && s.logger != nil {
		s.logger.Warn("bots: catalog regen after %q %s: %v", name, action, err)
	}
}

// botInstallRequest is the wire body for POST /api/v1/bots/install.
type botInstallRequest struct {
	URL   string `json:"url"`
	Ref   string `json:"ref,omitempty"`
	Path  string `json:"path,omitempty"`
	Name  string `json:"name,omitempty"`
	Force bool   `json:"force,omitempty"`
}

// handleBotInstall imports a bot bundle from a git URL (or a local path on a
// self-hosted server) into the workspace's .botz/ and returns the install
// result. Workspace-mutating + clones an arbitrary URL server-side, so it is
// LOCAL-MODE ONLY: cloud deployments must go through the vetted hosted
// marketplace flow (Phase B), never this raw clone-and-write path.
func (s *Server) handleBotInstall(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	if s.cfg.Mode == "cloud" {
		s.httpErrorFor(w, r, http.StatusForbidden, "bots: install is not available in cloud mode")
		return
	}
	if s.cfg.WorkDir == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "bots: no workspace configured to install into")
		return
	}
	var req botInstallRequest
	if err := readJSON(r, &req); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid request: %v", err)
		return
	}
	if strings.TrimSpace(req.URL) == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "bots: url is required")
		return
	}
	res, err := botinstall.Install(r.Context(), botinstall.Options{
		Source:  req.URL,
		Ref:     req.Ref,
		Path:    req.Path,
		Name:    req.Name,
		Force:   req.Force,
		Workdir: s.cfg.WorkDir,
	})
	if err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "bots: install: %v", err)
		return
	}
	s.writeJSONFor(w, r, res)
}

// handleBotUpload imports a bot bundle from an uploaded `.botz` archive
// into the workspace's .botz/ and returns the install result. Like
// handleBotInstall it is workspace-mutating + LOCAL-MODE ONLY. The body
// is multipart/form-data with a single `file` field (the .botz) plus an
// optional `force` field ("true" overwrites an existing install — the
// "update" path) and optional `name` override.
func (s *Server) handleBotUpload(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	if s.cfg.Mode == "cloud" {
		s.httpErrorFor(w, r, http.StatusForbidden, "bots: upload is not available in cloud mode")
		return
	}
	if s.cfg.WorkDir == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "bots: no workspace configured to install into")
		return
	}
	maxSize := s.cfg.MaxUploadSize
	if maxSize <= 0 {
		maxSize = 50 << 20
	}
	// MaxBytesReader covers the whole body even as ParseMultipartForm
	// streams; ~16 KB headroom for the multipart envelope.
	r.Body = http.MaxBytesReader(w, r.Body, maxSize+16<<10)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			s.httpErrorFor(w, r, http.StatusRequestEntityTooLarge, "bots: file exceeds max upload size (%d bytes)", maxSize)
			return
		}
		s.httpErrorFor(w, r, http.StatusBadRequest, "bots: invalid multipart form: %v", err)
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "bots: missing 'file' field: %v", err)
		return
	}
	defer file.Close()
	force := strings.EqualFold(strings.TrimSpace(r.FormValue("force")), "true")
	name := strings.TrimSpace(r.FormValue("name"))
	// Bundle extraction enforces its own traversal/size/entry guards; the
	// LimitReader is defence-in-depth so a lying Content-Length can't make
	// the extractor read past the cap.
	res, err := botinstall.InstallFromBotzBytes(r.Context(), io.LimitReader(file, maxSize+1), botinstall.Options{
		Name:    name,
		Force:   force,
		Workdir: s.cfg.WorkDir,
	})
	if err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "bots: upload: %v", err)
		return
	}
	s.writeJSONFor(w, r, res)
}

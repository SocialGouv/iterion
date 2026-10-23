package server

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"math"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/botinstall"
	"github.com/SocialGouv/iterion/pkg/botregistry"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
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
// discovery_errors carries one entry per skipped bot source, with the kind of
// skip it was: a bad manifest must not blank the gallery, but it must not
// vanish silently either — and neither must a well-formed source nothing
// routes to.
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
		diags = redactAbsoluteDiscoveryPaths(diags)
		// Headless deployments have no operator watching the CLI stderr or
		// the studio banner — the server log is the only place a skipped
		// bundle's lost automations stay attributable.
		if s.logger != nil {
			for _, d := range diags {
				s.logger.Warn("bots: skipping %s: %s", d.Path, d.Error)
			}
		}
		resp["discovery_errors"] = diags
	}
	s.writeJSONFor(w, r, resp)
}

// redactAbsoluteDiscoveryPaths is the HTTP-boundary fallback for a catalog
// pinned outside WorkDir. ListWithDiagnostics already makes workspace paths
// relative; an external --bots-path has no trustworthy public root, so retain
// the source basename and a source-relative error without exposing the host or
// container prefix to authenticated tenants.
//
// The source's own parent prefix goes first, which keeps the message
// source-relative (`broken-chat/manifest.yaml: …`) rather than reducing it to
// a bare file name. Then EVERY remaining absolute path in the message is cut
// to its last element, whoever put it there: a diagnostic may name a SECOND
// source — a shadowed one names the claimer that holds its name, which sits
// in another root — and a kind added later may name a third. The default is
// inverted on purpose, so a message this function has never seen loses a path
// instead of leaking one.
func redactAbsoluteDiscoveryPaths(diags []botregistry.DiscoveryError) []botregistry.DiscoveryError {
	out := append([]botregistry.DiscoveryError(nil), diags...)
	for i := range out {
		if filepath.IsAbs(out[i].Path) {
			path := filepath.Clean(out[i].Path)
			prefix := filepath.Dir(path) + string(filepath.Separator)
			out[i].Path = filepath.Base(path)
			out[i].Error = strings.ReplaceAll(out[i].Error, prefix, "")
		}
		out[i].Error = redactAbsolutePathsInText(out[i].Error)
	}
	return out
}

// redactAbsolutePathsInText rewrites every absolute path in a diagnostic
// message to its last element, keeping the punctuation the sentence wrapped it
// in (quotes, brackets, a trailing comma or period) so the prose still reads.
// Whitespace and its runs are preserved.
func redactAbsolutePathsInText(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	isSpace := func(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }
	for i := 0; i < len(s); {
		j := i
		for j < len(s) && !isSpace(s[j]) {
			j++
		}
		b.WriteString(redactAbsolutePathToken(s[i:j]))
		k := j
		for k < len(s) && isSpace(s[k]) {
			k++
		}
		b.WriteString(s[j:k])
		i = k
	}
	return b.String()
}

// redactAbsolutePathToken returns one whitespace-delimited token with its
// absolute path, if that is what it holds, reduced to the path's last element.
func redactAbsolutePathToken(tok string) string {
	const openers = `"'([{<`
	const closers = `"')]}>,;:.`
	inner := strings.TrimLeft(tok, openers)
	open := tok[:len(tok)-len(inner)]
	body := strings.TrimRight(inner, closers)
	if !filepath.IsAbs(body) {
		return tok
	}
	return open + filepath.Base(filepath.Clean(body)) + inner[len(body):]
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
	s.writeJSONFor(w, r, s.catalogEntryView(entry, ""))
}

// catalogEntryView is an entry of the filesystem catalog (or a platform
// override) as the bots API answers it: read-only, its origin, and whether
// it is an installed package (isInstalledBundle).
func (s *Server) catalogEntryView(entry botregistry.EntryWithSchema, catalogError string) botEntryView {
	origin := s.entryOrigin(entry.Name)
	return botEntryView{EntryWithSchema: entry, Editable: false, Origin: origin, Installed: origin == "catalog" && s.isInstalledBundle(entry.Path), CatalogError: catalogError}
}

// isInstalledBundle reports whether a discovered bundle's directory is an
// installed package — under the workspace's .botz/, compared on canonical
// paths as the save door's refusal is (isMaterializedBotDependencyPath).
func (s *Server) isInstalledBundle(path string) bool {
	if path == "" {
		return false
	}
	abs, err := s.safePath(path)
	return err == nil && s.isMaterializedBotDependencyPath(abs)
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
	Category    *string   `json:"category,omitempty"`
	Tags        *[]string `json:"tags,omitempty"`
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
	if s.refuseStoredBotEdit(w, r, name) {
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
	// Fail closed on a path that must never be joined. refuseStoredBotEdit
	// above is what names the tier and the surface that owns the edit; this
	// guards the WRITE itself, because an empty path joins to a relative
	// manifest.yaml under the server's CWD — a silently lost edit, and a file
	// written where no bundle lives.
	if entry.Path == "" {
		s.httpErrorFor(w, r, http.StatusConflict,
			"bots: %q resolves to a bundle with no filesystem path; edit it through the bot-source API, not the filesystem catalog", name)
		return
	}
	// An installed package's manifest is the package's: an update (install
	// --force, the marketplace's Update, an upload) replaces .botz/<name>
	// whole and reads nothing of it, so an edit here — a disable above all —
	// would be undone in silence. Its visibility in this workspace is the
	// overlay's, which an update leaves alone; the save door refuses .botz
	// the same way (isMaterializedBotDependencyPath).
	if s.isInstalledBundle(entry.Path) {
		s.httpErrorFor(w, r, http.StatusConflict,
			"bots: %q is an installed package (.botz/): its manifest is the package's, and an update replaces it whole — set its visibility in this workspace with the Catalog manager's toggle (PUT /api/v1/bots/%s/overlay), which an update leaves alone, and edit its metadata in the package's source", name, name)
		return
	}
	var req botUpdateRequest
	if err := readJSON(r, &req); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid request: %v", err)
		return
	}

	manifestPath := filepath.Join(entry.Path, bundle.ManifestFile)
	patch := bundle.ManifestPatch{
		DisplayName: req.DisplayName,
		Icon:        req.Icon,
		Description: req.Description,
		Author:      req.Author,
		Version:     req.Version,
		WhenToUse:   req.WhenToUse,
		Enabled:     req.Enabled,
		Triggers:    req.Triggers,
		Category:    req.Category,
		Tags:        req.Tags,
	}
	if err := patchManifestLocked(r.Context(), manifestPath, patch); err != nil {
		if s.writeStoredFileRefusal(w, r, err) {
			return
		}
		var conflict authoringConflictError
		if errors.As(err, &conflict) {
			s.httpErrorFor(w, r, http.StatusConflict, "bots: write manifest: %v", err)
			return
		}
		s.httpErrorFor(w, r, http.StatusInternalServerError, "bots: write manifest: %v", err)
		return
	}
	catalogError := s.regenCatalog(name, "update")
	s.respondBot(w, r, name, catalogError)
}

// patchManifestLocked applies patch to the manifest at path under the
// authoring lock every server writer of the file takes: read there, patched
// (bundle.PatchManifest) and replaced in one rename (replaceFile) over the
// very bytes it was patched from. Two edits at once both land — the second
// patches what the first wrote — and a file another writer replaced outside
// the lock between the read and the rename is a 409 (checkStoredFile), never
// written over.
func patchManifestLocked(ctx context.Context, path string, patch bundle.ManifestPatch) error {
	locks, err := acquireAuthoringLocalLocks(ctx, []string{path})
	if err != nil {
		return err
	}
	defer closeAuthoringLocalLocks(locks)
	body, _, err := locks[0].parent.read(filepath.Base(path), math.MaxInt64-1)
	decided := storedAt(body)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		decided = storedFile{}
	}
	out, _, err := bundle.PatchManifest(path, body, patch)
	if err != nil {
		return err
	}
	return locks[0].replaceFile(storedCheck{name: bundle.ManifestFile, decided: decided}, out, nil)
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
	// The workspace overlay is never consulted for a STORED entry (team or
	// platform): botregistry reads it off the filesystem catalog, and a
	// stored bundle is materialised without it. A toggle there would 200
	// and change nothing.
	if s.refuseStoredBotEdit(w, r, name) {
		return
	}
	if err := botregistry.SetOverlayEnabled(r.Context(), s.cfg.WorkDir, name, req.Enabled); err != nil {
		// An overlay past a bound of its reader is not rewritten from what
		// was not read: the refusal is the workspace file's, not a fault.
		var refused *botregistry.OverlayRefusal
		if errors.As(err, &refused) {
			s.httpErrorFor(w, r, http.StatusUnprocessableEntity, "bots: overlay: %v", unit.RelTextRoot(s.cfg.WorkDir, err.Error()))
			return
		}
		// Another writer holds the overlay's lock past the wait: nothing
		// was written, and a later toggle may find it free.
		if errors.Is(err, context.DeadlineExceeded) {
			s.httpErrorFor(w, r, http.StatusServiceUnavailable, "bots: overlay: %v", unit.RelTextRoot(s.cfg.WorkDir, err.Error()))
			return
		}
		s.httpErrorFor(w, r, http.StatusInternalServerError, "bots: overlay: %v", err)
		return
	}
	catalogError := s.regenCatalog(name, "overlay")
	// respondBot re-resolves the entry; an unknown name 404s here (after a
	// harmless no-op overlay write — ResolveEnabled ignores unknown names).
	s.respondBot(w, r, name, catalogError)
}

// findBot returns the schema-augmented entry for name (exact match), with
// the platform overlay applied — a platform override's metadata is what
// every tenant sees.
func (s *Server) findBot(name string) (botregistry.EntryWithSchema, bool, error) {
	return s.effectiveFindByName(name)
}

// refuseStoredBotEdit answers 409 when name resolves to a STORED bundle for
// this caller — the active team's own row, or a platform override — and
// reports whether it did.
//
// Both callers (the manifest PUT, the workspace overlay) act on the FILESYSTEM
// catalog. A stored bot is not there: the PUT would write into the baked bundle
// every tenant of this deployment shares, and the overlay would 200 while
// changing nothing the stored bundle is read through. The refusal names the
// surface that does own the edit.
func (s *Server) refuseStoredBotEdit(w http.ResponseWriter, r *http.Request, name string) bool {
	id, _ := auth.FromContext(r.Context())
	switch s.entryOriginFor(r.Context(), id.TeamID, name) {
	case "tenant":
		s.httpErrorFor(w, r, http.StatusConflict,
			"bots: %q is your team's own bot; edit it via /api/teams/%s/bot-sources — this route writes the filesystem catalog every tenant shares", name, id.TeamID)
		return true
	case "platform":
		s.httpErrorFor(w, r, http.StatusConflict,
			"bots: %q is managed as a platform override; edit it via /api/admin/bots (or `iterion remote admin bots push`)", name)
		return true
	}
	return false
}

// respondBot re-resolves name (post-mutation) and writes it as JSON. It carries
// the editable/origin fields like list/get/create so a PUT/overlay response
// doesn't drop them from the studio's cached entry (a tenant bot would otherwise
// lose its editable flag on an icon/visibility change), and catalogError —
// the rendered cause of the refresh that followed the mutation, empty when it
// succeeded — so the studio's only word on a stale routing table is the
// answer to the gesture that made it stale.
func (s *Server) respondBot(w http.ResponseWriter, r *http.Request, name, catalogError string) {
	for _, e := range s.tenantBotEntries(r.Context()) {
		if e.Name == name {
			s.writeJSONFor(w, r, botEntryView{EntryWithSchema: e, Editable: true, Origin: "tenant", CatalogError: catalogError})
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
	s.writeJSONFor(w, r, s.catalogEntryView(entry, catalogError))
}

// regenCatalog refreshes the orchestrator-facing bot catalog after a change
// to the workspace's bots. Never fatal to the request — the gesture already
// landed — so it returns the rendered cause (empty when the refresh
// succeeded or there is no workspace) for the response to carry, and warns
// on the server log for the operators watching it.
func (s *Server) regenCatalog(name, action string) string {
	if s.cfg.WorkDir == "" {
		return ""
	}
	_, err := botregistry.RegenerateWhatsNextCatalog(s.cfg.WorkDir)
	if err == nil {
		return ""
	}
	if s.logger != nil {
		s.logger.Warn("bots: catalog regen after %q %s: %v", name, action, err)
	}
	return err.Error()
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

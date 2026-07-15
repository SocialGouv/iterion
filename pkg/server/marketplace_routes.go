package server

import (
	"net/http"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/botinstall"
	"github.com/SocialGouv/iterion/pkg/botregistry"
	"github.com/SocialGouv/iterion/pkg/marketplace"
	"github.com/SocialGouv/iterion/pkg/plugin"
)

// marketplaceIconMaxBytes caps the submit-time Icon field — an emoji or
// short glyph, never free prose.
const marketplaceIconMaxBytes = 32

// marketplaceSubmitRequest is the wire body for
// POST /api/v1/marketplace/submit. Same shape as the bot-install
// request — repo URL plus optional ref / subpath — augmented by
// operator-supplied marketplace tags.
type marketplaceSubmitRequest struct {
	RepoURL string   `json:"repo_url"`
	Ref     string   `json:"ref,omitempty"`
	Path    string   `json:"path,omitempty"`
	Tags    []string `json:"tags,omitempty"`
	// Icon is an optional emoji / short glyph for the marketplace card
	// (both kinds). Trimmed; capped at marketplaceIconMaxBytes.
	Icon string `json:"icon,omitempty"`
	// Scope is the requested visibility (cloud only). Ignored in local
	// mode. Validated against the server's allowed scopes; empty falls
	// back to the configured default.
	Scope string `json:"scope,omitempty"`
}

// marketplaceInstallResponse is what the install endpoint returns:
// the kind-specific install payload (Install for bots, Plugin for
// plugins) plus the post-bump entry so the studio can show the
// updated install count without a follow-up GET.
type marketplaceInstallResponse struct {
	Install *botinstall.Result `json:"install,omitempty"`
	Plugin  *plugin.View       `json:"plugin,omitempty"`
	Entry   *marketplace.Entry `json:"entry"`
}

// requireMarketplace short-circuits to 404 when the marketplace store
// isn't wired. The HTTP error code matches the "endpoint not enabled"
// convention used elsewhere in this server (cleaner than 503 for a
// pure configuration choice).
func (s *Server) requireMarketplace(w http.ResponseWriter, r *http.Request) bool {
	if s.marketplace == nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "marketplace: not enabled")
		return false
	}
	return true
}

// handleMarketplaceList answers GET /api/v1/marketplace/bots. Query
// params: `q` (free-text), `tag` (exact match), `kind` (bot|plugin),
// `sort` (popular|recent|name). Returns {bots: [...]} for consistency
// with the existing /api/v1/bots envelope.
func (s *Server) handleMarketplaceList(w http.ResponseWriter, r *http.Request) {
	if !s.requireMarketplace(w, r) {
		return
	}
	kind := marketplace.Kind(strings.TrimSpace(r.URL.Query().Get("kind")))
	switch kind {
	case "", marketplace.KindBot, marketplace.KindPlugin:
	default:
		s.httpErrorFor(w, r, http.StatusBadRequest, "marketplace: unknown kind %q (want %s|%s)", kind, marketplace.KindBot, marketplace.KindPlugin)
		return
	}
	sortBy := strings.TrimSpace(r.URL.Query().Get("sort"))
	if err := marketplace.ValidateSort(sortBy); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "%v", err)
		return
	}
	q := marketplace.Query{
		Text:   r.URL.Query().Get("q"),
		Tag:    r.URL.Query().Get("tag"),
		Kind:   kind,
		Sort:   sortBy,
		Viewer: s.marketplaceViewer(r),
	}
	entries, err := s.marketplace.List(r.Context(), q)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "marketplace: list: %v", err)
		return
	}
	if entries == nil {
		entries = []marketplace.Entry{}
	}
	s.writeJSONFor(w, r, map[string]any{"bots": entries})
}

// handleMarketplaceGet answers GET /api/v1/marketplace/bots/{slug}.
func (s *Server) handleMarketplaceGet(w http.ResponseWriter, r *http.Request) {
	if !s.requireMarketplace(w, r) {
		return
	}
	slug := strings.TrimSpace(r.PathValue("slug"))
	if slug == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "marketplace: slug required")
		return
	}
	e, ok, err := s.marketplace.Get(r.Context(), slug)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "marketplace: get: %v", err)
		return
	}
	if !ok {
		s.httpErrorFor(w, r, http.StatusNotFound, "marketplace: %q not found", slug)
		return
	}
	// Return 404 (not 403) when the entry exists but the viewer may not
	// see it — never leak the existence of a scoped/pending slug.
	if !marketplace.Visible(*e, s.marketplaceViewer(r)) {
		s.httpErrorFor(w, r, http.StatusNotFound, "marketplace: %q not found", slug)
		return
	}
	s.writeJSONFor(w, r, e)
}

// handleMarketplaceSubmit answers POST /api/v1/marketplace/submit. Like
// /api/v1/bots/install it clones an arbitrary URL server-side and so is
// LOCAL-MODE ONLY — cloud deployments must go through their own vetted
// submission path. marketplace.InspectSource validates the source
// without writing anything to the workspace and detects its kind (bot
// bundle or plugin); on success we derive the registry slug + persist
// the entry.
func (s *Server) handleMarketplaceSubmit(w http.ResponseWriter, r *http.Request) {
	if !s.requireMarketplace(w, r) {
		return
	}
	if !s.requireSafeOrigin(w, r) {
		return
	}
	// Cloud submissions are authenticated and land pending moderation;
	// local submissions are the sole operator's and auto-approve.
	cloud := s.cfg.Mode == "cloud"
	var submitter auth.Identity
	if cloud {
		id, ok := auth.FromContext(r.Context())
		if !ok || id.UserID == "" {
			s.httpErrorFor(w, r, http.StatusUnauthorized, "authentication required")
			return
		}
		submitter = id
	}
	var req marketplaceSubmitRequest
	if err := readJSON(r, &req); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid request: %v", err)
		return
	}
	if strings.TrimSpace(req.RepoURL) == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "marketplace: repo_url is required")
		return
	}
	icon := strings.TrimSpace(req.Icon)
	if len(icon) > marketplaceIconMaxBytes {
		s.httpErrorFor(w, r, http.StatusBadRequest, "marketplace: icon exceeds %d bytes", marketplaceIconMaxBytes)
		return
	}
	info, err := marketplace.InspectSource(r.Context(), req.RepoURL, req.Ref, req.Path)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "marketplace: inspect: %v", err)
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	var entry marketplace.Entry
	switch info.Kind {
	case marketplace.KindPlugin:
		entry = marketplace.EntryFromPlugin(info.Plugin, req.RepoURL, req.Ref, req.Path)
		entry.Tags = normalizeTags(req.Tags)
		entry.CreatedAt = now
		entry.UpdatedAt = now
	default: // bot
		md := info.Bot
		entry = marketplace.Entry{
			Slug:        botregistry.NormalizeName(md.Name),
			Name:        md.Name,
			DisplayName: md.DisplayName,
			Description: md.Description,
			Author:      md.Author,
			Tags:        normalizeTags(req.Tags),
			RepoURL:     req.RepoURL,
			Ref:         req.Ref,
			Subpath:     req.Path,
			Version:     md.Version,
			README:      md.README,
			Presets:     toEntryPresets(md.Presets),
			Source:      marketplace.SourceGit,
			CreatedAt:   now,
			UpdatedAt:   now,
		}
	}
	entry.Icon = icon
	slug := entry.Slug
	if slug == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "marketplace: %s has no name", info.Kind)
		return
	}
	if cloud {
		// Resolve + validate the requested scope, land the entry pending,
		// and stamp the submitter. Org-scoped slugs are namespaced by org
		// so two orgs publishing the same bot name don't collide on the
		// (slug == _id) key.
		scope := s.resolveSubmitScope(req.Scope)
		entry.Scope = marketplace.Scope(scope)
		entry.Status = marketplace.StatusPending
		entry.SubmittedBy = submitter.UserID
		entry.OrgID = submitter.TeamID
		if entry.Scope == marketplace.ScopeOrg && submitter.TeamID != "" {
			entry.Slug = slug + "@" + submitter.TeamID
			slug = entry.Slug
		}
	}
	// A slug can't change kind through a re-submit: a bot slug stays a
	// bot, a plugin slug stays a plugin (409 instead of a silent morph).
	existing, exists, err := s.marketplace.Get(r.Context(), slug)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "marketplace: get: %v", err)
		return
	}
	if exists && marketplace.EffectiveKind(*existing) != marketplace.EffectiveKind(entry) {
		s.httpErrorFor(w, r, http.StatusConflict, "marketplace: slug %q already exists as a %s entry", slug, marketplace.EffectiveKind(*existing))
		return
	}
	if err := s.marketplace.Upsert(r.Context(), entry); err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "marketplace: upsert: %v", err)
		return
	}
	// Re-read so the response carries the canonical persisted entry
	// (the upsert may have preserved a prior install count).
	stored, ok, err := s.marketplace.Get(r.Context(), slug)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "marketplace: re-read: %v", err)
		return
	}
	if !ok {
		// Should be impossible right after a successful upsert; fall
		// back to the entry we wrote so the client still sees something.
		stored = &entry
	}
	s.writeJSONFor(w, r, stored)
}

// handleMarketplaceInstall answers
// POST /api/v1/marketplace/bots/{slug}/install. Resolves the registry
// entry and installs per its kind: a bot forwards to botinstall.Install
// (into the workspace's .botz/, workspace required); a plugin — a
// binary-adjacent, host-global install under ~/.iterion/plugins/ — is
// super-admin gated and forwards to plugin.InstallWith. Both bump the
// install counter and return the refreshed entry. Local-mode only
// (same constraint as POST /api/v1/bots/install).
func (s *Server) handleMarketplaceInstall(w http.ResponseWriter, r *http.Request) {
	if !s.requireMarketplace(w, r) {
		return
	}
	if !s.requireSafeOrigin(w, r) {
		return
	}
	if s.cfg.Mode == "cloud" {
		s.httpErrorFor(w, r, http.StatusForbidden, "marketplace: install is not available in cloud mode")
		return
	}
	slug := strings.TrimSpace(r.PathValue("slug"))
	if slug == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "marketplace: slug required")
		return
	}
	entry, ok, err := s.marketplace.Get(r.Context(), slug)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "marketplace: get: %v", err)
		return
	}
	if !ok {
		s.httpErrorFor(w, r, http.StatusNotFound, "marketplace: %q not found", slug)
		return
	}
	resp := marketplaceInstallResponse{}
	if marketplace.EffectiveKind(*entry) == marketplace.KindPlugin {
		// Plugins land in ~/.iterion/plugins/ (host-global, affects every
		// workspace this binary serves) — super-admin only. No workspace
		// is required for this branch.
		if !s.isSuperAdmin(r) {
			s.httpErrorFor(w, r, http.StatusForbidden, "marketplace: plugin install requires a super-admin")
			return
		}
		name, err := plugin.InstallWith(r.Context(), plugin.InstallOptions{
			Source:  entry.RepoURL,
			Ref:     entry.Ref,
			Subpath: entry.Subpath,
		})
		if err != nil {
			s.httpErrorFor(w, r, http.StatusBadRequest, "marketplace: install plugin: %v", err)
			return
		}
		resp.Plugin = s.pluginViewAfterInstall(name)
	} else {
		if s.cfg.WorkDir == "" {
			s.httpErrorFor(w, r, http.StatusBadRequest, "marketplace: no workspace configured to install into")
			return
		}
		// `?force=true` overwrites an existing install — the studio "Update"
		// path sends it so re-installing a drifted version succeeds instead
		// of erroring "already exists".
		force := strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("force")), "true")
		res, err := botinstall.Install(r.Context(), botinstall.Options{
			Source:  entry.RepoURL,
			Ref:     entry.Ref,
			Path:    entry.Subpath,
			Force:   force,
			Workdir: s.cfg.WorkDir,
		})
		if err != nil {
			s.httpErrorFor(w, r, http.StatusBadRequest, "marketplace: install: %v", err)
			return
		}
		resp.Install = res
	}
	// Best-effort: a counter bump failure must not fail the install
	// (the file is already on disk; the operator cares about the
	// install, not the popularity counter).
	if err := s.marketplace.IncrementInstalls(r.Context(), slug); err != nil && s.logger != nil {
		s.logger.Warn("marketplace: increment installs for %q: %v", slug, err)
	}
	// Re-read so the caller sees the bumped counter.
	refreshed, _, _ := s.marketplace.Get(r.Context(), slug)
	if refreshed == nil {
		refreshed = entry
	}
	resp.Entry = refreshed
	s.writeJSONFor(w, r, resp)
}

// pluginViewAfterInstall re-loads the plugin registry and projects the
// freshly installed plugin's View. Best-effort like the install-counter
// bump: the install itself already succeeded, so a registry re-read
// failure is logged, not surfaced as a request error.
func (s *Server) pluginViewAfterInstall(name string) *plugin.View {
	reg, err := plugin.Load()
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("marketplace: reload plugin registry after installing %q: %v", name, err)
		}
		return nil
	}
	p, ok := reg.Get(name)
	if !ok {
		if s.logger != nil {
			s.logger.Warn("marketplace: plugin %q not found in registry right after install", name)
		}
		return nil
	}
	v := p.View()
	return &v
}

// handleMarketplaceUninstall answers
// DELETE /api/v1/marketplace/bots/{slug}/install. Resolves the registry
// entry to recover the install name and removes the installed artifact
// per its kind: a bot's workspace bundle (workspace required), or a
// plugin under ~/.iterion/plugins/ (super-admin gated, same as
// install). Returns the (unchanged) entry so the studio can flip the
// card back to "Install". Local-mode only — same as install.
func (s *Server) handleMarketplaceUninstall(w http.ResponseWriter, r *http.Request) {
	if !s.requireMarketplace(w, r) {
		return
	}
	if !s.requireSafeOrigin(w, r) {
		return
	}
	if s.cfg.Mode == "cloud" {
		s.httpErrorFor(w, r, http.StatusForbidden, "marketplace: uninstall is not available in cloud mode")
		return
	}
	slug := strings.TrimSpace(r.PathValue("slug"))
	if slug == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "marketplace: slug required")
		return
	}
	entry, ok, err := s.marketplace.Get(r.Context(), slug)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "marketplace: get: %v", err)
		return
	}
	if !ok {
		s.httpErrorFor(w, r, http.StatusNotFound, "marketplace: %q not found", slug)
		return
	}
	if marketplace.EffectiveKind(*entry) == marketplace.KindPlugin {
		if !s.isSuperAdmin(r) {
			s.httpErrorFor(w, r, http.StatusForbidden, "marketplace: plugin uninstall requires a super-admin")
			return
		}
		// Plugins install under their manifest name (entry.Name), not
		// the registry slug.
		if err := plugin.Uninstall(entry.Name); err != nil {
			s.httpErrorFor(w, r, http.StatusBadRequest, "marketplace: uninstall plugin: %v", err)
			return
		}
		s.writeJSONFor(w, r, entry)
		return
	}
	if s.cfg.WorkDir == "" {
		s.httpErrorFor(w, r, http.StatusBadRequest, "marketplace: no workspace configured to uninstall from")
		return
	}
	// The bundle installs under its manifest name (entry.Name), not the
	// registry slug — Remove deletes <workdir>/.botz/<name>.
	if err := botinstall.Remove(r.Context(), botinstall.Options{
		Name:    entry.Name,
		Workdir: s.cfg.WorkDir,
	}); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "marketplace: uninstall: %v", err)
		return
	}
	s.writeJSONFor(w, r, entry)
}

// normalizeTags strips empty/whitespace entries and de-dups so the
// stored Tags slice is canonical (the JSON store filters tag membership
// exactly; cleanup at the boundary avoids ghost "" tags polluting the
// browse facets).
func normalizeTags(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, t := range in {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// toEntryPresets converts botinstall.PresetMeta into the registry's
// EntryPreset shape (identical fields, distinct types to keep the
// package layer clean).
func toEntryPresets(in []botinstall.PresetMeta) []marketplace.EntryPreset {
	if len(in) == 0 {
		return nil
	}
	out := make([]marketplace.EntryPreset, len(in))
	for i, p := range in {
		out[i] = marketplace.EntryPreset{
			Name:        p.Name,
			DisplayName: p.DisplayName,
			Description: p.Description,
			Skills:      append([]string(nil), p.Skills...),
		}
	}
	return out
}

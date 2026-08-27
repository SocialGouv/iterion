package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/botregistry"
	"github.com/SocialGouv/iterion/pkg/botsource"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Bot sources are TEAM-AUTHORED bot bundles — the writable, tenant-scoped
// counterpart to the read-only catalog baked into the runner image. They are
// what makes cloud bot editing possible: the studio editor's filesystem save
// path has no cloud target, so an edited/created bot persists here instead.
//
// Edit rights = the orthogonal config_editor capability (ADR-078) or team
// management: a bot runs in every team member's context, so authoring one is
// team automation policy, not a personal preference.
func (s *Server) registerBotSourceRoutes() {
	s.mux.Handle("GET /api/teams/{id}/bot-sources", s.requireAuth(http.HandlerFunc(s.handleListBotSources)))
	s.mux.Handle("GET /api/teams/{id}/bot-sources/{slug}", s.requireAuth(http.HandlerFunc(s.handleGetBotSource)))
	s.mux.Handle("PUT /api/teams/{id}/bot-sources/{slug}", s.requireAuth(http.HandlerFunc(s.handlePutBotSource)))
	s.mux.Handle("PUT /api/teams/{id}/bot-sources/{slug}/files/{path...}", s.requireAuth(http.HandlerFunc(s.handlePutBotSourceFile)))
	s.mux.Handle("DELETE /api/teams/{id}/bot-sources/{slug}/files/{path...}", s.requireAuth(http.HandlerFunc(s.handleDeleteBotSourceFile)))
	s.mux.Handle("DELETE /api/teams/{id}/bot-sources/{slug}", s.requireAuth(http.HandlerFunc(s.handleDeleteBotSource)))
	s.mux.Handle("POST /api/teams/{id}/bot-sources/{slug}/fork", s.requireAuth(http.HandlerFunc(s.handleForkBotSource)))
}

// botSourceView is what list/get returns. The metadata list omits Files so a
// team with many bots stays cheap to enumerate; get includes them.
type botSourceView struct {
	botsource.BotSource
	// Warnings are non-fatal push-time notices (e.g. provisioned-projection
	// drift for a platform override). Empty on reads.
	Warnings []string `json:"warnings,omitempty"`
}

// botSourceCtx authorises the request and returns a tenant-scoped context.
func (s *Server) botSourceCtx(w http.ResponseWriter, r *http.Request) (teamID string, id auth.Identity, ok bool) {
	if s.botSources == nil {
		s.httpErrorFor(w, r, http.StatusNotImplemented, "bot editing is not enabled on this server")
		return "", auth.Identity{}, false
	}
	teamID = r.PathValue("id")
	id, _ = auth.FromContext(r.Context())
	if !s.canEditBots(r.Context(), id, teamID) {
		s.httpErrorFor(w, r, http.StatusForbidden, "bot editor, team admin, or owner required")
		return "", auth.Identity{}, false
	}
	return teamID, id, true
}

// maxBotSourceBody caps a bot-source write request body. Comfortably above
// botsource.MaxBundleBytes (JSON escaping inflates content) while keeping an
// unbounded upload from reaching the JSON decoder.
const maxBotSourceBody = 16 << 20

func (s *Server) handleListBotSources(w http.ResponseWriter, r *http.Request) {
	teamID, _, ok := s.botSourceCtx(w, r)
	if !ok {
		return
	}
	s.listBotSourcesFor(w, r, teamID)
}

func (s *Server) listBotSourcesFor(w http.ResponseWriter, r *http.Request, tenantID string) {
	list, err := s.botSources.ListByTenant(store.WithTenant(r.Context(), tenantID), tenantID)
	if err != nil {
		s.botSourceError(w, r, err)
		return
	}
	// Strip Files from the list payload — it is metadata only. Digest gives
	// "what exactly is deployed" a comparable answer without the content.
	views := make([]botSourceMetaView, 0, len(list))
	for _, b := range list {
		digest := botsource.Digest(b.Files)
		b.Files = nil
		views = append(views, botSourceMetaView{BotSource: b, Digest: digest})
	}
	s.writeJSONFor(w, r, map[string]any{"bot_sources": views})
}

// botSourceMetaView is one list row: the metadata plus the content digest.
type botSourceMetaView struct {
	botsource.BotSource
	Digest string `json:"digest,omitempty"`
}

func (s *Server) handleGetBotSource(w http.ResponseWriter, r *http.Request) {
	teamID, _, ok := s.botSourceCtx(w, r)
	if !ok {
		return
	}
	s.getBotSourceFor(w, r, teamID, r.PathValue("slug"))
}

func (s *Server) getBotSourceFor(w http.ResponseWriter, r *http.Request, tenantID, slug string) {
	bs, err := s.botSources.GetBySlug(store.WithTenant(r.Context(), tenantID), tenantID, slug)
	if err != nil {
		s.botSourceError(w, r, err)
		return
	}
	s.writeJSONFor(w, r, botSourceView{BotSource: bs})
}

type botSourcePutReq struct {
	Files map[string]string `json:"files"`
	// Version, when non-zero, is an if-match token: the write is rejected with
	// 409 if the stored version advanced (a concurrent editor wrote in between).
	Version int `json:"version,omitempty"`
}

// handlePutBotSource creates or replaces a bot's whole bundle. The bundle is
// compiled before it persists — a bot that does not compile is rejected at
// write time (400 with the diagnostics), never left to fail silently at launch.
func (s *Server) handlePutBotSource(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	teamID, id, ok := s.botSourceCtx(w, r)
	if !ok {
		return
	}
	s.putBotSourceFor(w, r, teamID, id.UserID, r.PathValue("slug"))
}

func (s *Server) putBotSourceFor(w http.ResponseWriter, r *http.Request, tenantID, userID, slug string) {
	slug = strings.TrimSpace(slug)
	var req botSourcePutReq
	r.Body = http.MaxBytesReader(w, r.Body, maxBotSourceBody)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			s.httpErrorFor(w, r, http.StatusRequestEntityTooLarge, "bundle body exceeds %d bytes", tooBig.Limit)
			return
		}
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid body: %v", err)
		return
	}
	bs := botsource.BotSource{TenantID: tenantID, Slug: slug, Files: req.Files, Version: req.Version}
	if err := bs.Validate(); err != nil {
		s.botSourceError(w, r, err)
		return
	}
	if diags := validateBundleCompile(bs.Files); len(diags) > 0 {
		s.httpErrorFor(w, r, http.StatusBadRequest, "bot does not compile: %s", strings.Join(diags, "; "))
		return
	}
	s.writeBotSource(w, r, tenantID, userID, bs, s.platformPushWarnings(tenantID, bs)...)
}

// platformPushWarnings surfaces the known gaps a platform push does NOT
// close, so the operator learns them at push time rather than from a
// silently unrouted webhook. Today: the provisioning-time CommandMap/
// BotRules projections stored on forge integrations are built from the
// manifest at PROVISION time and are not rebuilt on an override push — an
// override that changes `invocations:` routes correctly through live
// discovery but keeps the provisioned map stale until re-provisioning.
func (s *Server) platformPushWarnings(tenantID string, bs botsource.BotSource) []string {
	if !botsource.IsPlatform(tenantID) {
		return nil
	}
	m := bs.Manifest()
	if m == nil {
		return nil
	}
	baked, ok, err := botregistry.FindByName(s.botListOptions(), bs.Slug)
	if err != nil || !ok {
		return nil // a new-slug platform bot has nothing provisioned to drift from
	}
	if reflect.DeepEqual(baked.Invocations, m.Invocations) {
		return nil
	}
	return []string{fmt.Sprintf(
		"invocations differ from the baked %q manifest: provisioned webhook CommandMap/BotRules are built at provision time and will NOT pick this up until the repo integration is re-provisioned (live command discovery does)",
		bs.Slug)}
}

// handlePutBotSourceFile writes one file into an existing bundle — the editor's
// per-file save. The whole bundle is re-validated so a bad edit to any file is
// caught, not just main.bot.
func (s *Server) handlePutBotSourceFile(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	teamID, id, ok := s.botSourceCtx(w, r)
	if !ok {
		return
	}
	s.putBotSourceFileFor(w, r, teamID, id.UserID, r.PathValue("slug"))
}

func (s *Server) putBotSourceFileFor(w http.ResponseWriter, r *http.Request, tenantID, userID, slug string) {
	ctx := store.WithTenant(r.Context(), tenantID)
	bs, err := s.botSources.GetBySlug(ctx, tenantID, slug)
	if err != nil {
		s.botSourceError(w, r, err)
		return
	}
	var body struct {
		Content string `json:"content"`
		Version int    `json:"version,omitempty"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBotSourceBody)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid body: %v", err)
		return
	}
	path := strings.TrimSpace(r.PathValue("path"))
	// Clone before mutating: the store may hand out its live map (memory
	// store), and a write that Validate then REJECTS must not have already
	// poisoned the stored row — one rejected traversal key otherwise bricks
	// the bot (unwritable and unlaunchable) and races concurrent launches.
	files := make(map[string]string, len(bs.Files)+1)
	for k, v := range bs.Files {
		files[k] = v
	}
	files[path] = body.Content
	bs.Files = files
	bs.Version = body.Version
	if err := bs.Validate(); err != nil {
		s.botSourceError(w, r, err)
		return
	}
	if diags := validateBundleCompile(bs.Files); len(diags) > 0 {
		s.httpErrorFor(w, r, http.StatusBadRequest, "bot does not compile: %s", strings.Join(diags, "; "))
		return
	}
	s.writeBotSource(w, r, tenantID, userID, bs)
}

func (s *Server) handleDeleteBotSourceFile(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	teamID, id, ok := s.botSourceCtx(w, r)
	if !ok {
		return
	}
	s.deleteBotSourceFileFor(w, r, teamID, id.UserID, r.PathValue("slug"))
}

func (s *Server) deleteBotSourceFileFor(w http.ResponseWriter, r *http.Request, tenantID, userID, slug string) {
	ctx := store.WithTenant(r.Context(), tenantID)
	bs, err := s.botSources.GetBySlug(ctx, tenantID, slug)
	if err != nil {
		s.botSourceError(w, r, err)
		return
	}
	path := strings.TrimSpace(r.PathValue("path"))
	if path == botsource.MainBotFile {
		s.httpErrorFor(w, r, http.StatusBadRequest, "cannot delete %s (the bundle entry)", botsource.MainBotFile)
		return
	}
	// Clone before mutating — same aliasing hazard as the file put above.
	files := make(map[string]string, len(bs.Files))
	for k, v := range bs.Files {
		if k != path {
			files[k] = v
		}
	}
	bs.Files = files
	bs.Version = 0 // no if-match on a delete
	if err := bs.Validate(); err != nil {
		s.botSourceError(w, r, err)
		return
	}
	s.writeBotSource(w, r, tenantID, userID, bs)
}

// writeBotSource persists a create-or-update: an existing slug updates in place
// (preserving the id), a new slug creates. warnings ride the response so a
// push learns its known gaps immediately (see platformPushWarnings).
func (s *Server) writeBotSource(w http.ResponseWriter, r *http.Request, tenantID, userID string, bs botsource.BotSource, warnings ...string) {
	ctx := store.WithTenant(r.Context(), tenantID)
	existing, err := s.botSources.GetBySlug(ctx, tenantID, bs.Slug)
	switch {
	case err == nil:
		bs.ID = existing.ID
		bs.UpdatedBy = userID
		out, uerr := s.botSources.Update(ctx, bs)
		if uerr != nil {
			s.botSourceError(w, r, uerr)
			return
		}
		s.auditBotSource(r, tenantID, "updated", out)
		s.writeJSONFor(w, r, botSourceView{BotSource: out, Warnings: warnings})
	case errors.Is(err, botsource.ErrNotFound):
		bs.CreatedBy = userID
		out, cerr := s.botSources.Create(ctx, bs)
		if cerr != nil {
			s.botSourceError(w, r, cerr)
			return
		}
		s.auditBotSource(r, tenantID, "created", out)
		s.writeJSONFor(w, r, botSourceView{BotSource: out, Warnings: warnings})
	default:
		s.botSourceError(w, r, err)
	}
}

func (s *Server) handleDeleteBotSource(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	teamID, _, ok := s.botSourceCtx(w, r)
	if !ok {
		return
	}
	s.deleteBotSourceFor(w, r, teamID, r.PathValue("slug"))
}

func (s *Server) deleteBotSourceFor(w http.ResponseWriter, r *http.Request, tenantID, slug string) {
	ctx := store.WithTenant(r.Context(), tenantID)
	bs, err := s.botSources.GetBySlug(ctx, tenantID, slug)
	if err != nil {
		s.botSourceError(w, r, err)
		return
	}
	if err := s.botSources.Delete(ctx, bs.ID); err != nil {
		s.botSourceError(w, r, err)
		return
	}
	s.auditBotSource(r, tenantID, "deleted", bs)
	s.writeJSONFor(w, r, map[string]any{"deleted": true})
}

// auditBotSource records a bot-source mutation. Platform-tenant mutations are
// deployment-wide code changes and land on the PLATFORM audit log with the
// content digest — the provenance record for "what exactly is deployed";
// team-scoped mutations land on the team's log.
func (s *Server) auditBotSource(r *http.Request, tenantID, action string, bs botsource.BotSource) {
	meta := map[string]any{
		"slug":    bs.Slug,
		"version": bs.Version,
		"digest":  botsource.Digest(bs.Files),
	}
	if botsource.IsPlatform(tenantID) {
		// The mutating replica reads its own write immediately; the others
		// converge within the entry cache's TTL.
		s.invalidatePlatformBots()
		s.auditPlatform(r, "", "platform.bot."+action, "bot_source", bs.Slug, meta)
		return
	}
	s.auditTenant(r, tenantID, "bot_source."+action, "bot_source", bs.Slug, meta)
}

type botSourceForkReq struct {
	// From is the catalog bot id to fork (e.g. "feature-dev").
	From string `json:"from"`
}

// handleForkBotSource copies a read-only baked catalog bot into the team store
// under the request slug, so the team can then edit it. The whole bundle is
// copied — main.bot, manifest, skills/, prompts/, … — not just main.bot.
func (s *Server) handleForkBotSource(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	teamID, id, ok := s.botSourceCtx(w, r)
	if !ok {
		return
	}
	s.forkBotSourceFor(w, r, teamID, id.UserID, r.PathValue("slug"))
}

func (s *Server) forkBotSourceFor(w http.ResponseWriter, r *http.Request, tenantID, userID, slug string) {
	slug = strings.TrimSpace(slug)
	var req botSourceForkReq
	// The body is one {"from": "<slug>"} — cap it; a bare Decode buffers
	// whatever a caller streams.
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid body: %v", err)
		return
	}
	files, resolvedID, err := s.catalogBundleFiles(req.From)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "%v", err)
		return
	}
	if files == nil {
		s.httpErrorFor(w, r, http.StatusNotFound, "catalog bot %q not found", req.From)
		return
	}
	bs := botsource.BotSource{
		TenantID:  tenantID,
		Slug:      slug,
		Files:     files,
		Origin:    "forked:" + resolvedID,
		CreatedBy: userID,
	}
	if err := bs.Validate(); err != nil {
		s.botSourceError(w, r, err)
		return
	}
	out, err := s.botSources.Create(store.WithTenant(r.Context(), tenantID), bs)
	if err != nil {
		s.botSourceError(w, r, err)
		return
	}
	s.auditBotSource(r, tenantID, "forked", out)
	s.writeJSONFor(w, r, botSourceView{BotSource: out})
}

// catalogBundleFiles reads a baked catalog bot's COMPLETE bundle tree off the
// pod filesystem into a files map (the fork source) — every file, including
// root runtime files like devbox.json/devbox.lock, not just the layout dirs
// (an earlier version copied LayoutDirs only, silently dropping the bot's
// pinned toolchain). Returns (nil, "", nil) when the bot id does not resolve;
// an explicit error when the bundle contains a file the store cannot carry
// (non-UTF-8 — botsource.ReadBundleDir is the one shared definition of
// "what a bundle dir contains", also used by the CLI push).
func (s *Server) catalogBundleFiles(botID string) (map[string]string, string, error) {
	botID = strings.TrimSpace(botID)
	if botID == "" {
		return nil, "", nil
	}
	mainPath, err := botregistry.ResolveBotPath(botID, s.effectivePaths())
	if err != nil {
		return nil, "", nil //nolint:nilerr // unresolvable id = not found, not an error
	}
	if filepath.Base(mainPath) != botsource.MainBotFile {
		// A loose <name>.bot has no bundle dir of its own — walking its
		// PARENT would sweep in every sibling bot. Fork just its source.
		body, rerr := os.ReadFile(mainPath)
		if rerr != nil {
			return nil, "", fmt.Errorf("read catalog bot %q: %w", botID, rerr)
		}
		return map[string]string{botsource.MainBotFile: string(body)}, botID, nil
	}
	files, err := botsource.ReadBundleDir(filepath.Dir(mainPath))
	if err != nil {
		return nil, "", fmt.Errorf("read catalog bundle %q: %w", botID, err)
	}
	if strings.TrimSpace(files[botsource.MainBotFile]) == "" {
		return nil, "", nil
	}
	return files, botID, nil
}

// validateBundleCompile materializes a bundle to a temp dir and runs the same
// parse → merge-bundle-prompts → compile path as `iterion validate`, returning
// the SeverityError diagnostics (empty = compiles). This is the compilability
// oracle the pure structural botsource.Validate deliberately leaves to the
// route, where the full bundle context is available.
func validateBundleCompile(files map[string]string) []string {
	dir, err := os.MkdirTemp("", "botsource-validate-*")
	if err != nil {
		return []string{"internal: " + err.Error()}
	}
	defer func() { _ = os.RemoveAll(dir) }()

	if err := botsource.Materialize(dir, files); err != nil {
		return []string{err.Error()}
	}

	mainPath := filepath.Join(dir, botsource.MainBotFile)
	src, err := os.ReadFile(mainPath)
	if err != nil {
		return []string{"internal: " + err.Error()}
	}
	var diags []string
	pr := parser.Parse(mainPath, string(src))
	for _, d := range pr.Diagnostics {
		if d.Severity == parser.SeverityError {
			diags = append(diags, d.Error())
		}
	}
	if pr.File == nil || len(pr.File.Workflows) == 0 {
		return append(diags, "no workflow found in main.bot")
	}
	if b, berr := bundle.OpenDir(dir); berr == nil && b != nil {
		_ = runview.MergeBundlePrompts(pr.File, b)
	}
	cr := ir.Compile(pr.File)
	for _, d := range cr.Diagnostics {
		if d.Severity == ir.SeverityError {
			diags = append(diags, d.Error())
		}
	}
	return diags
}

// botSourceError maps store errors to actionable status codes.
func (s *Server) botSourceError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, botsource.ErrNotFound):
		s.httpErrorFor(w, r, http.StatusNotFound, "bot source not found")
	case errors.Is(err, botsource.ErrSlugConflict):
		s.httpErrorFor(w, r, http.StatusConflict, "a bot with this slug already exists for the team")
	case errors.Is(err, botsource.ErrVersionConflict):
		s.httpErrorFor(w, r, http.StatusConflict, "%v", err)
	case errors.Is(err, botsource.ErrTenantMissing):
		s.httpErrorFor(w, r, http.StatusForbidden, "%v", err)
	default:
		s.httpErrorFor(w, r, http.StatusBadRequest, "%v", err)
	}
}

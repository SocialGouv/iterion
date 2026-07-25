package server

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
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

func (s *Server) handleListBotSources(w http.ResponseWriter, r *http.Request) {
	teamID, _, ok := s.botSourceCtx(w, r)
	if !ok {
		return
	}
	list, err := s.botSources.ListByTenant(store.WithTenant(r.Context(), teamID), teamID)
	if err != nil {
		s.botSourceError(w, r, err)
		return
	}
	// Strip Files from the list payload — it is metadata only.
	views := make([]botsource.BotSource, 0, len(list))
	for _, b := range list {
		b.Files = nil
		views = append(views, b)
	}
	s.writeJSONFor(w, r, map[string]any{"bot_sources": views})
}

func (s *Server) handleGetBotSource(w http.ResponseWriter, r *http.Request) {
	teamID, _, ok := s.botSourceCtx(w, r)
	if !ok {
		return
	}
	bs, err := s.botSources.GetBySlug(store.WithTenant(r.Context(), teamID), teamID, r.PathValue("slug"))
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
	slug := strings.TrimSpace(r.PathValue("slug"))
	var req botSourcePutReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid body: %v", err)
		return
	}
	bs := botsource.BotSource{TenantID: teamID, Slug: slug, Files: req.Files, Version: req.Version}
	if err := bs.Validate(); err != nil {
		s.botSourceError(w, r, err)
		return
	}
	if diags := validateBundleCompile(bs.Files); len(diags) > 0 {
		s.httpErrorFor(w, r, http.StatusBadRequest, "bot does not compile: %s", strings.Join(diags, "; "))
		return
	}
	s.writeBotSource(w, r, teamID, id, bs)
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
	ctx := store.WithTenant(r.Context(), teamID)
	bs, err := s.botSources.GetBySlug(ctx, teamID, r.PathValue("slug"))
	if err != nil {
		s.botSourceError(w, r, err)
		return
	}
	var body struct {
		Content string `json:"content"`
		Version int    `json:"version,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid body: %v", err)
		return
	}
	path := strings.TrimSpace(r.PathValue("path"))
	if bs.Files == nil {
		bs.Files = map[string]string{}
	}
	bs.Files[path] = body.Content
	bs.Version = body.Version
	if err := bs.Validate(); err != nil {
		s.botSourceError(w, r, err)
		return
	}
	if diags := validateBundleCompile(bs.Files); len(diags) > 0 {
		s.httpErrorFor(w, r, http.StatusBadRequest, "bot does not compile: %s", strings.Join(diags, "; "))
		return
	}
	s.writeBotSource(w, r, teamID, id, bs)
}

func (s *Server) handleDeleteBotSourceFile(w http.ResponseWriter, r *http.Request) {
	if !s.requireSafeOrigin(w, r) {
		return
	}
	teamID, id, ok := s.botSourceCtx(w, r)
	if !ok {
		return
	}
	ctx := store.WithTenant(r.Context(), teamID)
	bs, err := s.botSources.GetBySlug(ctx, teamID, r.PathValue("slug"))
	if err != nil {
		s.botSourceError(w, r, err)
		return
	}
	path := strings.TrimSpace(r.PathValue("path"))
	if path == botsource.MainBotFile {
		s.httpErrorFor(w, r, http.StatusBadRequest, "cannot delete %s (the bundle entry)", botsource.MainBotFile)
		return
	}
	delete(bs.Files, path)
	bs.Version = 0 // no if-match on a delete
	if err := bs.Validate(); err != nil {
		s.botSourceError(w, r, err)
		return
	}
	s.writeBotSource(w, r, teamID, id, bs)
}

// writeBotSource persists a create-or-update: an existing slug updates in place
// (preserving the id), a new slug creates.
func (s *Server) writeBotSource(w http.ResponseWriter, r *http.Request, teamID string, id auth.Identity, bs botsource.BotSource) {
	ctx := store.WithTenant(r.Context(), teamID)
	existing, err := s.botSources.GetBySlug(ctx, teamID, bs.Slug)
	switch {
	case err == nil:
		bs.ID = existing.ID
		bs.UpdatedBy = id.UserID
		out, uerr := s.botSources.Update(ctx, bs)
		if uerr != nil {
			s.botSourceError(w, r, uerr)
			return
		}
		s.writeJSONFor(w, r, botSourceView{BotSource: out})
	case errors.Is(err, botsource.ErrNotFound):
		bs.CreatedBy = id.UserID
		out, cerr := s.botSources.Create(ctx, bs)
		if cerr != nil {
			s.botSourceError(w, r, cerr)
			return
		}
		s.writeJSONFor(w, r, botSourceView{BotSource: out})
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
	ctx := store.WithTenant(r.Context(), teamID)
	bs, err := s.botSources.GetBySlug(ctx, teamID, r.PathValue("slug"))
	if err != nil {
		s.botSourceError(w, r, err)
		return
	}
	if err := s.botSources.Delete(ctx, bs.ID); err != nil {
		s.botSourceError(w, r, err)
		return
	}
	s.writeJSONFor(w, r, map[string]any{"deleted": true})
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
	slug := strings.TrimSpace(r.PathValue("slug"))
	var req botSourceForkReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid body: %v", err)
		return
	}
	files, resolvedID, found := s.catalogBundleFiles(req.From)
	if !found {
		s.httpErrorFor(w, r, http.StatusNotFound, "catalog bot %q not found", req.From)
		return
	}
	bs := botsource.BotSource{
		TenantID:  teamID,
		Slug:      slug,
		Files:     files,
		Origin:    "forked:" + resolvedID,
		CreatedBy: id.UserID,
	}
	if err := bs.Validate(); err != nil {
		s.botSourceError(w, r, err)
		return
	}
	out, err := s.botSources.Create(store.WithTenant(r.Context(), teamID), bs)
	if err != nil {
		s.botSourceError(w, r, err)
		return
	}
	s.writeJSONFor(w, r, botSourceView{BotSource: out})
}

// catalogBundleFiles reads a baked catalog bot's whole bundle tree off the pod
// filesystem into a files map (the fork source). Returns the resolved bot id so
// the fork can record its provenance.
func (s *Server) catalogBundleFiles(botID string) (map[string]string, string, bool) {
	botID = strings.TrimSpace(botID)
	if botID == "" {
		return nil, "", false
	}
	mainPath, err := botregistry.ResolveBotPath(botID, s.effectivePaths())
	if err != nil {
		return nil, "", false
	}
	main, err := os.ReadFile(mainPath)
	if err != nil {
		return nil, "", false
	}
	dir := filepath.Dir(mainPath)
	files := map[string]string{botsource.MainBotFile: string(main)}
	for _, mf := range []string{bundle.ManifestFile, bundle.ManifestFileAlt} {
		if b, rerr := os.ReadFile(filepath.Join(dir, mf)); rerr == nil {
			files[mf] = string(b)
		}
	}
	for _, ld := range bundle.LayoutDirs {
		root := filepath.Join(dir, ld)
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, werr error) error {
			if werr != nil || d.IsDir() {
				return nil //nolint:nilerr // a missing layout dir is simply absent
			}
			rel, rerr := filepath.Rel(dir, p)
			if rerr != nil {
				return nil
			}
			if b, brerr := os.ReadFile(p); brerr == nil {
				files[filepath.ToSlash(rel)] = string(b)
			}
			return nil
		})
	}
	return files, botID, true
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

	for rel, content := range files {
		dst := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return []string{"internal: " + err.Error()}
		}
		if err := os.WriteFile(dst, []byte(content), 0o644); err != nil {
			return []string{"internal: " + err.Error()}
		}
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

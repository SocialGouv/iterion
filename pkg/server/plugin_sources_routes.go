package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/SocialGouv/iterion/pkg/auth"

	"github.com/SocialGouv/iterion/pkg/pluginsource"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Plugin sources bind a TEAM to a plugin hosted in a git repository — the
// durable, org-private counterpart to a plugin installed into a pod's iterion
// home (which a restart silently loses). See pkg/pluginsource.
//
// Team-manage rights are required: a source designates code that will be
// mirrored into every one of the team's runs, so it is org automation policy,
// not a personal preference.
func (s *Server) registerPluginSourceRoutes() {
	s.mux.Handle("GET /api/teams/{id}/plugin-sources", s.requireAuth(http.HandlerFunc(s.handleListPluginSources)))
	s.mux.Handle("POST /api/teams/{id}/plugin-sources", s.requireAuth(http.HandlerFunc(s.handleCreatePluginSource)))
	s.mux.Handle("PATCH /api/teams/{id}/plugin-sources/{source_id}", s.requireAuth(http.HandlerFunc(s.handleUpdatePluginSource)))
	s.mux.Handle("DELETE /api/teams/{id}/plugin-sources/{source_id}", s.requireAuth(http.HandlerFunc(s.handleDeletePluginSource)))
}

type pluginSourceReq struct {
	Name     *string `json:"name,omitempty"`
	GitURL   *string `json:"git_url,omitempty"`
	Ref      *string `json:"ref,omitempty"`
	SecretID *string `json:"secret_id,omitempty"`
	Enabled  *bool   `json:"enabled,omitempty"`
}

// pluginSourceView is what the API returns. It never includes the credential —
// only the secret's ID, which the operator already knows.
type pluginSourceView struct {
	pluginsource.PluginSource
	// PinnedRef surfaces the drift risk in the UI: a moving ref means the
	// plugin can change under a run with no operator action.
	PinnedRef bool `json:"pinned_ref"`
	// Degraded says the last launch that needed this source could not
	// materialise it and proceeded without it; degraded_reason carries the
	// failure and degraded_at when it was recorded. A PATCH that re-verifies
	// the source clears it.
	Degraded bool `json:"degraded"`
}

func toPluginSourceView(p pluginsource.PluginSource) pluginSourceView {
	return pluginSourceView{PluginSource: p, PinnedRef: p.PinnedRef(), Degraded: p.Degraded()}
}

// verifyPluginSource materialises the source exactly as a launch will — clone,
// parse the manifest, read every contribution — so a source that registers is
// a source a launch can use. A launch is where this used to be discovered,
// one delivery at a time, with the operator long gone. Reported as 422 with
// the underlying error (the YAML parser's line, git's refusal) verbatim.
//
// Without a fetcher wired the registration proceeds unverified, and says so:
// the source will then only be found broken by the launches that skip it.
func (s *Server) verifyPluginSource(ctx context.Context, w http.ResponseWriter, r *http.Request, ps pluginsource.PluginSource) bool {
	if s.pluginSourceFetcher == nil {
		s.logWarn("plugin source %q (team %s) registered UNVERIFIED: no fetcher is wired on this server, so a broken manifest is only found by the launches that skip it", ps.Name, ps.TenantID)
		return true
	}
	if _, err := pluginsource.Materialize(ctx, s.pluginSourceFetcher, ps); err != nil {
		s.httpErrorFor(w, r, http.StatusUnprocessableEntity, "plugin source %q cannot be used by a launch and was not saved — %v", ps.Name, err)
		return false
	}
	return true
}

// pluginSourceCtx authorises the request and returns a tenant-scoped context.
func (s *Server) pluginSourceCtx(w http.ResponseWriter, r *http.Request) (teamID string, ok bool) {
	if s.pluginSources == nil {
		s.httpErrorFor(w, r, http.StatusNotImplemented, "plugin sources are not enabled on this server")
		return "", false
	}
	teamID = r.PathValue("id")
	id, _ := auth.FromContext(r.Context())
	if !s.canManageTeam(r.Context(), id, teamID) {
		s.httpErrorFor(w, r, http.StatusForbidden, "team admin or owner required")
		return "", false
	}
	return teamID, true
}

func (s *Server) handleListPluginSources(w http.ResponseWriter, r *http.Request) {
	teamID, ok := s.pluginSourceCtx(w, r)
	if !ok {
		return
	}
	list, err := s.pluginSources.ListByTenant(store.WithTenant(r.Context(), teamID), teamID)
	if err != nil {
		s.httpErrorFor(w, r, http.StatusInternalServerError, "list plugin sources: %v", err)
		return
	}
	views := make([]pluginSourceView, 0, len(list))
	for _, p := range list {
		views = append(views, toPluginSourceView(p))
	}
	s.writeJSONFor(w, r, map[string]any{"plugin_sources": views})
}

func (s *Server) handleCreatePluginSource(w http.ResponseWriter, r *http.Request) {
	teamID, ok := s.pluginSourceCtx(w, r)
	if !ok {
		return
	}
	var req pluginSourceReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid body: %v", err)
		return
	}
	ps := pluginsource.PluginSource{TenantID: teamID}
	applyPluginSourceReq(&ps, req)
	if ps.Ref == "" {
		ps.Ref = "main"
	}
	if id, ok := auth.FromContext(r.Context()); ok {
		ps.CreatedBy = id.UserID
	}
	ctx := store.WithTenant(r.Context(), teamID)
	// Shape first (cheap, and a local path must be refused before anything
	// is cloned), then the materialisation a launch will perform.
	if err := ps.Validate(); err != nil {
		s.pluginSourceError(w, r, err)
		return
	}
	if !s.verifyPluginSource(ctx, w, r, ps) {
		return
	}
	if err := s.pluginSources.Create(ctx, ps); err != nil {
		s.pluginSourceError(w, r, err)
		return
	}
	list, _ := s.pluginSources.ListByTenant(ctx, teamID)
	for _, p := range list {
		if p.Name == ps.Name {
			s.writeJSONFor(w, r, toPluginSourceView(p))
			return
		}
	}
	s.writeJSONFor(w, r, toPluginSourceView(ps))
}

func (s *Server) handleUpdatePluginSource(w http.ResponseWriter, r *http.Request) {
	teamID, ok := s.pluginSourceCtx(w, r)
	if !ok {
		return
	}
	var req pluginSourceReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.httpErrorFor(w, r, http.StatusBadRequest, "invalid body: %v", err)
		return
	}
	ctx := store.WithTenant(r.Context(), teamID)
	ps, err := s.pluginSources.Get(ctx, r.PathValue("source_id"))
	if err != nil {
		s.pluginSourceError(w, r, err)
		return
	}
	applyPluginSourceReq(&ps, req)
	// Re-verify whenever what a launch fetches changed, whenever the source
	// is being switched on, and whenever it currently reads degraded — a
	// PATCH is the operator's way of saying "I fixed it", and the flag must
	// answer with a real check, not a toggle. A rename or a disable of a
	// healthy source costs no clone.
	reverify := req.GitURL != nil || req.Ref != nil || req.SecretID != nil ||
		(req.Enabled != nil && *req.Enabled) || ps.Degraded()
	if reverify {
		if err := ps.Validate(); err != nil {
			s.pluginSourceError(w, r, err)
			return
		}
		if !s.verifyPluginSource(ctx, w, r, ps) {
			return
		}
	}
	if err := s.pluginSources.Update(ctx, ps); err != nil {
		s.pluginSourceError(w, r, err)
		return
	}
	if reverify && ps.Degraded() {
		if err := s.pluginSources.ClearDegraded(ctx, teamID, ps.ID); err != nil {
			s.httpErrorFor(w, r, http.StatusInternalServerError, "plugin source %q verified but its degraded flag could not be cleared: %v", ps.Name, err)
			return
		}
		ps.DegradedReason, ps.DegradedAt = "", nil
	}
	s.writeJSONFor(w, r, toPluginSourceView(ps))
}

func (s *Server) handleDeletePluginSource(w http.ResponseWriter, r *http.Request) {
	teamID, ok := s.pluginSourceCtx(w, r)
	if !ok {
		return
	}
	ctx := store.WithTenant(r.Context(), teamID)
	if err := s.pluginSources.Delete(ctx, r.PathValue("source_id")); err != nil {
		s.pluginSourceError(w, r, err)
		return
	}
	s.writeJSONFor(w, r, map[string]any{"deleted": true})
}

func applyPluginSourceReq(ps *pluginsource.PluginSource, req pluginSourceReq) {
	if req.Name != nil {
		ps.Name = *req.Name
	}
	if req.GitURL != nil {
		ps.GitURL = *req.GitURL
	}
	if req.Ref != nil {
		ps.Ref = *req.Ref
	}
	if req.SecretID != nil {
		ps.SecretID = *req.SecretID
	}
	if req.Enabled != nil {
		ps.Enabled = *req.Enabled
	}
}

// pluginSourceError maps store errors to actionable status codes. Validation
// failures are 400 with the reason verbatim: a malformed source must be
// rejected at write time rather than silently contributing nothing at launch.
func (s *Server) pluginSourceError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, pluginsource.ErrNotFound):
		s.httpErrorFor(w, r, http.StatusNotFound, "plugin source not found")
	case errors.Is(err, pluginsource.ErrNameConflict):
		s.httpErrorFor(w, r, http.StatusConflict, "a plugin source with this name already exists for the team")
	case errors.Is(err, pluginsource.ErrTenantMissing):
		s.httpErrorFor(w, r, http.StatusForbidden, "%v", err)
	default:
		s.httpErrorFor(w, r, http.StatusBadRequest, "%v", err)
	}
}

package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/cloudsched"
	"github.com/SocialGouv/iterion/pkg/configshare"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// registerConfigEditorRoutes wires the AUTHENTICATED (session) config-editor
// surface (ADR-078): a real user holding the config_editor capability on a team
// — or a team admin — edits the team's config-shares WITHOUT an iws_ token,
// through the normal cookie session. Distinct from the public capability-URL
// routes (registerConfigSharePublicRoutes), which keep their Bearer-only,
// cookie-less, CSRF-immune contract untouched. Same ProjectedRead/ApplyEdit
// service, so the projection + fail-closed allow-list are identical.
func (s *Server) registerConfigEditorRoutes() {
	s.mux.Handle("GET /api/teams/{id}/config-editor/shares", s.requireAuth(http.HandlerFunc(s.handleConfigEditorList)))
	s.mux.Handle("GET /api/teams/{id}/config-editor/shares/{sid}/config", s.requireAuth(http.HandlerFunc(s.handleConfigEditorGet)))
	s.mux.Handle("PATCH /api/teams/{id}/config-editor/shares/{sid}/config", s.requireAuth(http.HandlerFunc(s.handleConfigEditorPatch)))
	// Cadence: a config_editor may edit the cron of the schedule tied to its
	// share's (bot, category) — the recurrence stays first-class in iterion's
	// schedule store (visible in the Schedules UI), never buried in the repo.
	s.mux.Handle("GET /api/teams/{id}/config-editor/shares/{sid}/schedule", s.requireAuth(http.HandlerFunc(s.handleConfigEditorGetSchedule)))
	s.mux.Handle("PATCH /api/teams/{id}/config-editor/shares/{sid}/schedule", s.requireAuth(http.HandlerFunc(s.handleConfigEditorPatchSchedule)))
	// Recent runs of the share's (bot, category): a read-only, reduced view so
	// the editor can SEE the effect of their edits (did the last digest run,
	// when, did it succeed) without granting the full run console.
	s.mux.Handle("GET /api/teams/{id}/config-editor/shares/{sid}/runs", s.requireAuth(http.HandlerFunc(s.handleConfigEditorRuns)))
}

// configEditorRunsLimit caps how many recent digests the editor view returns —
// enough to see the recent cadence, not a full history browser.
const configEditorRunsLimit = 8

// handleConfigEditorRuns returns a REDUCED, read-only view of the recent runs
// of the share's (bot, category): status + timestamps only, never run ids,
// inputs, errors, or artifacts (the config_editor role has no run-console
// access, and this endpoint must not become a side-channel to it). Scoped to
// the share's category when it has one, so an editor of `a11y` never sees the
// `design-systems` cadence. Tenant isolation rides ListRunRecordsCtx (the
// request context carries the caller's team → mongo tenant_id filter).
func (s *Server) handleConfigEditorRuns(w http.ResponseWriter, r *http.Request) {
	sh, ok := s.loadEditableShare(w, r)
	if !ok {
		return
	}
	if s.runs == nil {
		writeJSON(w, map[string]any{"runs": []any{}})
		return
	}
	records, err := s.runs.ListRunRecordsCtx(r.Context(), runview.ListFilter{Bundle: sh.BotID, Limit: 200})
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	writeJSON(w, map[string]any{"runs": editorRunRows(records, sh.Category, configEditorRunsLimit)})
}

// editorRunRows is the reduced, no-leak projection for handleConfigEditorRuns,
// factored out so the scoping + field allow-list are unit-testable without a
// live store. For each run of the share's bot it emits ONLY status + timestamps
// — never the run id, inputs, or error, which the config_editor role must not
// see. When category is non-empty, only runs whose launch var
// `category` matches are kept, so an `a11y` editor never sees the
// `design-systems` cadence. Records are assumed newest-first (ListRunRecordsCtx
// sorts by CreatedAt desc); the first `limit` matches are returned.
func editorRunRows(records []*store.Run, category string, limit int) []map[string]any {
	rows := make([]map[string]any, 0, limit)
	for _, run := range records {
		if run == nil {
			continue
		}
		if category != "" {
			cat, _ := run.Inputs["category"].(string)
			if cat != category {
				continue
			}
		}
		row := map[string]any{
			"status":     run.Status,
			"created_at": run.CreatedAt,
		}
		if run.FinishedAt != nil {
			row["finished_at"] = run.FinishedAt
		}
		rows = append(rows, row)
		if len(rows) >= limit {
			break
		}
	}
	return rows
}

// editorShareView is the REDUCED projection a config-editor sees — enough to
// render the editor menu, never the token metadata / audit surface the operator
// shareView carries (token_last4, fingerprint, deliveries).
func editorShareView(sh *configshare.Share) map[string]any {
	return map[string]any{
		"id": sh.ID, "bot_id": sh.BotID, "label": sh.Label,
		"category": sh.Category, "config_path": sh.ConfigPath, "read_only": sh.ReadOnly,
	}
}

// loadEditableShare gates on canEditConfigShares and loads the share, verifying
// it belongs to the path team.
func (s *Server) loadEditableShare(w http.ResponseWriter, r *http.Request) (*configshare.Share, bool) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canEditConfigShares(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "forbidden")
		return nil, false
	}
	sh, err := s.configShares.GetByID(r.Context(), r.PathValue("sid"))
	if err != nil || sh == nil || sh.TenantID != teamID || !sh.Enabled {
		httpError(w, http.StatusNotFound, "not found")
		return nil, false
	}
	return sh, true
}

func (s *Server) handleConfigEditorList(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canEditConfigShares(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "forbidden")
		return
	}
	rows, err := s.configShares.ListByTenant(r.Context(), teamID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	specCache := map[string]*bundle.ConfigShareSpec{}
	views := make([]map[string]any, 0, len(rows))
	for _, sh := range rows {
		if !sh.Enabled {
			continue
		}
		v := editorShareView(sh)
		// Bot-declared editor branding (manifest config_share.editor_title) so
		// the shell can show "Éditeur de veilles" instead of the generic title.
		spec, ok := specCache[sh.BotID]
		if !ok {
			spec = s.botConfigShareSpec(sh.BotID)
			specCache[sh.BotID] = spec
		}
		if spec != nil {
			if spec.EditorTitle != "" {
				v["editor_title"] = spec.EditorTitle
			}
			if spec.EditorDescription != "" {
				v["editor_description"] = spec.EditorDescription
			}
		}
		views = append(views, v)
	}
	writeJSON(w, map[string]any{"shares": views})
}

func (s *Server) handleConfigEditorGet(w http.ResponseWriter, r *http.Request) {
	sh, ok := s.loadEditableShare(w, r)
	if !ok {
		return
	}
	fc, err := s.resolveShareFC(r.Context(), sh)
	if err != nil {
		httpError(w, http.StatusBadGateway, "config source unavailable")
		return
	}
	proj, sha, err := s.configShareSvc.ProjectedRead(r.Context(), fc, sh)
	if err != nil {
		httpError(w, http.StatusBadGateway, "read failed")
		return
	}
	writeJSON(w, map[string]any{
		"config": proj, "sha": sha,
		"bot_id": sh.BotID, "label": sh.Label, "category": sh.Category,
		"config_path": sh.ConfigPath, "read_only": sh.ReadOnly,
	})
}

func (s *Server) handleConfigEditorPatch(w http.ResponseWriter, r *http.Request) {
	sh, ok := s.loadEditableShare(w, r)
	if !ok {
		return
	}
	if sh.ReadOnly {
		httpError(w, http.StatusForbidden, "read_only")
		return
	}
	var req configSharePatchReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 128<<10)).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if len(req.Patch) == 0 {
		httpError(w, http.StatusBadRequest, "patch has no editable field")
		return
	}
	if req.SHA == "" {
		httpError(w, http.StatusBadRequest, "sha required")
		return
	}
	msg := "chore(config-share): edit " + sh.ConfigPath + " via config-editor"
	s.applyShareEditAndRespond(w, r, sh, req, msg)
}

// findCategorySchedule locates the recurring schedule bound to a share's
// (bot, category) — the per-category digest schedule feed-watch-style bots
// register (vars.category == the share's category). Returns false when the
// share isn't category-scoped, the schedule store is absent (local mode), or
// no matching schedule exists. Deliberately narrow: a share with no category
// never matches the category-less collect schedule, so the config_editor can
// only ever touch its own category's cadence.
func (s *Server) findCategorySchedule(r *http.Request, sh *configshare.Share) (cloudsched.ScheduledBot, bool) {
	if s.cfg.ScheduledBots == nil || sh.Category == "" {
		return cloudsched.ScheduledBot{}, false
	}
	rows, err := s.cfg.ScheduledBots.ListByTenant(r.Context(), sh.TenantID)
	if err != nil {
		return cloudsched.ScheduledBot{}, false
	}
	for _, sb := range rows {
		if sb.BotID == sh.BotID && sb.Vars["category"] == sh.Category {
			return sb, true
		}
	}
	return cloudsched.ScheduledBot{}, false
}

// handleConfigEditorGetSchedule returns the cadence (cron + next fire) of the
// share's category schedule, if one exists. The cadence lives in iterion's
// schedule store, NOT the repo config — so it stays visible in the Schedules
// UI; this endpoint only lets the scoped editor read/adjust it.
func (s *Server) handleConfigEditorGetSchedule(w http.ResponseWriter, r *http.Request) {
	sh, ok := s.loadEditableShare(w, r)
	if !ok {
		return
	}
	if s.cfg.ScheduledBots == nil {
		httpError(w, http.StatusNotFound, "schedules not available")
		return
	}
	sb, found := s.findCategorySchedule(r, sh)
	resp := map[string]any{"exists": found}
	if found {
		resp["schedule_id"] = sb.ID
		resp["cron"] = sb.Cron
		resp["disabled"] = sb.Disabled
		if !sb.NextFireAt.IsZero() {
			resp["next_fire_at"] = sb.NextFireAt
		}
	}
	writeJSON(w, resp)
}

// handleConfigEditorPatchSchedule updates ONLY the cron of the share's
// category schedule. Edit-only by design: creating a schedule (and its
// delivery sinks) stays an operator action, mirroring category creation —
// a missing schedule returns a clear 404 rather than being auto-created.
func (s *Server) handleConfigEditorPatchSchedule(w http.ResponseWriter, r *http.Request) {
	sh, ok := s.loadEditableShare(w, r)
	if !ok {
		return
	}
	if sh.ReadOnly {
		httpError(w, http.StatusForbidden, "read_only")
		return
	}
	if s.cfg.ScheduledBots == nil {
		httpError(w, http.StatusNotFound, "schedules not available")
		return
	}
	var req struct {
		Cron string `json:"cron"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid body")
		return
	}
	cronExpr := strings.TrimSpace(req.Cron)
	if cronExpr == "" {
		httpError(w, http.StatusBadRequest, "cron required")
		return
	}
	if err := cloudsched.ValidateCron(cronExpr); err != nil {
		httpError(w, http.StatusBadRequest, "%s", err.Error())
		return
	}
	sb, found := s.findCategorySchedule(r, sh)
	if !found {
		httpError(w, http.StatusNotFound, "no schedule for this category — ask an administrator to create one")
		return
	}
	now := s.scheduleNow()
	next, err := cloudsched.NextFire(cronExpr, now)
	if err != nil {
		httpError(w, http.StatusBadRequest, "%s", err.Error())
		return
	}
	updated, err := s.cfg.ScheduledBots.Update(r.Context(), sb.ID, cloudsched.SchedulePatch{
		UpdatedAt:  now,
		Cron:       &cronExpr,
		NextFireAt: &next,
	})
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	s.auditTenant(r, sh.TenantID, "schedule.updated", "schedule", updated.ID, map[string]any{
		"bot_id": updated.BotID, "category": sh.Category, "via": "config-editor", "cron_changed": true,
	})
	resp := map[string]any{"cron": updated.Cron}
	if !updated.NextFireAt.IsZero() {
		resp["next_fire_at"] = updated.NextFireAt
	}
	writeJSON(w, resp)
}

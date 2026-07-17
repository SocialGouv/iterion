package server

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/cloudsched"
)

// registerScheduleRoutes wires the team-scoped CRUD REST API for cloud
// recurring bot schedules. Cloud-only: registered by server_routes when the
// cloudsched store is present (local mode has no ticker to fire them).
// Mirrors the sibling generic-secrets / bot-bindings routes for
// requireAuth + team-authorization semantics.
func (s *Server) registerScheduleRoutes() {
	s.mux.Handle("GET /api/teams/{id}/schedules", s.requireAuth(http.HandlerFunc(s.handleListSchedules)))
	s.mux.Handle("POST /api/teams/{id}/schedules", s.requireAuth(http.HandlerFunc(s.handleCreateSchedule)))
	s.mux.Handle("PATCH /api/teams/{id}/schedules/{sid}", s.requireAuth(http.HandlerFunc(s.handleUpdateSchedule)))
	s.mux.Handle("DELETE /api/teams/{id}/schedules/{sid}", s.requireAuth(http.HandlerFunc(s.handleDeleteSchedule)))
}

type createScheduleReq struct {
	BotID    string            `json:"bot_id"`
	Cron     string            `json:"cron"`
	Vars     map[string]string `json:"vars,omitempty"`
	RepoURL  string            `json:"repo_url,omitempty"`
	RepoRef  string            `json:"repo_ref,omitempty"`
	Disabled bool              `json:"disabled,omitempty"`
}

type updateScheduleReq struct {
	Cron     *string            `json:"cron,omitempty"`
	Vars     *map[string]string `json:"vars,omitempty"`
	RepoURL  *string            `json:"repo_url,omitempty"`
	RepoRef  *string            `json:"repo_ref,omitempty"`
	Disabled *bool              `json:"disabled,omitempty"`
}

// scheduleNow returns the UTC instant used for CreatedAt / UpdatedAt and to
// seed NextFire. Tests override via s.scheduleClock (a package-level test
// hook) so NextFireAt is deterministic. Default is time.Now().UTC() —
// matching the forge Orchestrator's clock default.
func (s *Server) scheduleNow() time.Time {
	if s.scheduleClock != nil {
		return s.scheduleClock().UTC()
	}
	return time.Now().UTC()
}

func (s *Server) handleListSchedules(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canViewTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "not a member")
		return
	}
	rows, err := s.cfg.ScheduledBots.ListByTenant(r.Context(), teamID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	if rows == nil {
		rows = []cloudsched.ScheduledBot{}
	}
	writeJSON(w, struct {
		Schedules []cloudsched.ScheduledBot `json:"schedules"`
	}{Schedules: rows})
}

func (s *Server) handleCreateSchedule(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canManageTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "admin or owner required")
		return
	}
	var req createScheduleReq
	if !decodeJSON(w, r, &req) {
		return
	}
	botID := strings.TrimSpace(req.BotID)
	cronExpr := strings.TrimSpace(req.Cron)
	if botID == "" || cronExpr == "" {
		httpError(w, http.StatusBadRequest, "bot_id and cron required")
		return
	}
	if err := cloudsched.ValidateCron(cronExpr); err != nil {
		httpError(w, http.StatusBadRequest, "%s", err.Error())
		return
	}
	now := s.scheduleNow()
	next, err := cloudsched.NextFire(cronExpr, now)
	if err != nil {
		httpError(w, http.StatusBadRequest, "%s", err.Error())
		return
	}
	sb := cloudsched.ScheduledBot{
		ID:         uuid.NewString(),
		TenantID:   teamID,
		BotID:      botID,
		Cron:       cronExpr,
		Vars:       req.Vars,
		RepoURL:    strings.TrimSpace(req.RepoURL),
		RepoRef:    strings.TrimSpace(req.RepoRef),
		Disabled:   req.Disabled,
		NextFireAt: next,
		CreatedBy:  id.UserID,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := s.cfg.ScheduledBots.Create(r.Context(), sb); err != nil {
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	s.auditTenant(r, teamID, "schedule.created", "schedule", sb.ID, map[string]any{
		"bot_id": botID, "cron": cronExpr, "repo_url": sb.RepoURL != "", "disabled": sb.Disabled,
	})
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, sb)
}

// scheduleForTeam looks the row up and enforces tenant ownership. A row that
// belongs to another team surfaces as 404, not 403, so a caller can't probe
// for schedule ids across teams — same discipline as bindingForTenantBot.
func (s *Server) scheduleForTeam(w http.ResponseWriter, r *http.Request, teamID, id string) (cloudsched.ScheduledBot, bool) {
	sb, err := s.cfg.ScheduledBots.Get(r.Context(), id)
	if err != nil || sb.TenantID != teamID {
		httpError(w, http.StatusNotFound, "schedule not found")
		return cloudsched.ScheduledBot{}, false
	}
	return sb, true
}

func (s *Server) handleUpdateSchedule(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canManageTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "admin or owner required")
		return
	}
	scheduleID := r.PathValue("sid")
	if _, ok := s.scheduleForTeam(w, r, teamID, scheduleID); !ok {
		return
	}
	var req updateScheduleReq
	if !decodeJSON(w, r, &req) {
		return
	}
	now := s.scheduleNow()
	patch := cloudsched.SchedulePatch{UpdatedAt: now}
	if req.Cron != nil {
		cronExpr := strings.TrimSpace(*req.Cron)
		if err := cloudsched.ValidateCron(cronExpr); err != nil {
			httpError(w, http.StatusBadRequest, "%s", err.Error())
			return
		}
		next, err := cloudsched.NextFire(cronExpr, now)
		if err != nil {
			httpError(w, http.StatusBadRequest, "%s", err.Error())
			return
		}
		patch.Cron = &cronExpr
		patch.NextFireAt = &next
	}
	if req.Vars != nil {
		patch.Vars = req.Vars
	}
	if req.RepoURL != nil {
		trimmed := strings.TrimSpace(*req.RepoURL)
		patch.RepoURL = &trimmed
	}
	if req.RepoRef != nil {
		trimmed := strings.TrimSpace(*req.RepoRef)
		patch.RepoRef = &trimmed
	}
	if req.Disabled != nil {
		patch.Disabled = req.Disabled
	}
	updated, err := s.cfg.ScheduledBots.Update(r.Context(), scheduleID, patch)
	if err != nil {
		if errors.Is(err, cloudsched.ErrNotFound) {
			httpError(w, http.StatusNotFound, "schedule not found")
			return
		}
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	s.auditTenant(r, teamID, "schedule.updated", "schedule", updated.ID, map[string]any{
		"bot_id": updated.BotID, "cron_changed": req.Cron != nil, "disabled_changed": req.Disabled != nil,
	})
	writeJSON(w, updated)
}

func (s *Server) handleDeleteSchedule(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	teamID := r.PathValue("id")
	if !s.canManageTeam(r.Context(), id, teamID) {
		httpError(w, http.StatusForbidden, "admin or owner required")
		return
	}
	scheduleID := r.PathValue("sid")
	sb, ok := s.scheduleForTeam(w, r, teamID, scheduleID)
	if !ok {
		return
	}
	if err := s.cfg.ScheduledBots.Delete(r.Context(), scheduleID); err != nil {
		if errors.Is(err, cloudsched.ErrNotFound) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		httpError(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	s.auditTenant(r, teamID, "schedule.deleted", "schedule", sb.ID, map[string]any{"bot_id": sb.BotID})
	w.WriteHeader(http.StatusNoContent)
}

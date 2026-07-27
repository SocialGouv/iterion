package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/cloudsched"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/retrypolicy"
	"github.com/SocialGouv/iterion/pkg/schedgate"
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
	BotID string `json:"bot_id"`
	Cron  string `json:"cron"`
	// IntervalSeconds turns this into an always-on (keepalive) schedule
	// instead of cron: mutually exclusive with cron. Overlap defaults to
	// keepalive when set.
	IntervalSeconds int               `json:"interval_seconds,omitempty"`
	Vars            map[string]string `json:"vars,omitempty"`
	RepoURL         string            `json:"repo_url,omitempty"`
	RepoRef         string            `json:"repo_ref,omitempty"`
	Disabled        bool              `json:"disabled,omitempty"`
	// Overlap policy + guard (pkg/schedgate; validated on create).
	Overlap       string `json:"overlap,omitempty"`
	MaxConcurrent int    `json:"max_concurrent,omitempty"`
	Guard         string `json:"guard,omitempty"`
	GuardTimeout  string `json:"guard_timeout,omitempty"`
	GuardVar      string `json:"guard_var,omitempty"`
	StaleAfter    string `json:"stale_after,omitempty"`
	// Retry policy (pkg/retrypolicy; validated on create). Only the fields
	// set here override the bot's manifest and the machine default.
	RetryUsageWindow string `json:"retry_usage_window,omitempty"`
	RetryMaxAttempts int    `json:"retry_max_attempts,omitempty"`
	RetryMaxWait     string `json:"retry_max_wait,omitempty"`
	RetryJitter      string `json:"retry_jitter,omitempty"`
}

type updateScheduleReq struct {
	Cron            *string            `json:"cron,omitempty"`
	IntervalSeconds *int               `json:"interval_seconds,omitempty"`
	Vars            *map[string]string `json:"vars,omitempty"`
	RepoURL         *string            `json:"repo_url,omitempty"`
	RepoRef         *string            `json:"repo_ref,omitempty"`
	Disabled        *bool              `json:"disabled,omitempty"`
	// Overlap policy + guard (pkg/schedgate; the merged result is
	// validated against the row's current values on update).
	Overlap       *string `json:"overlap,omitempty"`
	MaxConcurrent *int    `json:"max_concurrent,omitempty"`
	Guard         *string `json:"guard,omitempty"`
	GuardTimeout  *string `json:"guard_timeout,omitempty"`
	GuardVar      *string `json:"guard_var,omitempty"`
	StaleAfter    *string `json:"stale_after,omitempty"`
	// Retry policy (pkg/retrypolicy; the merged result is validated against
	// the row's current values on update).
	RetryUsageWindow *string `json:"retry_usage_window,omitempty"`
	RetryMaxAttempts *int    `json:"retry_max_attempts,omitempty"`
	RetryMaxWait     *string `json:"retry_max_wait,omitempty"`
	RetryJitter      *string `json:"retry_jitter,omitempty"`
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
	keepalive := req.IntervalSeconds > 0
	if botID == "" {
		httpError(w, http.StatusBadRequest, "bot_id required")
		return
	}
	if keepalive && cronExpr != "" {
		httpError(w, http.StatusBadRequest, "set either cron or interval_seconds, not both")
		return
	}
	if !keepalive && cronExpr == "" {
		httpError(w, http.StatusBadRequest, "cron or interval_seconds required")
		return
	}
	overlap := req.Overlap
	if keepalive {
		if floor := int(bundle.KeepaliveMinInterval.Seconds()); req.IntervalSeconds < floor {
			httpError(w, http.StatusBadRequest, "interval_seconds must be >= %d", floor)
			return
		}
		if strings.TrimSpace(overlap) == "" {
			overlap = schedgate.OverlapKeepalive
		}
	} else {
		if err := cloudsched.ValidateCron(cronExpr); err != nil {
			httpError(w, http.StatusBadRequest, "%s", err.Error())
			return
		}
	}
	if err := schedgate.Validate(schedgate.Policy{
		Overlap:       overlap,
		MaxConcurrent: req.MaxConcurrent,
		Guard:         req.Guard,
		GuardTimeout:  req.GuardTimeout,
		GuardVar:      req.GuardVar,
		StaleAfter:    req.StaleAfter,
	}); err != nil {
		httpError(w, http.StatusBadRequest, "%s", err.Error())
		return
	}
	if err := retrypolicy.Validate(retrypolicy.Policy{
		UsageWindow: req.RetryUsageWindow,
		MaxAttempts: req.RetryMaxAttempts,
		MaxWait:     req.RetryMaxWait,
		Jitter:      req.RetryJitter,
	}); err != nil {
		httpError(w, http.StatusBadRequest, "%s", err.Error())
		return
	}
	now := s.scheduleNow()
	sb := cloudsched.ScheduledBot{
		ID:              uuid.NewString(),
		TenantID:        teamID,
		BotID:           botID,
		Cron:            cronExpr,
		IntervalSeconds: req.IntervalSeconds,
		Vars:            req.Vars,
		RepoURL:         strings.TrimSpace(req.RepoURL),
		RepoRef:         strings.TrimSpace(req.RepoRef),
		// First-class repo binding when the URL maps to a provisioned
		// integration: the schedules UI then groups this row with the
		// repo's other automation instead of joining by URL string.
		RepoIntegrationID: s.resolveRepoIntegrationID(r.Context(), teamID, req.RepoURL),
		Disabled:          req.Disabled,
		Overlap:           overlap,
		MaxConcurrent:     req.MaxConcurrent,
		Guard:             req.Guard,
		GuardTimeout:      req.GuardTimeout,
		GuardVar:          req.GuardVar,
		StaleAfter:        req.StaleAfter,
		RetryUsageWindow:  req.RetryUsageWindow,
		RetryMaxAttempts:  req.RetryMaxAttempts,
		RetryMaxWait:      req.RetryMaxWait,
		RetryJitter:       req.RetryJitter,
		CreatedBy:         id.UserID,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	next, err := cloudsched.NextFireForBot(sb, now)
	if err != nil {
		httpError(w, http.StatusBadRequest, "%s", err.Error())
		return
	}
	sb.NextFireAt = next
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

// resolveRepoIntegrationID maps a schedule's repo_url onto the team's
// provisioned integration for that repo, when one exists. Best-effort:
// an unknown URL (or no forge stores wired) just leaves the schedule
// URL-bound, which the ticker handles identically — the integration id
// only improves grouping and lifecycle (DeleteByIntegration).
func (s *Server) resolveRepoIntegrationID(ctx context.Context, teamID, repoURL string) string {
	target := normalizeRepoURL(repoURL)
	if target == "" || s.forgeIntegrations == nil || s.forgeConnections == nil {
		return ""
	}
	integrations, err := s.forgeIntegrations.ListByTenant(ctx, teamID)
	if err != nil {
		return ""
	}
	for _, ri := range integrations {
		conn, cerr := s.forgeConnections.Get(ctx, ri.ConnectionID)
		if cerr != nil {
			continue
		}
		if normalizeRepoURL(forge.CloneURLFor(conn.BaseURL(), ri.RepoFullName)) == target {
			return ri.ID
		}
	}
	return ""
}

// normalizeRepoURL canonicalizes a repo URL for equality: lowercase,
// scheme-less, no trailing ".git" or "/".
func normalizeRepoURL(u string) string {
	u = strings.ToLower(strings.TrimSpace(u))
	for _, p := range []string{"https://", "http://", "ssh://"} {
		u = strings.TrimPrefix(u, p)
	}
	u = strings.TrimPrefix(u, "git@")
	u = strings.Replace(u, ":", "/", 1) // git@host:owner/repo → host/owner/repo
	u = strings.TrimSuffix(strings.TrimSuffix(u, "/"), ".git")
	return u
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
	cur, ok := s.scheduleForTeam(w, r, teamID, scheduleID)
	if !ok {
		return
	}
	var req updateScheduleReq
	if !decodeJSON(w, r, &req) {
		return
	}
	// Validate the MERGED policy (current row + patch) so a partial
	// update can't leave an incoherent combination behind.
	merged := schedgate.Policy{
		Overlap:       cur.Overlap,
		MaxConcurrent: cur.MaxConcurrent,
		Guard:         cur.Guard,
		GuardTimeout:  cur.GuardTimeout,
		GuardVar:      cur.GuardVar,
		StaleAfter:    cur.StaleAfter,
	}
	if req.Overlap != nil {
		merged.Overlap = *req.Overlap
	}
	if req.MaxConcurrent != nil {
		merged.MaxConcurrent = *req.MaxConcurrent
	}
	if req.Guard != nil {
		merged.Guard = *req.Guard
	}
	if req.GuardTimeout != nil {
		merged.GuardTimeout = *req.GuardTimeout
	}
	if req.GuardVar != nil {
		merged.GuardVar = *req.GuardVar
	}
	if req.StaleAfter != nil {
		merged.StaleAfter = *req.StaleAfter
	}
	if err := schedgate.Validate(merged); err != nil {
		httpError(w, http.StatusBadRequest, "%s", err.Error())
		return
	}
	// Same merge-then-validate for the retry policy: a patch touching one
	// retry field must not be able to leave the row incoherent.
	mergedRetry := cur.RetryPolicy()
	if req.RetryUsageWindow != nil {
		mergedRetry.UsageWindow = *req.RetryUsageWindow
	}
	if req.RetryMaxAttempts != nil {
		mergedRetry.MaxAttempts = *req.RetryMaxAttempts
	}
	if req.RetryMaxWait != nil {
		mergedRetry.MaxWait = *req.RetryMaxWait
	}
	if req.RetryJitter != nil {
		mergedRetry.Jitter = *req.RetryJitter
	}
	if err := retrypolicy.Validate(mergedRetry); err != nil {
		httpError(w, http.StatusBadRequest, "%s", err.Error())
		return
	}
	now := s.scheduleNow()
	patch := cloudsched.SchedulePatch{UpdatedAt: now}
	patch.Overlap = req.Overlap
	patch.MaxConcurrent = req.MaxConcurrent
	patch.Guard = req.Guard
	patch.GuardTimeout = req.GuardTimeout
	patch.GuardVar = req.GuardVar
	patch.StaleAfter = req.StaleAfter
	patch.RetryUsageWindow = req.RetryUsageWindow
	patch.RetryMaxAttempts = req.RetryMaxAttempts
	patch.RetryMaxWait = req.RetryMaxWait
	patch.RetryJitter = req.RetryJitter
	// cron and interval_seconds are mutually exclusive cadences; whichever
	// the request provides recomputes NextFireAt and clears the other. The
	// exclusive switch (not two independent ifs) makes each patch field set
	// exactly once.
	switch {
	case req.IntervalSeconds != nil && *req.IntervalSeconds > 0:
		if req.Cron != nil && strings.TrimSpace(*req.Cron) != "" {
			httpError(w, http.StatusBadRequest, "set either cron or interval_seconds, not both")
			return
		}
		iv := *req.IntervalSeconds
		if floor := int(bundle.KeepaliveMinInterval.Seconds()); iv < floor {
			httpError(w, http.StatusBadRequest, "interval_seconds must be >= %d", floor)
			return
		}
		next := now.Add(time.Duration(iv) * time.Second)
		empty := ""
		patch.IntervalSeconds, patch.Cron, patch.NextFireAt = &iv, &empty, &next // keepalive clears cron
	case req.Cron != nil:
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
		zero := 0
		patch.Cron, patch.NextFireAt, patch.IntervalSeconds = &cronExpr, &next, &zero // cron clears the interval
	case req.IntervalSeconds != nil:
		// Explicit interval_seconds:0 disables keepalive without a new cron.
		iv := 0
		patch.IntervalSeconds = &iv
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

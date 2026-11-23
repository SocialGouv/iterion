package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/cloudsched"
	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/llmroute"
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
	// Routing block (pkg/llmroute, ADR-121; validated on create) — the
	// schedule's binding-level voice over the bot's manifest. Decoded
	// STRICTLY through the RawMessage (a camelCase typo must 400, not
	// silently store a no-op block audited as set).
	Routing json.RawMessage `json:"routing,omitempty"`
	// Pool block (ADR-164; validated on create) — the binding-level pool
	// opt-in, decoded STRICTLY through decodePoolBlock like the routing
	// block beside it. On create, `null` and absent both mean no block.
	Pool json.RawMessage `json:"pool,omitempty"`
	// KeyOverrides pins a BYOK api key per LLM provider (provider → api key
	// id) for this schedule's runs, over the bound integration's own pins.
	// Validated like a webhook's or a trigger subscription's pin
	// (validateKeyOverrides), against the identity the tick launches under.
	KeyOverrides map[string]string `json:"key_overrides,omitempty"`
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
	// Routing block (pkg/llmroute, ADR-121): ABSENT leaves the stored block
	// untouched (an edit dialog that mirrors the row back must not wipe
	// what it does not know about), `null` clears it, an object replaces it
	// wholesale. The same mechanic the platform record's PUT uses.
	Routing json.RawMessage `json:"routing,omitempty"`
	// Pool block (ADR-164): the binding-level opt-in — {pool_id, enabled,
	// strategy{mode, margin_pct, triggers, reset_hold}}. ABSENT leaves the
	// stored block untouched, `null` clears it (the schedule inherits
	// again), an object replaces it wholesale. Validated at record-write
	// time (validatePoolBindingWrite): an ENABLED block needs the pool to
	// exist, be enabled and CONSENT (an org pool of this team's org, a user
	// pool whose audience admits the team); an opt-out block (enabled
	// false) grants nothing and needs only a valid shape. An enabled block
	// is the machine lane's grant: the schedule's launches draw the named
	// pool because an admin of this team wrote it.
	Pool json.RawMessage `json:"pool,omitempty"`
	// KeyOverrides (provider → api key id): ABSENT leaves the stored pins
	// untouched, `null` or `{}` clears them (the schedule inherits its
	// integration's pins again), an object replaces them wholesale —
	// validated before it lands, like the create lane.
	KeyOverrides json.RawMessage `json:"key_overrides,omitempty"`
}

// decodeScheduleKeyPins reads a PATCH's key_overrides block: spoke is false
// when the request left the field out, pins is nil when it cleared them.
func decodeScheduleKeyPins(raw json.RawMessage) (map[string]string, bool, error) {
	if len(raw) == 0 {
		return nil, false, nil
	}
	var pins map[string]string
	if err := json.Unmarshal(raw, &pins); err != nil {
		return nil, false, fmt.Errorf("key_overrides: want an object of provider → api key id: %w", err)
	}
	if len(pins) == 0 {
		return nil, true, nil
	}
	return pins, true, nil
}

// validateScheduleKeyPins judges a schedule's key pins for the write: the
// rule every machine lane's pin passes (validateKeyOverrides), against the
// identity the schedule's tick resolves credentials under — the tick holds
// the stored pins to that same rule again when it spends them.
func (s *Server) validateScheduleKeyPins(ctx context.Context, teamID, botID string, pins map[string]string) error {
	return s.validateKeyOverrides(teamTenantCtx(ctx, teamID), teamID, scheduledLaunchOwner(botID), pins)
}

// decodeScheduleRouting decodes a schedule request's routing block through
// the ONE routing decoder (routing_decode.go) under the binding surface's
// lock posture: the binding level does not hold the lock primitive
// (ADR-121 — routingLockHolders do; a schedule lock would launder unset
// past the author).
func decodeScheduleRouting(raw json.RawMessage) (*llmroute.Policy, bool, *routingRequestError) {
	return decodeRoutingBlock(raw, routingLocksRefused)
}

type routingRequestError struct {
	status int
	err    error
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
	// canReap=false: the cloud gate drops ReapRunIDs — the NATS lease is the
	// liveness authority there (see schedules_gate.go).
	if err := schedgate.ValidateReapable(schedgate.Policy{
		Overlap:       overlap,
		MaxConcurrent: req.MaxConcurrent,
		Guard:         req.Guard,
		GuardTimeout:  req.GuardTimeout,
		GuardVar:      req.GuardVar,
		StaleAfter:    req.StaleAfter,
	}, false); err != nil {
		httpError(w, http.StatusBadRequest, "%s", err.Error())
		return
	}
	if err := s.cloudGuardRefusal(req.Guard); err != nil {
		httpError(w, http.StatusUnprocessableEntity, "%s", err.Error())
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
	routing, _, routeErr := decodeScheduleRouting(req.Routing)
	if routeErr != nil {
		httpError(w, routeErr.status, "%s", routeErr.err.Error())
		return
	}
	createPool, _, poolErr := s.decodeAndValidatePoolBlock(r.Context(), teamID, req.Pool)
	if poolErr != nil {
		httpError(w, http.StatusBadRequest, "%s", poolErr.Error())
		return
	}
	if !s.schedulePoolBindingAdmitted(w, r, id, teamID, createPool) {
		return
	}
	if err := s.validateScheduleKeyPins(r.Context(), teamID, botID, req.KeyOverrides); err != nil {
		httpError(w, http.StatusBadRequest, "%s", err.Error())
		return
	}
	var keyPins map[string]string
	if len(req.KeyOverrides) > 0 {
		keyPins = req.KeyOverrides
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
		Routing:           routing,
		Pool:              createPool,
		KeyOverrides:      keyPins,
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
		"routing_set": sb.Routing != nil, "key_overrides_set": len(sb.KeyOverrides) > 0,
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
	if normalizeRepoURL(repoURL) == "" || s.forgeIntegrations == nil || s.forgeConnections == nil {
		return ""
	}
	integrations, err := s.forgeIntegrations.ListByTenant(ctx, teamID)
	if err != nil {
		return ""
	}
	for _, ri := range integrations {
		conn, cerr := s.forgeConnections.Get(ctx, teamID, ri.ConnectionID)
		// A watch-only connection's row is not a lifecycle anchor: the
		// schedule would be deleted with the vulnerability watch.
		if cerr != nil || conn.IsSecurityReadOnly() {
			continue
		}
		if repoURLNamesRepo(repoURL, conn.BaseURL(), ri.RepoFullName) {
			return ri.ID
		}
	}
	return ""
}

// repoURLNamesRepo reports whether a schedule's repo_url names the
// repository fullName of the forge served at baseURL, read through
// normalizeRepoURL. It is the one predicate the create route binds a
// schedule to an integration by (resolveRepoIntegrationID) and the tick
// names the integration's repository by (scheduledProjectPath), so a URL
// bound to an integration is stamped with that integration's repository
// whatever its spelling.
func repoURLNamesRepo(repoURL, baseURL, fullName string) bool {
	target := normalizeRepoURL(repoURL)
	return target != "" && normalizeRepoURL(forge.CloneURLFor(baseURL, fullName)) == target
}

// repoOnForgeBase reads a repo_url against a forge base through the same
// normalizeRepoURL: ok is false when the URL is not hosted under that base —
// another host, a userinfo other than the git@ of an ssh URL, a port the
// base does not carry — and slug is the URL's repository path under the
// base, case-folded by the normalisation, when it is. Every URL
// repoURLNamesRepo matches for that base is on it: such a URL normalises to
// the base's own normalisation followed by the repository.
func repoOnForgeBase(repoURL, baseURL string) (slug string, ok bool) {
	base, target := normalizeRepoURL(baseURL), normalizeRepoURL(repoURL)
	if base == "" || !strings.HasPrefix(target, base+"/") {
		return "", false
	}
	return forge.CanonicalRepoSlug(strings.TrimPrefix(target, base+"/")), true
}

// normalizeRepoURL canonicalizes a repo URL for equality: lowercase,
// scheme-less, no trailing ".git" or "/". The scp-like form's colon is its
// path separator (git@host:owner/repo → host/owner/repo); a scheme URL's
// colon is its port and stays in the host (https://host:8443/owner/repo →
// host:8443/owner/repo), so a URL on another port of the host never reads as
// the host's own.
func normalizeRepoURL(u string) string {
	u = strings.ToLower(strings.TrimSpace(u))
	scheme := false
	for _, p := range []string{"https://", "http://", "ssh://"} {
		if strings.HasPrefix(u, p) {
			u, scheme = u[len(p):], true
		}
	}
	u = strings.TrimPrefix(u, "git@")
	if !scheme {
		u = strings.Replace(u, ":", "/", 1)
	}
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
	if err := schedgate.ValidateReapable(merged, false); err != nil {
		httpError(w, http.StatusBadRequest, "%s", err.Error())
		return
	}
	// Only a patch that CHANGES the guard is refused. A row carrying one from
	// before stays editable — pausing it must keep working — and its guard
	// does not run (cloudScheduleGate). The comparison is against the stored
	// value, not against "the field is present": every edit dialog sends the
	// whole policy back, the stored guard included, so refusing on presence
	// refuses the very edits this exemption exists for.
	if g := req.Guard; g != nil && strings.TrimSpace(*g) != strings.TrimSpace(cur.Guard) {
		if err := s.cloudGuardRefusal(*g); err != nil {
			httpError(w, http.StatusUnprocessableEntity, "%s", err.Error())
			return
		}
	}
	// Routing: absent leaves the stored block (edit dialogs mirror the row
	// back), null clears, an object replaces wholesale — decoded strictly
	// and validated before it lands.
	patchRouting, routingSpoke, routeErr := decodeScheduleRouting(req.Routing)
	if routeErr != nil {
		httpError(w, routeErr.status, "%s", routeErr.err.Error())
		return
	}
	clearRouting := patchRouting == nil && routingSpoke
	// The pool block (ADR-164): absent leaves the stored block, null
	// clears (the schedule inherits again), an object replaces — validated
	// at write time like the create lane (shape, the pool's existence and
	// consent).
	patchPool, poolSpoke, poolErr := decodePoolBlock(req.Pool)
	if poolErr != nil {
		httpError(w, http.StatusBadRequest, "%s", poolErr.Error())
		return
	}
	// An edit dialog sends the whole record back: echoing the stored block
	// unchanged is no pool write at all — the row keeps its block AND its
	// provenance (a projected block stays the integration's), and the
	// write validation and the review gate judge only a CHANGE (a stored
	// block whose pool was disabled since must not refuse a pause).
	if patchPool.Equal(cur.Pool) {
		patchPool, poolSpoke = nil, false
	}
	if err := s.validatePoolBindingWrite(r.Context(), teamID, patchPool); err != nil {
		httpError(w, http.StatusBadRequest, "%s", err.Error())
		return
	}
	if !s.schedulePoolBindingAdmitted(w, r, id, teamID, patchPool) {
		return
	}
	clearPool := patchPool == nil && poolSpoke
	// Key pins: absent leaves the stored pins, null or {} clears, an object
	// replaces wholesale. An edit dialog echoing the stored pins back writes
	// nothing and is not judged — a pin whose key was deleted since must not
	// refuse a pause; the tick refuses it by name (judgeScheduleKeyPins).
	patchKeyPins, keyPinsSpoke, keyPinsErr := decodeScheduleKeyPins(req.KeyOverrides)
	if keyPinsErr != nil {
		httpError(w, http.StatusBadRequest, "%s", keyPinsErr.Error())
		return
	}
	if keyPinsSpoke && maps.Equal(patchKeyPins, cur.KeyOverrides) {
		patchKeyPins, keyPinsSpoke = nil, false
	}
	if keyPinsSpoke {
		if err := s.validateScheduleKeyPins(r.Context(), teamID, cur.BotID, patchKeyPins); err != nil {
			httpError(w, http.StatusBadRequest, "%s", err.Error())
			return
		}
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
	patch.Routing = patchRouting
	patch.ClearRouting = clearRouting
	patch.Pool = patchPool
	patch.ClearPool = clearPool
	if keyPinsSpoke {
		patch.KeyOverrides = &patchKeyPins
	}
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
		"routing_set": updated.Routing != nil, "routing_changed": len(req.Routing) > 0,
		"key_overrides_set": len(updated.KeyOverrides) > 0, "key_overrides_changed": keyPinsSpoke,
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

// schedulePoolBindingAdmitted gates a schedule write that binds a LIVE pool
// behind the org's provisioning review, as the repo-bots API does: a
// recurring unattended run drawing a shared pool is exactly what an org
// requiring approval reviews. A schedule has no parking lane, so an org
// that would review the binding refuses it here (409) — an org admin binds
// it, or the binding rides an integration through the repo-bots API. An
// opt-out block binds nothing and needs no review. Writes the refusal and
// returns false when the write must not proceed.
func (s *Server) schedulePoolBindingAdmitted(w http.ResponseWriter, r *http.Request, id auth.Identity, teamID string, pool *credpool.Binding) bool {
	if pool == nil || !pool.Enabled {
		return true
	}
	gateOrg, err := s.provisionOrgRequiringApproval(r.Context(), id, teamID, true)
	if err != nil {
		httpError(w, http.StatusServiceUnavailable, "provision-approval gate unavailable: %v", err)
		return false
	}
	if gateOrg != "" {
		httpError(w, http.StatusConflict,
			"this org reviews shared-pool bindings: an org admin binds pool %q on this schedule, or bind it through the repo-bots API (/api/teams/{id}/forge/repo-bots), which queues it for an org admin", pool.PoolID)
		return false
	}
	return true
}

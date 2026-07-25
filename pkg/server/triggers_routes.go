package server

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/SocialGouv/iterion/pkg/auth"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dispatcher"
	"github.com/SocialGouv/iterion/pkg/schedgate"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// registerTriggerRoutes wires the event-driven trigger subscription CRUD under
// /api/v1/triggers. Tenant scope follows the request: the JWT's active team
// in cloud mode, the local single-host scope ("") otherwise — mirroring
// cloudBoardResolve. All routes go through requireAuth so a non-loopback
// bind can't mutate automation wiring unauthenticated, mirroring the
// native/dispatcher route gating.
func (s *Server) registerTriggerRoutes() {
	s.mux.Handle("GET /api/v1/triggers", s.requireAuth(http.HandlerFunc(s.handleListTriggers)))
	s.mux.Handle("POST /api/v1/triggers", s.requireAuth(http.HandlerFunc(s.handleCreateTrigger)))
	s.mux.Handle("GET /api/v1/triggers/{id}", s.requireAuth(http.HandlerFunc(s.handleGetTrigger)))
	s.mux.Handle("PUT /api/v1/triggers/{id}", s.requireAuth(http.HandlerFunc(s.handleUpdateTrigger)))
	s.mux.Handle("DELETE /api/v1/triggers/{id}", s.requireAuth(http.HandlerFunc(s.handleDeleteTrigger)))
	s.mux.Handle("POST /api/v1/triggers/emit", s.requireAuth(http.HandlerFunc(s.handleEmitTrigger)))
	s.mux.Handle("POST /api/v1/bots/{name}/triggers/from-invocation", s.requireAuth(http.HandlerFunc(s.handleTriggerFromInvocation)))
	s.mux.Handle("GET /api/v1/triggers/health", s.requireAuth(http.HandlerFunc(s.handleTriggersHealth)))
}

// handleTriggersHealth exposes the schedule-scheduler's liveness
// snapshot so a wedged loop is observable (frozen last_tick_at)
// instead of silently never firing. The dispatcher's twin lives on
// /api/dispatcher/snapshot (last_tick_at field).
func (s *Server) handleTriggersHealth(w http.ResponseWriter, r *http.Request) {
	st := s.triggerCoord.SchedulerStatus()
	dispatcher.WriteJSON(w, http.StatusOK, map[string]any{
		"scheduler_running": s.triggerCoord.SchedulerRunning(),
		"scheduler":         st,
	})
}

// triggerTenant resolves the tenant scope of a trigger-route request: the
// JWT's active team in cloud mode, "" in the local single-host scope (where
// the identity carries no team).
func (s *Server) triggerTenant(r *http.Request) string {
	id, _ := auth.FromContext(r.Context())
	return id.TeamID
}

// triggerFromInvocationReq selects one of the bot's manifest-declared
// invocations by index. Cron optionally overrides a schedule invocation's
// suggested_cron (the bot home lets the operator retune before enabling).
type triggerFromInvocationReq struct {
	Index int    `json:"index"`
	Cron  string `json:"cron,omitempty"`
}

// handleTriggerFromInvocation is the bot home's one-click "enable this
// trigger": it derives a trigger.Subscription from the bot's manifest
// invocation via the same From* constructors the provisioning paths use
// (single source of truth — the derivation never lives client-side).
// Duplicate protection: an existing bot-home subscription for the same
// bot+kind is a 409 carrying the existing id.
func (s *Server) handleTriggerFromInvocation(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PathValue("name"))
	entry, ok, err := s.findBot(name)
	if err != nil {
		dispatcher.WriteErr(w, http.StatusInternalServerError, err)
		return
	}
	if !ok {
		dispatcher.WriteErr(w, http.StatusNotFound, fmt.Errorf("bots: %q not found", name))
		return
	}
	var req triggerFromInvocationReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Index < 0 || req.Index >= len(entry.Invocations) {
		dispatcher.WriteErr(w, http.StatusBadRequest,
			fmt.Errorf("invocation index %d out of range (bot declares %d)", req.Index, len(entry.Invocations)))
		return
	}
	inv := entry.Invocations[req.Index]
	if req.Cron != "" {
		if inv.Kind != bundle.InvocationKindSchedule || inv.Schedule == nil {
			dispatcher.WriteErr(w, http.StatusBadRequest, errors.New("cron override only applies to a schedule invocation"))
			return
		}
		if fields := strings.Fields(req.Cron); len(fields) != 5 {
			dispatcher.WriteErr(w, http.StatusBadRequest, fmt.Errorf("cron %q must be a 5-field expression", req.Cron))
			return
		}
		schedule := *inv.Schedule
		schedule.SuggestedCron = req.Cron
		inv.Schedule = &schedule
	}

	now := time.Now().UTC()
	id := uuid.NewString()
	tenant := s.triggerTenant(r)
	var (
		sub     trigger.Subscription
		derived bool
	)
	switch inv.Kind {
	case bundle.InvocationKindSchedule:
		sub, derived = trigger.FromScheduleInvocation(id, tenant, "", entry.Name, botHomeTriggerOrigin, inv, now)
		if !derived {
			dispatcher.WriteErr(w, http.StatusBadRequest, errors.New("schedule invocation has no suggested_cron — pass cron explicitly"))
			return
		}
	case bundle.InvocationKindBoard:
		sub, derived = trigger.FromBoardInvocation(id, tenant, "", entry.Name, botHomeTriggerOrigin, inv, now)
		if !derived {
			dispatcher.WriteErr(w, http.StatusBadRequest,
				errors.New("board invocation has no board: block — it is a plain dispatcher target, nothing to subscribe"))
			return
		}
	case bundle.InvocationKindKeepalive:
		sub, derived = trigger.FromKeepaliveInvocation(id, tenant, "", entry.Name, botHomeTriggerOrigin, inv, now)
		if !derived {
			dispatcher.WriteErr(w, http.StatusBadRequest,
				errors.New("keepalive invocation has no valid interval — check the keepalive: block"))
			return
		}
	default:
		dispatcher.WriteErr(w, http.StatusBadRequest,
			fmt.Errorf("invocation kind %q is not enabled from here — wire it through the forge integration flow", inv.Kind))
		return
	}

	existing, err := s.cfg.TriggerStore.ListByBot(r.Context(), tenant, entry.Name)
	if err != nil {
		dispatcher.WriteErr(w, http.StatusInternalServerError, err)
		return
	}
	for _, e := range existing {
		if e.Origin == botHomeTriggerOrigin && e.Invocation == sub.Invocation {
			dispatcher.WriteJSON(w, http.StatusConflict, map[string]any{
				"error":           fmt.Sprintf("a %s trigger from this bot's manifest already exists", sub.Invocation),
				"subscription_id": e.ID,
			})
			return
		}
	}
	if err := s.cfg.TriggerStore.Create(r.Context(), sub); err != nil {
		dispatcher.WriteErr(w, http.StatusInternalServerError, err)
		return
	}
	dispatcher.WriteJSON(w, http.StatusCreated, sub)
}

// botHomeTriggerOrigin marks subscriptions created by the bot home's
// one-click invocation enablement — the dedup key distinguishing them
// from hand-authored ("operator") subscriptions.
const botHomeTriggerOrigin = "bot-home"

// maxEmitVarsBytes caps the cumulative size of the custom-emit Vars map.
// The endpoint is authenticated, but a buggy/abusive integration could
// otherwise POST gigabytes of var data that fan out onto the bus and
// into every launched run — a 400 is cheaper than the memory pressure.
const maxEmitVarsBytes = 1 << 20 // 1 MiB

// emitTriggerReq is the custom-integration ingress payload: an arbitrary
// external system injects an event onto the spine, and matching custom
// subscriptions fire. Source is always forced to "custom" — the endpoint
// cannot spoof a board/forge/run event.
type emitTriggerReq struct {
	Kind    string            `json:"kind"`
	Action  string            `json:"action,omitempty"`
	Repo    string            `json:"repo,omitempty"`
	Actor   string            `json:"actor,omitempty"`
	Labels  []string          `json:"labels,omitempty"`
	Vars    map[string]string `json:"vars,omitempty"`
	Subject struct {
		Type  string `json:"type,omitempty"`
		ID    string `json:"id,omitempty"`
		URL   string `json:"url,omitempty"`
		Ref   string `json:"ref,omitempty"`
		Title string `json:"title,omitempty"`
		Body  string `json:"body,omitempty"`
		State string `json:"state,omitempty"`
	} `json:"subject,omitempty"`
}

// handleEmitTrigger is the custom-integration ingress. It publishes a
// SourceCustom event onto the trigger bus; matching custom subscriptions fire
// asynchronously (the same fire-and-observe model as board events). Returns
// 202 — the launch, if any, happens via the evaluator, so no run_id is
// returned synchronously (use a direct webhook for a synchronous launch).
func (s *Server) handleEmitTrigger(w http.ResponseWriter, r *http.Request) {
	bus := s.cfg.EventsBus
	if bus == nil && s.triggerCoord != nil {
		bus = s.triggerCoord.Bus()
	}
	if bus == nil {
		dispatcher.WriteErr(w, http.StatusServiceUnavailable, errors.New("trigger spine not enabled"))
		return
	}
	var req emitTriggerReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Kind == "" {
		dispatcher.WriteErr(w, http.StatusBadRequest, errors.New("kind is required"))
		return
	}
	// Run-launch admission. A custom emit can fan out to N matching
	// subscriptions, each a launch — gate it exactly like the inbound
	// webhook path so an authenticated integration can't bypass the
	// per-org quota / cost cap / rate limit. Fail-open (nil) in local
	// single-host scope, so this is a no-op there.
	if _, d := s.gateLaunch(r.Context()); d != nil {
		s.writeLaunchDenial(w, r, d)
		return
	}
	payload := map[string]any{}
	if len(req.Vars) > 0 {
		total := 0
		vm := make(map[string]any, len(req.Vars))
		for k, v := range req.Vars {
			total += len(k) + len(v)
			if total > maxEmitVarsBytes {
				dispatcher.WriteErr(w, http.StatusBadRequest, errors.New("vars payload too large"))
				return
			}
			vm[k] = v
		}
		payload[trigger.PayloadVars] = vm
	}
	// Idempotency id. When the caller omits Subject.ID the natural key
	// "custom:<kind>:" collides across every event of that kind — fall
	// back to a unique id so distinct events stay distinct (the forge
	// source's launched_run_id marker relies on a stable, per-event id).
	subjectID := req.Subject.ID
	eventID := "custom:" + req.Kind + ":" + subjectID
	if subjectID == "" {
		eventID = "custom:" + req.Kind + ":" + uuid.NewString()
	}
	ev := trigger.Event{
		ID:       eventID,
		TenantID: s.triggerTenant(r),
		Source:   trigger.SourceCustom,
		Kind:     req.Kind,
		Action:   req.Action,
		Repo:     req.Repo,
		Actor:    req.Actor,
		Labels:   req.Labels,
		Subject: trigger.Subject{
			Type: req.Subject.Type, ID: req.Subject.ID, URL: req.Subject.URL,
			Ref: req.Subject.Ref, Title: req.Subject.Title, Body: req.Subject.Body, State: req.Subject.State,
		},
		Payload:    payload,
		OccurredAt: time.Now().UTC(),
	}
	if err := bus.Publish(r.Context(), ev); err != nil {
		dispatcher.WriteErr(w, http.StatusInternalServerError, err)
		return
	}
	dispatcher.WriteJSON(w, http.StatusAccepted, map[string]any{"status": "accepted", "event_id": ev.ID})
}

// triggerSubscriptionReq is the create/update payload. It mirrors
// trigger.Subscription minus server-managed fields (id, timestamps, origin).
type triggerSubscriptionReq struct {
	Repo       string            `json:"repo,omitempty"`
	BotID      string            `json:"bot_id"`
	Invocation string            `json:"invocation,omitempty"`
	Mode       string            `json:"mode,omitempty"`
	Match      trigger.Matcher   `json:"match"`
	Vars       map[string]string `json:"vars,omitempty"`
	ArgsVar    string            `json:"args_var,omitempty"`
	Cron       string            `json:"cron,omitempty"`
	Enabled    *bool             `json:"enabled,omitempty"`
	// Overlap policy + guard for schedule-kind subscriptions
	// (pkg/schedgate; validated on create/update).
	Overlap       string `json:"overlap,omitempty"`
	MaxConcurrent int    `json:"max_concurrent,omitempty"`
	Guard         string `json:"guard,omitempty"`
	GuardTimeout  string `json:"guard_timeout,omitempty"`
	GuardVar      string `json:"guard_var,omitempty"`
}

// validatePolicy rejects incoherent schedgate fields with a 400-worthy error.
func (r triggerSubscriptionReq) validatePolicy() error {
	return schedgate.Validate(schedgate.Policy{
		Overlap:       r.Overlap,
		MaxConcurrent: r.MaxConcurrent,
		Guard:         r.Guard,
		GuardTimeout:  r.GuardTimeout,
		GuardVar:      r.GuardVar,
	})
}

func (s *Server) handleListTriggers(w http.ResponseWriter, r *http.Request) {
	// Optional ?repo= / ?bot= filters drive the "by repo" / "by bot" views;
	// no filter returns the whole local scope.
	q := r.URL.Query()
	var (
		subs []trigger.Subscription
		err  error
	)
	tenant := s.triggerTenant(r)
	switch {
	case q.Get("repo") != "":
		subs, err = s.cfg.TriggerStore.ListByRepo(r.Context(), tenant, q.Get("repo"))
	case q.Get("bot") != "":
		subs, err = s.cfg.TriggerStore.ListByBot(r.Context(), tenant, q.Get("bot"))
	default:
		subs, err = s.cfg.TriggerStore.ListByTenant(r.Context(), tenant)
	}
	if err != nil {
		dispatcher.WriteErr(w, http.StatusInternalServerError, err)
		return
	}
	if subs == nil {
		subs = []trigger.Subscription{}
	}
	dispatcher.WriteJSON(w, http.StatusOK, map[string]any{"subscriptions": subs})
}

func (s *Server) handleGetTrigger(w http.ResponseWriter, r *http.Request) {
	sub, err := s.cfg.TriggerStore.Get(r.Context(), r.PathValue("id"))
	if err == nil && sub.TenantID != s.triggerTenant(r) {
		err = trigger.ErrSubscriptionNotFound
	}
	if errors.Is(err, trigger.ErrSubscriptionNotFound) {
		dispatcher.WriteErr(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		dispatcher.WriteErr(w, http.StatusInternalServerError, err)
		return
	}
	dispatcher.WriteJSON(w, http.StatusOK, sub)
}

func (s *Server) handleCreateTrigger(w http.ResponseWriter, r *http.Request) {
	var req triggerSubscriptionReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.BotID == "" {
		dispatcher.WriteErr(w, http.StatusBadRequest, errors.New("bot_id is required"))
		return
	}
	if err := req.validatePolicy(); err != nil {
		dispatcher.WriteErr(w, http.StatusBadRequest, err)
		return
	}
	now := time.Now().UTC()
	sub := applyTriggerReq(trigger.Subscription{
		ID:        uuid.NewString(),
		TenantID:  s.triggerTenant(r),
		Origin:    "operator",
		Enabled:   true,
		CreatedAt: now,
		UpdatedAt: now,
	}, req)
	if err := s.cfg.TriggerStore.Create(r.Context(), sub); err != nil {
		dispatcher.WriteErr(w, http.StatusInternalServerError, err)
		return
	}
	dispatcher.WriteJSON(w, http.StatusCreated, sub)
}

func (s *Server) handleUpdateTrigger(w http.ResponseWriter, r *http.Request) {
	cur, err := s.cfg.TriggerStore.Get(r.Context(), r.PathValue("id"))
	if err == nil && cur.TenantID != s.triggerTenant(r) {
		err = trigger.ErrSubscriptionNotFound
	}
	if errors.Is(err, trigger.ErrSubscriptionNotFound) {
		dispatcher.WriteErr(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		dispatcher.WriteErr(w, http.StatusInternalServerError, err)
		return
	}
	var req triggerSubscriptionReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := req.validatePolicy(); err != nil {
		dispatcher.WriteErr(w, http.StatusBadRequest, err)
		return
	}
	cur = applyTriggerReq(cur, req)
	cur.UpdatedAt = time.Now().UTC()
	if err := s.cfg.TriggerStore.Update(r.Context(), cur); err != nil {
		dispatcher.WriteErr(w, http.StatusInternalServerError, err)
		return
	}
	dispatcher.WriteJSON(w, http.StatusOK, cur)
}

func (s *Server) handleDeleteTrigger(w http.ResponseWriter, r *http.Request) {
	cur, err := s.cfg.TriggerStore.Get(r.Context(), r.PathValue("id"))
	if err == nil && cur.TenantID != s.triggerTenant(r) {
		err = trigger.ErrSubscriptionNotFound
	}
	if err == nil {
		err = s.cfg.TriggerStore.Delete(r.Context(), r.PathValue("id"))
	}
	if errors.Is(err, trigger.ErrSubscriptionNotFound) {
		dispatcher.WriteErr(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		dispatcher.WriteErr(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// applyTriggerReq folds a request onto a base subscription, preserving
// server-managed fields (id, origin, created_at). Enabled defaults to the
// base value unless the request explicitly sets it.
func applyTriggerReq(base trigger.Subscription, req triggerSubscriptionReq) trigger.Subscription {
	base.Repo = req.Repo
	base.BotID = req.BotID
	if req.Invocation != "" {
		base.Invocation = bundle.InvocationKind(req.Invocation)
	}
	base.Mode = bundle.ExecutionMode(req.Mode)
	base.Match = req.Match
	base.Vars = req.Vars
	base.ArgsVar = req.ArgsVar
	base.Cron = req.Cron
	base.Overlap = req.Overlap
	base.MaxConcurrent = req.MaxConcurrent
	base.Guard = req.Guard
	base.GuardTimeout = req.GuardTimeout
	base.GuardVar = req.GuardVar
	if req.Enabled != nil {
		base.Enabled = *req.Enabled
	}
	return base
}

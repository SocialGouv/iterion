package server

import (
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dispatcher"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// registerTriggerRoutes wires the event-driven trigger subscription CRUD under
// /api/v1/triggers. Local single-host scope (tenant ""); the cloud
// multi-tenant variant is a team-scoped follow-on. All routes go through
// requireAuth so a non-loopback bind can't mutate automation wiring
// unauthenticated, mirroring the native/dispatcher route gating.
func (s *Server) registerTriggerRoutes() {
	s.mux.Handle("GET /api/v1/triggers", s.requireAuth(http.HandlerFunc(s.handleListTriggers)))
	s.mux.Handle("POST /api/v1/triggers", s.requireAuth(http.HandlerFunc(s.handleCreateTrigger)))
	s.mux.Handle("GET /api/v1/triggers/{id}", s.requireAuth(http.HandlerFunc(s.handleGetTrigger)))
	s.mux.Handle("PUT /api/v1/triggers/{id}", s.requireAuth(http.HandlerFunc(s.handleUpdateTrigger)))
	s.mux.Handle("DELETE /api/v1/triggers/{id}", s.requireAuth(http.HandlerFunc(s.handleDeleteTrigger)))
	s.mux.Handle("POST /api/v1/triggers/emit", s.requireAuth(http.HandlerFunc(s.handleEmitTrigger)))
}

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
	if s.triggerCoord == nil || s.triggerCoord.Bus() == nil {
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
	if d := s.gateLaunch(r.Context()); d != nil {
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
		ID:     eventID,
		Source: trigger.SourceCustom,
		Kind:   req.Kind,
		Action: req.Action,
		Repo:   req.Repo,
		Actor:  req.Actor,
		Labels: req.Labels,
		Subject: trigger.Subject{
			Type: req.Subject.Type, ID: req.Subject.ID, URL: req.Subject.URL,
			Ref: req.Subject.Ref, Title: req.Subject.Title, Body: req.Subject.Body, State: req.Subject.State,
		},
		Payload:    payload,
		OccurredAt: time.Now().UTC(),
	}
	if err := s.triggerCoord.Bus().Publish(r.Context(), ev); err != nil {
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
}

func (s *Server) handleListTriggers(w http.ResponseWriter, r *http.Request) {
	// Optional ?repo= / ?bot= filters drive the "by repo" / "by bot" views;
	// no filter returns the whole local scope.
	q := r.URL.Query()
	var (
		subs []trigger.Subscription
		err  error
	)
	switch {
	case q.Get("repo") != "":
		subs, err = s.cfg.TriggerStore.ListByRepo(r.Context(), "", q.Get("repo"))
	case q.Get("bot") != "":
		subs, err = s.cfg.TriggerStore.ListByBot(r.Context(), "", q.Get("bot"))
	default:
		subs, err = s.cfg.TriggerStore.ListByTenant(r.Context(), "")
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
	now := time.Now().UTC()
	sub := applyTriggerReq(trigger.Subscription{
		ID:        uuid.NewString(),
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
	cur = applyTriggerReq(cur, req)
	cur.UpdatedAt = time.Now().UTC()
	if err := s.cfg.TriggerStore.Update(r.Context(), cur); err != nil {
		dispatcher.WriteErr(w, http.StatusInternalServerError, err)
		return
	}
	dispatcher.WriteJSON(w, http.StatusOK, cur)
}

func (s *Server) handleDeleteTrigger(w http.ResponseWriter, r *http.Request) {
	err := s.cfg.TriggerStore.Delete(r.Context(), r.PathValue("id"))
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
	if req.Enabled != nil {
		base.Enabled = *req.Enabled
	}
	return base
}

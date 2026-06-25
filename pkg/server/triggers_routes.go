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

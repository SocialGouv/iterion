package server

import (
	"bytes"
	"encoding/json"
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
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// triggerWriteRungDetail is what a refused automation write is told.
const triggerWriteRungDetail = "changing this workspace's automation takes at least the member role — a trigger subscription launches runs the platform performs on its own; ask a workspace admin to raise your role"

// triggerWriteAuth is the wrapper every trigger route mounts through — reads
// included, where it is inert. It is the trigger family's instance of the
// member rung (mutatingRungAuth, board_cloud_routes.go), owed here for the
// same reason the board family owes it: a stored subscription is a standing
// launch order, and the spine that executes it launches under a machine
// principal (machineLaunchCtx, via serviceLauncher) the gate exempts — so the
// authorization is owed where the subscription is WRITTEN. Disabling one is a
// write too: a viewer must not be able to silence a team's automation.
func (s *Server) triggerWriteAuth(next http.Handler) http.Handler {
	return s.mutatingRungAuth(triggerWriteRungDetail)(next)
}

// triggerWriteDenial is the trigger family's instance of the rung decision.
func (s *Server) triggerWriteDenial(r *http.Request) *launchDenial {
	return s.mutatingRungDenial(r, triggerWriteRungDetail)
}

// triggerPinRightDetail is what a refused credential decision is told.
const triggerPinRightDetail = "changing a trigger subscription's credential pins, or what a pinned subscription launches, takes a team admin: key_overrides / secret_overrides choose which stored api key or secret the platform spends on every fire — the right a webhook's pins and the team's own keys and secrets take. A member may still pause, resume or delete a pinned subscription, and edit any unpinned one"

// authorizeTriggerCredentials is the trigger routes' second rung: a write
// that decides a subscription's credentials (decidesTriggerCredentials)
// takes the team admin's right — canManageTeam, the right that writes a
// webhook's pins and creates the team's keys and secrets. The member rung at
// the mount admits automation work, and a pin is more than that: the spine
// spends it under triggerSpineActor, a machine principal no rung reads, so
// the right is owed where the pin is written, as the member rung is owed
// where the subscription is. Without it a member could bind any org-scoped
// secret of the team — a project bot's managed forge token included — to a
// workflow secret slot of a bot of their choosing, or point an admin's pin
// at one.
//
// Local mode (no identity store) has no roles to read — the arm
// mutatingRungDenial opens with. A right the identity store could not judge
// (an outage, not a refusal) answers 5xx with its cause, never a 403 telling
// an admin they are not one. Reports whether the write may proceed, having
// written the refusal when it may not.
func (s *Server) authorizeTriggerCredentials(w http.ResponseWriter, r *http.Request, tenant string, before *trigger.Subscription, after trigger.Subscription) bool {
	if s.authStore() == nil {
		return true
	}
	decides, err := decidesTriggerCredentials(before, after)
	if err != nil {
		dispatcher.WriteErr(w, http.StatusInternalServerError, fmt.Errorf("compare the subscription with its stored row: %w", err))
		return false
	}
	if !decides {
		return true
	}
	id, _ := auth.FromContext(r.Context())
	allowed, err := s.canManageTeamOrFail(r.Context(), id, tenant)
	if allowed {
		return true
	}
	if err != nil {
		dispatcher.WriteErr(w, http.StatusInternalServerError, fmt.Errorf("judge the right to change the credential pins of a trigger subscription in %s: %w", tenant, err))
		return false
	}
	dispatcher.WriteErr(w, http.StatusForbidden, errors.New(triggerPinRightDetail))
	return false
}

// decidesTriggerCredentials reports whether storing after over before (nil
// on a create) is a credential decision: the choice of which stored api key
// or secret a standing launch spends, and on what. A create decides when it
// pins anything. An edit decides when the row holds a pin before or after
// it and the write changes anything but the enabled flag — the pins
// themselves (an add, a replace, a clear) or what a pinned credential is
// spent on: the bot, the repo, the vars, the match, the cadence. Pausing or
// resuming a pinned subscription decides none of that, and a row that pins
// nothing on either side has no credential to decide.
func decidesTriggerCredentials(before *trigger.Subscription, after trigger.Subscription) (bool, error) {
	if before == nil {
		return holdsCredentialPin(after), nil
	}
	if !holdsCredentialPin(*before) && !holdsCredentialPin(after) {
		return false, nil
	}
	same, err := sameTriggerLaunch(*before, after)
	return !same, err
}

// holdsCredentialPin reports whether the row pins a key or a secret.
func holdsCredentialPin(sub trigger.Subscription) bool {
	return len(sub.KeyOverrides) > 0 || len(sub.SecretOverrides) > 0
}

// sameTriggerLaunch reports whether two versions of a row launch the same
// thing: equal in every field but the enabled flag and the update stamp. It
// compares the rows' JSON, the shape the routes read and write, so a nil map
// or slice and an empty one compare equal — omitempty drops both, and the
// studio's toggle round-trips the stored row through exactly that shape —
// and a field added to trigger.Subscription is compared without a line here.
func sameTriggerLaunch(a, b trigger.Subscription) (bool, error) {
	b.Enabled, b.UpdatedAt = a.Enabled, a.UpdatedAt
	ja, err := json.Marshal(a)
	if err != nil {
		return false, err
	}
	jb, err := json.Marshal(b)
	if err != nil {
		return false, err
	}
	return bytes.Equal(ja, jb), nil
}

// registerTriggerRoutes wires the event-driven trigger subscription CRUD under
// /api/v1/triggers. Tenant scope follows the request: the JWT's active team
// in cloud mode, the local single-host scope ("") otherwise — mirroring
// cloudBoardResolve. All routes go through triggerWriteAuth: requireAuth, so a
// non-loopback bind can't mutate automation wiring unauthenticated (mirroring
// the native/dispatcher route gating), plus the member rung on every
// state-changing method. The reads carry it too, where it is inert — so every
// line of this registrar reads the same, and a route added with requireAuth
// alone stands out. The create and update handlers add the team admin's rung
// for a write that decides a subscription's credentials
// (authorizeTriggerCredentials) — a pin is not automation work but a choice
// of which stored credential the platform spends.
//
// Every route mounts on s.mux, the RECORDING mux: its routing table is what
// /api/openapi.json publishes, and the class witness
// (trigger_write_rung_test.go) holds this file's own registration calls
// against that table — a route handed s.mux.ServeMux is absent from the
// table and reddens there, and a registration spelled in a form the witness
// cannot parse reddens on its count.
func (s *Server) registerTriggerRoutes() {
	s.mux.Handle("GET /api/v1/triggers", s.triggerWriteAuth(http.HandlerFunc(s.handleListTriggers)))
	s.mux.Handle("POST /api/v1/triggers", s.triggerWriteAuth(http.HandlerFunc(s.handleCreateTrigger)))
	s.mux.Handle("GET /api/v1/triggers/{id}", s.triggerWriteAuth(http.HandlerFunc(s.handleGetTrigger)))
	s.mux.Handle("PUT /api/v1/triggers/{id}", s.triggerWriteAuth(http.HandlerFunc(s.handleUpdateTrigger)))
	s.mux.Handle("DELETE /api/v1/triggers/{id}", s.triggerWriteAuth(http.HandlerFunc(s.handleDeleteTrigger)))
	s.mux.Handle("POST /api/v1/triggers/emit", s.triggerWriteAuth(http.HandlerFunc(s.handleEmitTrigger)))
	s.mux.Handle("POST /api/v1/bots/{name}/triggers/from-invocation", s.triggerWriteAuth(http.HandlerFunc(s.handleTriggerFromInvocation)))
	s.mux.Handle("GET /api/v1/triggers/health", s.triggerWriteAuth(http.HandlerFunc(s.handleTriggersHealth)))
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
	// The subscription this derives launches through the trigger spine, which
	// resolves the tenant's own tier — so the invocation it is derived FROM
	// comes from the same tier, or the binding describes a bundle that will
	// not run (and a bot the team authored is not enable-able at all).
	tenant := s.triggerTenant(r)
	// The manifest the subscription is derived from is resolved under the
	// bot tree's write lock, held through the subscription's write: never a
	// catalog read between another writer's two moves of that manifest.
	release, locked := s.lockBotTree(w, r, botTreeWriterTriggerInvocation)
	if !locked {
		return
	}
	defer release()
	entry, ok, err := s.effectiveFindByNameForTeam(r.Context(), tenant, name)
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
	// Run-launch admission as a PRE-CHECK: a suspended org, an org at its
	// concurrency cap or over its monthly caps is refused here and now, with
	// the same envelope the other surfaces send. The metered slot is handed
	// back at once — an emit is one EVENT, and it fans out to however many
	// subscriptions match it (zero, one, ten), each of which the spine
	// launcher meters as its own launch. Fail-open (nil) in local single-host
	// scope, so this is a no-op there.
	// No subject: an emit is ONE event that fans out to however many
	// subscriptions match it, each launching its own bot into its own repo.
	// This pre-check bounds the event, not any of them — the spine launcher
	// gates each resulting launch with its own subject.
	adm, d := s.gateLaunch(r.Context(), launchSubject{})
	if d != nil {
		s.writeLaunchDenial(w, r, d)
		return
	}
	adm.rollback(s.logger)
	payload := map[string]any{}
	// A client drives this ingress, so whatever it sends under a
	// server-minted var's name is not that var: the server mints the forge
	// publish grant per run and binds the bearer to its own endpoints.
	// Dropped here rather than carried into an event every matching
	// subscription reads (the same rule every client-driven write path
	// applies — store.DropServerMintedVars).
	if emitVars := store.DropServerMintedVars(req.Vars); len(emitVars) > 0 {
		total := 0
		vm := make(map[string]any, len(emitVars))
		for k, v := range emitVars {
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
	// Credential pins, the subscription's home for a key/secret id — the
	// third place one lives (the BYOK key row and the webhook config are
	// the other two). The spine launches under triggerSpineActor, so the
	// write validates each pin against that actor: the same visibility
	// question secrets.Resolve answers at fire time, asked here where a
	// refusal is still an answer. On update a request that omits a pin
	// keeps the stored one; an empty map is the explicit clear. The webhook
	// routes share the two validators. Writing a pin — or changing what a
	// pinned subscription launches — takes the team admin's right
	// (authorizeTriggerCredentials), the right the webhook routes take.
	KeyOverrides    map[string]string `json:"key_overrides,omitempty"`
	SecretOverrides map[string]string `json:"secret_overrides,omitempty"`
	Cron            string            `json:"cron,omitempty"`
	Enabled         *bool             `json:"enabled,omitempty"`
	// Overlap policy + guard for schedule-kind subscriptions
	// (pkg/schedgate; validated on create/update).
	Overlap       string `json:"overlap,omitempty"`
	MaxConcurrent int    `json:"max_concurrent,omitempty"`
	// Pointers, like the schedule route's patch: a request that does not
	// mention a guard leaves the stored one alone. With a plain string an
	// edit that says nothing about the guard would silently drop it — and on
	// a cloud deployment, where a stored guard is refused but kept, that is
	// exactly the request an operator sends to disable such a trigger.
	Guard        *string `json:"guard,omitempty"`
	GuardTimeout *string `json:"guard_timeout,omitempty"`
	GuardVar     *string `json:"guard_var,omitempty"`
}

// guardOf returns the guard this request asks for, and whether it says
// anything about it at all.
func (r triggerSubscriptionReq) guardOf(cur string) string {
	if r.Guard == nil {
		return cur
	}
	return *r.Guard
}

// validatePolicy rejects incoherent schedgate fields with a 400-worthy error.
func (r triggerSubscriptionReq) validatePolicy() error {
	return schedgate.Validate(schedgate.Policy{
		Overlap:       r.Overlap,
		MaxConcurrent: r.MaxConcurrent,
		Guard:         r.guardOf(""),
		GuardTimeout:  deref(r.GuardTimeout),
		GuardVar:      deref(r.GuardVar),
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
	tenant := s.triggerTenant(r)
	now := time.Now().UTC()
	sub := applyTriggerReq(trigger.Subscription{
		ID:        uuid.NewString(),
		TenantID:  tenant,
		Origin:    "operator",
		Enabled:   true,
		CreatedAt: now,
		UpdatedAt: now,
	}, req)
	// Who may pin before what may be pinned: a create that pins anything is
	// a team admin's credential decision.
	if !s.authorizeTriggerCredentials(w, r, tenant, nil, sub) {
		return
	}
	// The pins answer to the actor the spine will launch under, so a pin no
	// machine lane can spend is refused here — the one place a refusal can
	// still reach the operator (an unspendable pin is invisible to
	// secrets.Resolve, which would seal the team's default in silence).
	if err := s.validateKeyOverrides(teamTenantCtx(r.Context(), tenant), tenant, triggerSpineActor, req.KeyOverrides); err != nil {
		dispatcher.WriteErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.validateSecretOverrides(teamTenantCtx(r.Context(), tenant), tenant, req.SecretOverrides); err != nil {
		dispatcher.WriteErr(w, http.StatusBadRequest, err)
		return
	}
	// The same refusal the schedule routes carry: a trigger guard is the
	// same shell command, stored through a route a cloud server mounts. Its
	// executor is not wired in cloud today, so this refuses a guard that
	// nothing would run yet — which is the point: the stored snippet must
	// not be waiting for the wiring that runs it.
	if err := s.cloudGuardRefusal(req.guardOf("")); err != nil {
		dispatcher.WriteErr(w, http.StatusUnprocessableEntity, err)
		return
	}
	if err := s.cfg.TriggerStore.Create(r.Context(), sub); err != nil {
		dispatcher.WriteErr(w, http.StatusInternalServerError, err)
		return
	}
	// The pins just landed; close the born-mid-delete window before the
	// answer — reconcileLandedPins sweeps a record revoked between this
	// write's validation and its landing, the row the delete's sweep never
	// listed (see credential_pin_sweep.go). The row is stored under a fresh
	// id, so a failed re-check is a receipt on the 201 (pins_unreconciled,
	// cause in the log), never a 5xx whose retry would mint a second cron
	// subscription and whose opaque body could not even carry this id.
	guardCtx, guardCancel := pinGuardCtx(r)
	defer guardCancel()
	pins := s.reconcileLandedPins(guardCtx, tenant, sub.KeyOverrides, sub.SecretOverrides)
	if len(pins.Cleared) > 0 && s.logger != nil {
		s.logger.Warn("triggers: subscription %s created with a pin naming a record deleted in flight; swept: %v", sub.ID, pins.Cleared)
	}
	if pins.Err != nil {
		s.logUnreconciledPins("trigger subscription", sub.ID, pins)
	}
	// The answer must not resell the swept pins — the honesty the delete's
	// 204 owes: answer with the row as the store now holds it.
	pins.strip(sub.KeyOverrides)
	pins.strip(sub.SecretOverrides)
	dispatcher.WriteJSON(w, http.StatusCreated, triggerCreated{Subscription: sub, PinsUnreconciled: pins.Unreconciled})
}

// triggerCreated is the create answer: the stored row, flattened, and —
// only when the route could not re-check a record its landed pins name
// (see reconcileLandedPins) — the ids of those records, the receipt a
// client acts on by re-reading them and clearing a pin naming a dead one.
type triggerCreated struct {
	trigger.Subscription
	PinsUnreconciled []string `json:"pins_unreconciled,omitempty"`
}

func (s *Server) handleUpdateTrigger(w http.ResponseWriter, r *http.Request) {
	tenant := s.triggerTenant(r)
	cur, err := s.cfg.TriggerStore.Get(r.Context(), r.PathValue("id"))
	if err == nil && cur.TenantID != tenant {
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
	next := applyTriggerReq(cur, req)
	// Who may pin before what may be pinned, judged on the row the edit
	// would store against the row it read: changing a pin, or what a pinned
	// subscription launches, is a team admin's credential decision; pausing
	// or resuming one is not.
	if !s.authorizeTriggerCredentials(w, r, tenant, &cur, next) {
		return
	}
	// A pin the request names is judged against the launch actor before it
	// lands; one it omits keeps the stored value (the fold above), so an
	// edit that says nothing about the pins can neither wipe one nor needs
	// to re-pass one.
	if req.KeyOverrides != nil {
		if err := s.validateKeyOverrides(teamTenantCtx(r.Context(), tenant), tenant, triggerSpineActor, req.KeyOverrides); err != nil {
			dispatcher.WriteErr(w, http.StatusBadRequest, err)
			return
		}
	}
	if req.SecretOverrides != nil {
		if err := s.validateSecretOverrides(teamTenantCtx(r.Context(), tenant), tenant, req.SecretOverrides); err != nil {
			dispatcher.WriteErr(w, http.StatusBadRequest, err)
			return
		}
	}
	// A guard the request CHANGES, not one it merely carries: the studio
	// builds its payload from the stored subscription, guard included, so
	// every toggle of a trigger that already holds one would be refused.
	if g := req.guardOf(cur.Guard); strings.TrimSpace(g) != strings.TrimSpace(cur.Guard) {
		if err := s.cloudGuardRefusal(g); err != nil {
			dispatcher.WriteErr(w, http.StatusUnprocessableEntity, err)
			return
		}
	}
	next.UpdatedAt = time.Now().UTC()
	// Update commits against the version this request read. A write that
	// landed in between — a pin sweep's clear most importantly: a swept pin
	// written back from a pre-sweep read would stand forever, with no sweep
	// ever coming behind the delete's 204 — conflicts, and the edit answers
	// 409 so the client re-reads the swept row instead of reviving it.
	if err := s.cfg.TriggerStore.Update(r.Context(), next); err != nil {
		if errors.Is(err, trigger.ErrSubscriptionConflict) {
			dispatcher.WriteErr(w, http.StatusConflict, err)
			return
		}
		dispatcher.WriteErr(w, http.StatusInternalServerError, err)
		return
	}
	// Same closure as the create route: the landed pins are re-checked
	// against their records — a pin that validated while its record lived
	// but landed after the record's delete sweep listed would otherwise
	// stand unreconciled (see credential_pin_sweep.go). The re-check reads
	// every pin the stored row holds, the ones this request omitted (kept
	// by the fold, never validated above) included. Unlike the create, a
	// failed re-check answers 5xx here: the edit is stored and nothing
	// one-time is lost, and the mend is the retry's own pass of this
	// re-check, not a validation — the studio's editor and toggle send no
	// pin map, so a retry from it validates nothing (reconcileLandedPins).
	guardCtx, guardCancel := pinGuardCtx(r)
	defer guardCancel()
	pins := s.reconcileLandedPins(guardCtx, tenant, next.KeyOverrides, next.SecretOverrides)
	if len(pins.Cleared) > 0 && s.logger != nil {
		s.logger.Warn("triggers: subscription %s updated with a pin naming a record deleted in flight; swept: %v", next.ID, pins.Cleared)
	}
	if pins.Err != nil {
		dispatcher.WriteErr(w, http.StatusInternalServerError, fmt.Errorf("subscription %s updated, but re-checking the records its pins name failed: %w — a pin may name a record deleted in flight and stand until it is cleared", next.ID, pins.Err))
		return
	}
	// The answer must not resell the swept pins — the honesty the delete's
	// 204 owes: answer with the row as the store now holds it.
	pins.strip(next.KeyOverrides)
	pins.strip(next.SecretOverrides)
	dispatcher.WriteJSON(w, http.StatusOK, next)
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
	// nil-means-untouched, like the guard pointers: the studio builds its
	// trigger payload from the stored subscription, so a plain overwrite
	// would drop a stored pin on every edit that says nothing about it.
	// A present map replaces wholesale (an empty one is the clear).
	if req.KeyOverrides != nil {
		base.KeyOverrides = req.KeyOverrides
	}
	if req.SecretOverrides != nil {
		base.SecretOverrides = req.SecretOverrides
	}
	base.Guard = req.guardOf(base.Guard)
	if req.GuardTimeout != nil {
		base.GuardTimeout = *req.GuardTimeout
	}
	if req.GuardVar != nil {
		base.GuardVar = *req.GuardVar
	}
	if req.Enabled != nil {
		base.Enabled = *req.Enabled
	}
	return base
}

// deref is the value of an optional request field, empty when absent.
func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

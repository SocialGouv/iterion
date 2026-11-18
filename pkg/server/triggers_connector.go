package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/connector/binding"
	"github.com/SocialGouv/iterion/pkg/connector/ingress"
	"github.com/SocialGouv/iterion/pkg/connector/poller"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
	"github.com/SocialGouv/iterion/pkg/dispatcher"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// isConnectorSubscription reports whether a subscription binds a connector:
// its kind says so, or it names a connection or a trigger — fields no other
// kind reads, refused on one by the validation that follows.
func isConnectorSubscription(sub trigger.Subscription) bool {
	return sub.Invocation == bundle.InvocationKindConnector || sub.ConnectionID != "" || sub.TriggerID != ""
}

// connectorBindingView is what a connector subscription's save validation
// judges; an edit that changes none of it (a pause, a pin) is not judged
// again, so a subscription whose connection is gone stays pausable.
type connectorBindingView struct {
	BotID, Invocation, Mode, ConnectionID, TriggerID, When, NoLiveRun, ArgsVar, Cron string
	Backfill                                                                         int
	Filters                                                                          map[string][]string
	VarMap, Vars, PollQuery                                                          map[string]string
}

func bindingViewOf(s trigger.Subscription) connectorBindingView {
	return connectorBindingView{
		BotID: s.BotID, Invocation: string(s.Invocation), Mode: string(s.Mode), ConnectionID: s.ConnectionID,
		TriggerID: s.TriggerID, When: s.When, NoLiveRun: s.NoLiveRun, ArgsVar: s.ArgsVar, Cron: s.Cron,
		Backfill: s.Backfill, Filters: s.Filters, VarMap: s.VarMap, Vars: s.Vars, PollQuery: s.PollQuery,
	}
}

// activatePolledSubscription records the activation of a subscription to a
// polled trigger (ADR-168 §7) — saved enabled, enabled again, or moved to
// another poller by a new query: it ensures the poller exists, and moves the
// subscription's PollerSince to the poller's sequence, so it receives the
// intakes accepted from now on and none before; a backfill is owed again. An
// edit that activates nothing keeps both. The poller is written before the
// subscription, so a subscription never names a poller nothing ticks.
func (s *Server) activatePolledSubscription(ctx context.Context, tenant string, cur *trigger.Subscription, next *trigger.Subscription) (int, error) {
	if next.Poller == "" {
		return 0, nil
	}
	if cur != nil && cur.Enabled && cur.Poller == next.Poller {
		next.PollerSince, next.BackfillDone = cur.PollerSince, cur.BackfillDone
		return 0, nil
	}
	if !next.Enabled {
		return 0, nil
	}
	if why := s.connectorPollersOff(); why != "" {
		return http.StatusServiceUnavailable, fmt.Errorf("trigger %s is polled, and %s: a subscription to it would never fire", next.TriggerID, why)
	}
	now := time.Now()
	p, _, err := s.connectorPollers.Ensure(ctx, poller.Poller{
		TenantID: binding.ConnectionTenant(tenant), ConnectionID: next.ConnectionID, TriggerID: next.TriggerID,
		Query: next.PollQuery, CreatedAt: now,
	})
	if err != nil {
		return http.StatusServiceUnavailable, fmt.Errorf("ensure the poller of trigger %s: %w", next.TriggerID, err)
	}
	if p.ID != next.Poller {
		return http.StatusInternalServerError, fmt.Errorf("the poller of trigger %s is %s, and the subscription derived %s", next.TriggerID, p.ID, next.Poller)
	}
	// A dormant poller — nobody listened — is read at once, and its next
	// scan primes what changed while it slept.
	if err := s.connectorPollers.Wake(ctx, p.TenantID, p.ID, now); err != nil {
		return http.StatusServiceUnavailable, fmt.Errorf("wake the poller of trigger %s: %w", next.TriggerID, err)
	}
	next.PollerSince, next.BackfillDone = p.Sequence, false
	return 0, nil
}

// checkRequestedPoller refuses a request that names a poller other than the
// one its query derives; naming the same one — a GET's row sent back as the
// edit — is accepted.
func checkRequestedPoller(requested string, sub trigger.Subscription) error {
	if requested == "" || requested == sub.Poller {
		return nil
	}
	return fmt.Errorf("poller %q: a subscription names its query in poll_query, and the poller is derived from it (%q)", requested, sub.Poller)
}

// validateConnectorSubscription resolves what a connector subscription
// names — its connection in the tenant's scope, the trigger its connector's
// package declares, the bot's declared vars — and holds it to them
// (binding.ValidateSubscription). The status says whose a refusal is: 400
// the request's, 503 an outage of what it was checked against.
func (s *Server) validateConnectorSubscription(ctx context.Context, tenant string, sub trigger.Subscription) (trigger.Subscription, int, error) {
	b, status, err := s.resolveConnectorBinding(ctx, tenant, sub)
	return b.sub, status, err
}

// connectorBinding is a connector subscription judged at save, with what
// it was judged against: the trigger, its package's ingress, the bot's
// declared var names.
type connectorBinding struct {
	sub     trigger.Subscription
	trig    spec.Trigger
	pkg     *spec.Package
	ingress *spec.Ingress
	botVars []string
}

// resolveConnectorBinding is validateConnectorSubscription's work, its
// resolved parts kept for a caller that reads an event through them (the
// preview).
func (s *Server) resolveConnectorBinding(ctx context.Context, tenant string, sub trigger.Subscription) (connectorBinding, int, error) {
	fail := func(status int, err error) (connectorBinding, int, error) {
		return connectorBinding{sub: sub}, status, err
	}
	if sub.Invocation != bundle.InvocationKindConnector {
		return fail(http.StatusBadRequest, fmt.Errorf("connection_id and trigger_id belong to a connector subscription (invocation %q)", bundle.InvocationKindConnector))
	}
	if sub.ConnectionID == "" || sub.TriggerID == "" {
		return fail(http.StatusBadRequest, errors.New("a connector subscription is bound to a connection — pass its connection_id, and its trigger_id"))
	}
	if s.connectionStore == nil {
		return fail(http.StatusServiceUnavailable, errors.New("this server has no connection plane to bind a connector subscription to"))
	}
	connTenant := binding.ConnectionTenant(tenant)
	conn, err := s.connectionStore.Get(ctx, connTenant, sub.ConnectionID)
	switch {
	case errors.Is(err, connection.ErrNotFound):
		return fail(http.StatusBadRequest, fmt.Errorf("connection %q not found", sub.ConnectionID))
	case err != nil:
		return fail(http.StatusServiceUnavailable, fmt.Errorf("read connection %s: %w", sub.ConnectionID, err))
	}
	connector, _, _ := strings.Cut(sub.TriggerID, ".")
	if connector != conn.Connector {
		return fail(http.StatusBadRequest, fmt.Errorf("trigger %s belongs to connector %q, and connection %q is a %q connection", sub.TriggerID, connector, conn.Alias, conn.Connector))
	}
	if !conn.Has(connection.CapTrigger) {
		return fail(http.StatusBadRequest, &connection.CapabilityError{Alias: conn.Alias, Want: connection.CapTrigger, Have: conn.Capabilities})
	}
	pkg, _, err := s.connectorCatalogFor(connTenant).Package(conn.Connector)
	switch {
	case connection.IsCatalogUnavailable(err):
		return fail(http.StatusServiceUnavailable, err)
	case err != nil:
		return fail(http.StatusBadRequest, fmt.Errorf("connector %q does not resolve in the tenant's catalog: %w", conn.Connector, err))
	}
	var trig *spec.Trigger
	for i := range pkg.Triggers {
		if pkg.Triggers[i].ID == sub.TriggerID {
			trig = &pkg.Triggers[i]
		}
	}
	if trig == nil {
		return fail(http.StatusBadRequest, fmt.Errorf("the %s package declares no trigger %s", conn.Connector, sub.TriggerID))
	}
	if trig.Polled() && !conn.Has(connection.CapAction) {
		// Polling spends the connection's credential, which the trigger
		// grant does not authorize (ADR-168 §7): a subscription saved
		// without it would fail on every tick.
		return fail(http.StatusBadRequest, fmt.Errorf("trigger %s is polled, and polling spends connection %q's credential, which needs the action grant: %w",
			trig.ID, conn.Alias, &connection.CapabilityError{Alias: conn.Alias, Want: connection.CapAction, Have: conn.Capabilities}))
	}
	if !trig.Polled() && pkg.Ingress == nil {
		// Prevents: a subscription nothing can ever feed — a webhook trigger
		// reaches iterion through the package's ingress, and this one has
		// none (the catalogue does not offer it either).
		return fail(http.StatusBadRequest, fmt.Errorf("the %s package declares no ingress, so no delivery can reach trigger %s", conn.Connector, sub.TriggerID))
	}
	entry, found, err := s.effectiveFindByNameForTeam(ctx, tenant, sub.BotID)
	switch {
	case err != nil:
		return fail(http.StatusServiceUnavailable, fmt.Errorf("read bot %s: %w", sub.BotID, err))
	case !found:
		return fail(http.StatusBadRequest, fmt.Errorf("bot %q not found", sub.BotID))
	}
	var (
		vars  []binding.BotVar
		names []string
	)
	if entry.Vars != nil {
		for _, f := range entry.Vars.Fields {
			vars = append(vars, binding.BotVar{Name: f.Name, Required: f.Default == nil})
			names = append(names, f.Name)
		}
	}
	out, err := binding.ValidateSubscription(sub, *trig, vars)
	if err != nil {
		return fail(http.StatusBadRequest, err)
	}
	return connectorBinding{sub: out, trig: *trig, pkg: pkg, ingress: pkg.Ingress, botVars: names}, 0, nil
}

// judgeConnectorEdit runs the save validation on an edit that changes a
// connector subscription's binding; an edit that changes none of it passes.
func (s *Server) judgeConnectorEdit(ctx context.Context, tenant string, cur *trigger.Subscription, next trigger.Subscription) (trigger.Subscription, int, error) {
	if !isConnectorSubscription(next) && (cur == nil || !isConnectorSubscription(*cur)) {
		return next, 0, nil
	}
	if cur != nil && reflect.DeepEqual(bindingViewOf(*cur), bindingViewOf(next)) {
		return next, 0, nil
	}
	return s.validateConnectorSubscription(ctx, tenant, next)
}

// copyFilterValues copies a filter map, value lists included.
func copyFilterValues(in map[string][]string) map[string][]string {
	if in == nil {
		return nil
	}
	out := make(map[string][]string, len(in))
	for k, v := range in {
		out[k] = append([]string(nil), v...)
	}
	return out
}

// triggerPreviewReq is a connector subscription as it would be saved, and
// the delivery to read through it: a pasted payload and headers, or — with
// none — the trigger's recorded sample.
type triggerPreviewReq struct {
	BotID        string              `json:"bot_id"`
	ConnectionID string              `json:"connection_id"`
	TriggerID    string              `json:"trigger_id"`
	Filters      map[string][]string `json:"filters,omitempty"`
	When         string              `json:"when,omitempty"`
	VarMap       map[string]string   `json:"var_map,omitempty"`
	Vars         map[string]string   `json:"vars,omitempty"`
	// PollQuery is a polled trigger's query values, as the subscription
	// would carry them; Payload is then one ITEM of its operation's answer.
	PollQuery map[string]string `json:"poll_query,omitempty"`
	Payload   json.RawMessage   `json:"payload,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
}

// triggerPreview is what a delivery would do: dropped by the trigger's
// loop guard (filtered), or judged — matched, the vars a run would
// receive; not matched, the reason; refused, the refusal — and the event it
// was judged on.
type triggerPreview struct {
	Sample   bool              `json:"sample,omitempty"`
	Filtered string            `json:"filtered,omitempty"`
	Matched  bool              `json:"matched"`
	Reason   string            `json:"reason,omitempty"`
	Refusal  string            `json:"refusal,omitempty"`
	Vars     map[string]string `json:"vars,omitempty"`
	Event    json.RawMessage   `json:"event,omitempty"`
}

// handleTriggerPreview reads a delivery through a connector subscription as
// the lane, the matcher and the launch would (ADR-168 §6): the same
// extraction (ingress.Preview), the same judgment and vars (binding). It
// writes and launches nothing.
func (s *Server) handleTriggerPreview(w http.ResponseWriter, r *http.Request) {
	var req triggerPreviewReq
	if !decodeJSON(w, r, &req) {
		return
	}
	tenant := s.triggerTenant(r)
	b, status, err := s.resolveConnectorBinding(r.Context(), tenant, trigger.Subscription{
		TenantID: tenant, BotID: req.BotID, Invocation: bundle.InvocationKindConnector,
		ConnectionID: req.ConnectionID, TriggerID: req.TriggerID, PollQuery: req.PollQuery,
		Filters: req.Filters, When: req.When, VarMap: req.VarMap, Vars: req.Vars,
	})
	if err != nil {
		dispatcher.WriteErr(w, status, err)
		return
	}
	if b.trig.Polled() {
		s.previewPolledItem(w, r, b, req)
		return
	}
	var (
		out     triggerPreview
		payload map[string]any
		header  = http.Header{}
	)
	switch {
	case len(req.Payload) > 0:
		dec := json.NewDecoder(bytes.NewReader(req.Payload))
		dec.UseNumber()
		if err := dec.Decode(&payload); err != nil {
			dispatcher.WriteErr(w, http.StatusBadRequest, fmt.Errorf("payload: %w", err))
			return
		}
		for k, v := range req.Headers {
			header.Set(k, v)
		}
	case len(b.trig.SampleBody) > 0:
		var sample spec.SampleDelivery
		dec := json.NewDecoder(bytes.NewReader(b.trig.SampleBody))
		dec.UseNumber()
		if err := dec.Decode(&sample); err != nil {
			dispatcher.WriteErr(w, http.StatusInternalServerError, fmt.Errorf("the trigger's sample: %w", err))
			return
		}
		payload, out.Sample = sample.Payload, true
		for k, v := range sample.Headers {
			header.Set(k, v)
		}
	default:
		dispatcher.WriteErr(w, http.StatusBadRequest, fmt.Errorf("no payload, and trigger %s ships no sample — paste a delivery", b.trig.ID))
		return
	}
	raw, filtered, err := ingress.Preview(b.ingress, b.trig, payload, header)
	if err != nil {
		dispatcher.WriteErr(w, http.StatusBadRequest, err)
		return
	}
	if filtered != "" {
		out.Filtered = filtered
		dispatcher.WriteJSON(w, http.StatusOK, out)
		return
	}
	s.judgePreview(w, r, b, raw, out)
}

// judgePreview judges an event read through a subscription — matched, the
// vars a run would receive; not matched, the reason; refused, the refusal —
// as the matcher and the launch would, and answers it.
func (s *Server) judgePreview(w http.ResponseWriter, r *http.Request, b connectorBinding, raw json.RawMessage, out triggerPreview) {
	out.Event = raw
	ev, err := binding.DecodeEvent(raw)
	if err != nil {
		dispatcher.WriteErr(w, http.StatusInternalServerError, err)
		return
	}
	v, err := binding.Judge(r.Context(), b.sub, b.trig, ev)
	if err != nil {
		dispatcher.WriteErr(w, http.StatusServiceUnavailable, err)
		return
	}
	out.Matched, out.Reason, out.Refusal = v.Matched, v.Reason, v.Refusal
	if v.Matched {
		vars, refusal, err := binding.Vars(r.Context(), b.sub, ev, b.botVars)
		if err != nil {
			dispatcher.WriteErr(w, http.StatusServiceUnavailable, err)
			return
		}
		out.Vars, out.Refusal = vars, refusal
		out.Matched = refusal == ""
	}
	dispatcher.WriteJSON(w, http.StatusOK, out)
}

// previewPolledItem reads one item through a polled trigger as its poller
// would (ingress.ReadItem): a pasted item, or — with none — the first item
// of the trigger's recorded answer, located the way a scan locates them
// (spec.PollItems). Then the same judgment and vars as a delivery's.
func (s *Server) previewPolledItem(w http.ResponseWriter, r *http.Request, b connectorBinding, req triggerPreviewReq) {
	var (
		out  triggerPreview
		item map[string]any
	)
	switch {
	case len(req.Payload) > 0:
		dec := json.NewDecoder(bytes.NewReader(req.Payload))
		dec.UseNumber()
		if err := dec.Decode(&item); err != nil {
			dispatcher.WriteErr(w, http.StatusBadRequest, fmt.Errorf("payload: an item of trigger %s's answer, as a JSON object: %w", b.trig.ID, err))
			return
		}
	case len(b.trig.SampleBody) > 0:
		var sample spec.SampleResponse
		dec := json.NewDecoder(bytes.NewReader(b.trig.SampleBody))
		dec.UseNumber()
		if err := dec.Decode(&sample); err != nil {
			dispatcher.WriteErr(w, http.StatusInternalServerError, fmt.Errorf("the trigger's sample: %w", err))
			return
		}
		op, ok := b.pkg.Operation(b.trig.Poll.Operation)
		if !ok {
			dispatcher.WriteErr(w, http.StatusInternalServerError, fmt.Errorf("trigger %s polls operation %s, which its package no longer declares", b.trig.ID, b.trig.Poll.Operation))
			return
		}
		items, err := spec.PollItems(op, b.trig.Poll, sample.Response)
		if err != nil || len(items) == 0 {
			dispatcher.WriteErr(w, http.StatusInternalServerError, fmt.Errorf("the trigger's sample holds no item: %v", err))
			return
		}
		obj, ok := items[0].(map[string]any)
		if !ok {
			dispatcher.WriteErr(w, http.StatusInternalServerError, fmt.Errorf("the trigger's sample's first item is %T, not an object", items[0]))
			return
		}
		item, out.Sample = obj, true
	default:
		dispatcher.WriteErr(w, http.StatusBadRequest, fmt.Errorf("no payload, and trigger %s ships no sample — paste an item of its answer", b.trig.ID))
		return
	}
	raw, filtered, err := ingress.ReadItem(b.trig, item)
	if err != nil {
		dispatcher.WriteErr(w, http.StatusBadRequest, err)
		return
	}
	if filtered != "" {
		out.Filtered = filtered
		dispatcher.WriteJSON(w, http.StatusOK, out)
		return
	}
	s.judgePreview(w, r, b, raw, out)
}

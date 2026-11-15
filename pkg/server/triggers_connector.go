package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/connector/binding"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
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
	BotID, Invocation, Mode, ConnectionID, TriggerID, Poller, When, NoLiveRun, ArgsVar, Cron string
	Filters                                                                                  map[string][]string
	VarMap, Vars                                                                             map[string]string
}

func bindingViewOf(s trigger.Subscription) connectorBindingView {
	return connectorBindingView{
		BotID: s.BotID, Invocation: string(s.Invocation), Mode: string(s.Mode), ConnectionID: s.ConnectionID,
		TriggerID: s.TriggerID, Poller: s.Poller, When: s.When, NoLiveRun: s.NoLiveRun, ArgsVar: s.ArgsVar, Cron: s.Cron,
		Filters: s.Filters, VarMap: s.VarMap, Vars: s.Vars,
	}
}

// validateConnectorSubscription resolves what a connector subscription
// names — its connection in the tenant's scope, the trigger its connector's
// package declares, the bot's declared vars — and holds it to them
// (binding.ValidateSubscription). The status says whose a refusal is: 400
// the request's, 503 an outage of what it was checked against.
func (s *Server) validateConnectorSubscription(ctx context.Context, tenant string, sub trigger.Subscription) (trigger.Subscription, int, error) {
	if sub.Invocation != bundle.InvocationKindConnector {
		return sub, http.StatusBadRequest, fmt.Errorf("connection_id and trigger_id belong to a connector subscription (invocation %q)", bundle.InvocationKindConnector)
	}
	if sub.ConnectionID == "" || sub.TriggerID == "" {
		return sub, http.StatusBadRequest, errors.New("a connector subscription is bound to a connection — pass its connection_id, and its trigger_id")
	}
	if s.connectionStore == nil {
		return sub, http.StatusServiceUnavailable, errors.New("this server has no connection plane to bind a connector subscription to")
	}
	connTenant := binding.ConnectionTenant(tenant)
	conn, err := s.connectionStore.Get(ctx, connTenant, sub.ConnectionID)
	switch {
	case errors.Is(err, connection.ErrNotFound):
		return sub, http.StatusBadRequest, fmt.Errorf("connection %q not found", sub.ConnectionID)
	case err != nil:
		return sub, http.StatusServiceUnavailable, fmt.Errorf("read connection %s: %w", sub.ConnectionID, err)
	}
	connector, _, _ := strings.Cut(sub.TriggerID, ".")
	if connector != conn.Connector {
		return sub, http.StatusBadRequest, fmt.Errorf("trigger %s belongs to connector %q, and connection %q is a %q connection", sub.TriggerID, connector, conn.Alias, conn.Connector)
	}
	if !conn.Has(connection.CapTrigger) {
		return sub, http.StatusBadRequest, &connection.CapabilityError{Alias: conn.Alias, Want: connection.CapTrigger, Have: conn.Capabilities}
	}
	pkg, _, err := s.connectorCatalogFor(connTenant).Package(conn.Connector)
	switch {
	case connection.IsCatalogUnavailable(err):
		return sub, http.StatusServiceUnavailable, err
	case err != nil:
		return sub, http.StatusBadRequest, fmt.Errorf("connector %q does not resolve in the tenant's catalog: %w", conn.Connector, err)
	}
	var trig *spec.Trigger
	for i := range pkg.Triggers {
		if pkg.Triggers[i].ID == sub.TriggerID {
			trig = &pkg.Triggers[i]
		}
	}
	if trig == nil {
		return sub, http.StatusBadRequest, fmt.Errorf("the %s package declares no trigger %s", conn.Connector, sub.TriggerID)
	}
	entry, found, err := s.effectiveFindByNameForTeam(ctx, tenant, sub.BotID)
	switch {
	case err != nil:
		return sub, http.StatusServiceUnavailable, fmt.Errorf("read bot %s: %w", sub.BotID, err)
	case !found:
		return sub, http.StatusBadRequest, fmt.Errorf("bot %q not found", sub.BotID)
	}
	var vars []binding.BotVar
	if entry.Vars != nil {
		for _, f := range entry.Vars.Fields {
			vars = append(vars, binding.BotVar{Name: f.Name, Required: f.Default == nil})
		}
	}
	out, err := binding.ValidateSubscription(sub, *trig, vars)
	if err != nil {
		return sub, http.StatusBadRequest, err
	}
	return out, 0, nil
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

package binding

import (
	"context"
	"fmt"
	"strings"

	"github.com/SocialGouv/iterion/pkg/connection"
	"github.com/SocialGouv/iterion/pkg/connector/intake"
	"github.com/SocialGouv/iterion/pkg/connector/spec"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// SubscriptionTenant is the tenant an intake's subscriptions are stored
// under. The local single-host scope keeps its connections and intakes
// under connection.LocalTenant and its subscriptions under "" (the trigger
// store's local scope); a cloud tenant is the same on both sides.
func SubscriptionTenant(intakeTenant string) string {
	if intakeTenant == connection.LocalTenant {
		return ""
	}
	return intakeTenant
}

// Matcher judges an intake against its candidate subscriptions
// (intake.Matcher): the connection's subscriptions to the intake's trigger
// and source, in the subscription-id order every twin of
// trigger.SubscriptionStore.ListCandidates answers, each judged by Judge.
//
// What no retry changes is a refused pair per candidate — the package gone
// from the tenant's catalog or unloadable, the trigger no longer declared,
// an event that no longer decodes — never a dropped intake. What a retry
// may change is an error: the catalog or the store unavailable, the
// context's end.
type Matcher struct {
	Subscriptions trigger.SubscriptionStore
	// Catalog is the tenant's connector catalog (the server's
	// connectorCatalogFor), asked under the intake's own tenant.
	Catalog func(tenantID string) connection.Catalog
}

var _ intake.Matcher = (*Matcher)(nil)

func (m *Matcher) Match(ctx context.Context, in intake.Intake) ([]intake.Verdict, error) {
	cands, err := m.Subscriptions.ListCandidates(ctx, trigger.Event{
		Source:       trigger.SourceConnector,
		TenantID:     SubscriptionTenant(in.TenantID),
		Kind:         in.TriggerID,
		ConnectionID: in.ConnectionID,
		IngressID:    in.IngressID,
		PollerID:     in.PollerID,
	})
	if err != nil {
		return nil, fmt.Errorf("binding: list the candidates of intake %s: %w", in.ID, err)
	}
	if len(cands) == 0 {
		return nil, nil
	}
	refuseAll := func(reason string) []intake.Verdict {
		out := make([]intake.Verdict, len(cands))
		for i, sub := range cands {
			out[i] = intake.Verdict{SubscriptionID: sub.ID, Refusal: reason}
		}
		return out
	}

	trig, refusal, err := m.Trigger(in)
	if err != nil {
		return nil, err
	}
	if refusal != "" {
		return refuseAll(refusal), nil
	}
	ev, err := DecodeEvent(in.Event)
	if err != nil {
		return refuseAll(err.Error()), nil
	}
	out := make([]intake.Verdict, 0, len(cands))
	for _, sub := range cands {
		v, err := Judge(ctx, sub, trig, ev)
		if err != nil {
			return nil, err
		}
		out = append(out, intake.Verdict{SubscriptionID: sub.ID, Matched: v.Matched, Reason: v.Reason, Refusal: v.Refusal})
	}
	return out, nil
}

// Rejudge judges one subscription, as it stands now, against an intake:
// the launch-time check a matched pair passes again (ADR-168 Q2), and the
// replay of a pair matching refused. The event comes back decoded for the
// vars the launch computes from it.
func (m *Matcher) Rejudge(ctx context.Context, in intake.Intake, sub trigger.Subscription) (Verdict, Event, error) {
	trig, refusal, err := m.Trigger(in)
	if err != nil {
		return Verdict{}, Event{}, err
	}
	if refusal != "" {
		return Verdict{Refusal: refusal}, Event{}, nil
	}
	ev, err := DecodeEvent(in.Event)
	if err != nil {
		return Verdict{Refusal: err.Error()}, Event{}, nil
	}
	v, err := Judge(ctx, sub, trig, ev)
	return v, ev, err
}

// Trigger resolves the intake's trigger in its tenant's catalog: a refusal
// for what no retry changes, an error for an outage.
func (m *Matcher) Trigger(in intake.Intake) (spec.Trigger, string, error) {
	connector, _, _ := strings.Cut(in.TriggerID, ".")
	pkg, _, err := m.Catalog(in.TenantID).Package(connector)
	switch {
	case connection.IsCatalogUnavailable(err):
		return spec.Trigger{}, "", fmt.Errorf("binding: resolve connector %s: %w", connector, err)
	case connection.IsNoSuchConnector(err):
		return spec.Trigger{}, fmt.Sprintf("the tenant's catalog no longer holds connector %q", connector), nil
	case err != nil:
		return spec.Trigger{}, fmt.Sprintf("connector %q does not load: %v", connector, err), nil
	}
	for _, t := range pkg.Triggers {
		if t.ID == in.TriggerID {
			return t, "", nil
		}
	}
	return spec.Trigger{}, fmt.Sprintf("the %s package no longer declares trigger %s", connector, in.TriggerID), nil
}

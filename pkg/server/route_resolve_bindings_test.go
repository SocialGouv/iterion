package server

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/llmroute"
	"github.com/SocialGouv/iterion/pkg/platformcfg"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// A binding layer carrying a block the fold cannot read REFUSES the launch,
// naming the level: records can hold hand-edited or rolling-deploy values
// no write path ever validated (the subscription and webhook records have
// no validated write surface), and silently folding garbage would route
// the run somewhere nobody wrote.
func TestResolveRunLLMRoutePolicy_RefusesAnUnreadableBindingLayer(t *testing.T) {
	s := routeResolveServer(t, nil)
	got, err := s.resolveRunLLMRoutePolicy(context.Background(), "", "", llmroute.Layer{
		Source: llmroute.SourceTrigger,
		Policy: llmroute.Policy{Triggers: []string{"budget"}},
	})
	if err == nil {
		t.Fatalf("resolve = %+v, want a refusal", got)
	}
	if !strings.Contains(err.Error(), llmroute.SourceTrigger) || !strings.Contains(err.Error(), "vocabulary") {
		t.Fatalf("err = %v, want it to name the level and the field", err)
	}
	if got != nil {
		t.Fatalf("snapshot = %+v alongside an error", got)
	}
}

// The scheduled path folds its binding layer with provenance naming the
// surface (schedule), not a generic "binding" — the same convention the
// retry chain's provenance follows.
func TestResolveRunLLMRoutePolicy_BindingLayerNamesItsSurface(t *testing.T) {
	park := llmroute.RefusedPinnedPark
	s := routeResolveServer(t, &platformcfg.PlatformCredentials{Routing: &llmroute.Policy{RefusedPinnedKey: park}})
	got, err := s.resolveRunLLMRoutePolicy(context.Background(), "", "", llmroute.Layer{
		Source: llmroute.SourceSchedule,
		Policy: llmroute.Policy{PairOrder: []string{llmroute.Pair(llmroute.HarnessClaw, "anthropic_key")}},
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Sources["pair_order"] != llmroute.SourceSchedule {
		t.Fatalf("pair_order provenance = %q, want schedule", got.Sources["pair_order"])
	}
	if got.Sources["refused_pinned_key"] != llmroute.SourcePlatform {
		t.Fatalf("refused_pinned_key provenance = %q, want platform (the binding set nothing)", got.Sources["refused_pinned_key"])
	}
}

// The binding rides the plan: LaunchPlan.Routing is what the spine's
// launcher folds. A subscription's block survives the plan round-trip.
func TestLaunchPlan_CarriesTheSubscriptionRouting(t *testing.T) {
	plan := trigger.LaunchPlan{
		TenantID: "t1", BotID: "b1",
		Routing: llmroute.Policy{RefusedPinnedKey: llmroute.RefusedPinnedPark},
	}
	if plan.Routing.RefusedPinnedKey != llmroute.RefusedPinnedPark {
		t.Fatalf("plan routing lost: %+v", plan.Routing)
	}
}

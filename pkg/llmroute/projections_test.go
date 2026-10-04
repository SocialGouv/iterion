package llmroute_test

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/cloudsched"
	"github.com/SocialGouv/iterion/pkg/llmroute"
	"github.com/SocialGouv/iterion/pkg/trigger"
	"github.com/SocialGouv/iterion/pkg/webhooks"
)

// Every binding surface projects its block the same way: nil = the layer
// sets nothing (the zero Policy, never normalized — defaults filled here
// would masquerade as a binding-level choice), set = the block verbatim.
func TestBindingSurfaces_ProjectRouting(t *testing.T) {
	block := &llmroute.Policy{
		PairOrder:        []string{llmroute.Pair(llmroute.HarnessClaw, "anthropic_key")},
		RefusedPinnedKey: llmroute.RefusedPinnedPark,
	}
	for name, zero := range map[string]llmroute.Policy{
		"schedule":     (cloudsched.ScheduledBot{}).RoutingPolicy(),
		"subscription": (trigger.Subscription{}).RoutingPolicy(),
		"webhook":      (webhooks.Config{}).RoutingPolicy(),
	} {
		if zero.PairOrder != nil || zero.RefusedPinnedKey != "" {
			t.Errorf("%s: nil block projected %+v, want the zero policy", name, zero)
		}
	}
	if got := (cloudsched.ScheduledBot{Routing: block}).RoutingPolicy(); got.RefusedPinnedKey != llmroute.RefusedPinnedPark {
		t.Errorf("schedule projection lost the block: %+v", got)
	}
	if got := (trigger.Subscription{Routing: block}).RoutingPolicy(); len(got.PairOrder) != 1 {
		t.Errorf("subscription projection lost the block: %+v", got)
	}
	if got := (webhooks.Config{Routing: block}).RoutingPolicy(); got.RefusedPinnedKey != llmroute.RefusedPinnedPark {
		t.Errorf("webhook projection lost the block: %+v", got)
	}
}

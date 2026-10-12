package trigger

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/llmroute"
)

// The subscription's routing block must actually reach the plan through
// BOTH construction sites — the evaluator's event path and the
// scheduler's tick. Deleting either `Routing: sub.RoutingPolicy()` line
// reddens its half: a binding voice silently gone is a run routed
// somewhere the operator did not write.
func TestPlanCarriesTheSubscriptionRouting(t *testing.T) {
	park := llmroute.RefusedPinnedPark
	sub := Subscription{
		ID: "sub-1", TenantID: "t1", BotID: "reviewer", Enabled: true,
		Mode:    bundle.ExecutionDirect,
		Routing: &llmroute.Policy{PairOrder: []string{llmroute.Pair(llmroute.HarnessClaw, "anthropic_key")}, RefusedPinnedKey: park},
	}

	eval := NewEvaluator(NewMemorySubscriptionStore())
	plan := eval.buildPlan(sub, Event{
		Source:  SourceSchedule,
		Kind:    "cron",
		Subject: Subject{Type: "schedule", ID: sub.ID},
	})
	if len(plan.Routing.PairOrder) != 1 || plan.Routing.RefusedPinnedKey != park {
		t.Fatalf("evaluator plan routing = %+v, want the subscription's block", plan.Routing)
	}

	fl := &fakeLauncher{}
	sched := NewScheduler(NewMemorySubscriptionStore(), fl)
	sched.fireIsolated(context.Background(), sub)
	if len(fl.plans) != 1 {
		t.Fatalf("scheduler launched %d plans, want 1", len(fl.plans))
	}
	if len(fl.plans[0].Routing.PairOrder) != 1 || fl.plans[0].Routing.RefusedPinnedKey != park {
		t.Fatalf("scheduler plan routing = %+v, want the subscription's block", fl.plans[0].Routing)
	}
}

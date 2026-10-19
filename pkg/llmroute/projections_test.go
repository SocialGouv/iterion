package llmroute_test

import (
	"reflect"
	"sort"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/cloudsched"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
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

// The policy's override vocabularies must stay the vocabularies the CODE
// THAT READS THEM acts on: the effort levels the DSL validates against, and
// the hint classes the executor retry logic acts on. Both drift tests live
// in this external package because pkg/dsl/ir imports pkg/llmroute — the
// same assertion inside package llmroute is an import cycle.
func TestVocabulariesAgreeWithTheCodeThatReadsThem(t *testing.T) {
	// Efforts: the policy's closed list must be exactly the DSL's set.
	wantEfforts := make([]string, 0, len(ir.ValidReasoningEfforts))
	for e := range ir.ValidReasoningEfforts {
		wantEfforts = append(wantEfforts, e)
	}
	sort.Strings(wantEfforts)
	gotEfforts := append([]string(nil), llmroute.ReasoningEfforts...)
	sort.Strings(gotEfforts)
	if !reflect.DeepEqual(gotEfforts, wantEfforts) {
		t.Fatalf("llmroute efforts %v != ir.ValidReasoningEfforts %v", gotEfforts, wantEfforts)
	}
	for _, e := range gotEfforts {
		if !llmroute.ValidEffort(e) {
			t.Fatalf("ValidEffort(%q) = false, the list and the predicate disagree", e)
		}
	}
	if llmroute.ValidEffort("bogus") {
		t.Fatal("ValidEffort(bogus) = true — the predicate must key on the list")
	}
	// Hint classes: the local twin of model.ConsumesProviderHint must agree
	// with the function the executor acts on, over every harness name the
	// policy's own vocabulary can name (llmroute.Harnesses — one
	// enumeration, not a copy of it) plus the non-harness spellings.
	for _, h := range append(append([]string(nil), llmroute.Harnesses...), "", "unknown") {
		lc, ls := llmroute.ConsumesProviderHint(h)
		mc, ms := model.ConsumesProviderHint(h)
		if lc != mc || ls != ms {
			t.Fatalf("ConsumesProviderHint(%q): llmroute=(%v,%v) model=(%v,%v) — the twin drifted", h, lc, ls, mc, ms)
		}
		// The value-aware half: the twin must agree with the runtime's own
		// predicate over the whole (harness, value) plane the policy can
		// name, claw's "anthropic" switch included.
		for _, p := range append(append([]string(nil), llmroute.ProviderHints...), "", "Anthropic", " zai ", "auto") {
			if llmroute.HonoursProviderHintValue(h, p) != model.HonoursProviderHintValue(h, p) {
				t.Fatalf("HonoursProviderHintValue(%q, %q): llmroute=%v model=%v — the twin drifted", h, p, llmroute.HonoursProviderHintValue(h, p), model.HonoursProviderHintValue(h, p))
			}
		}
	}
}

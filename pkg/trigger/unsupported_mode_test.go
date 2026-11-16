package trigger

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
)

// A stored subscription whose mode no effect honours — written before the
// routes refused it, or by a store write that bypassed them — launches
// nothing when an event matches it: the evaluator reads the mode through
// bundle.ResolveMode, never as a default launch.
func TestTheEvaluatorLaunchesNothingOnAModeNoEffectHonours(t *testing.T) {
	for _, mode := range []bundle.ExecutionMode{bundle.ExecutionDeliver, "launch"} {
		t.Run(string(mode), func(t *testing.T) {
			st := seed(t, Subscription{ID: "s", BotID: "b", Mode: mode, Enabled: true})
			fl := &fakeLauncher{}
			ev := NewEvaluator(st, WithLauncher(fl))
			_ = ev.Handle(context.Background(), Event{Source: SourceCustom, Kind: "anything"})
			if len(fl.plans) != 0 {
				t.Fatalf("a %s-mode subscription launched: %+v", mode, fl.plans)
			}
		})
	}
}

// The effects worker retires an effect whose mode no effect honours on its
// first attempt: a retry cannot make the mode supported, so the row is done
// and nothing launched — never parked for retries.
func TestTheEffectWorkerRetiresAnUnsupportedModeAtOnce(t *testing.T) {
	sub := Subscription{
		ID: "s1", TenantID: "t1", BotID: "parked-bot", Enabled: true, Mode: bundle.ExecutionDeliver,
		Match: Matcher{Sources: []Source{SourceBoard}},
	}
	launcher := &stubEffectLauncher{}
	w, out, ev := effectWorld(t, sub, &stubConsumingBoard{}, launcher)

	w.Tick(context.Background(), 10)
	if launcher.launches != 0 {
		t.Fatalf("a deliver-mode effect launched %d run(s)", launcher.launches)
	}
	row, _ := out.Row(EffectID(ev.ID, "s1"))
	if row.State != EffectDone {
		t.Fatalf("state = %s after one tick, want done (attempts=%d)", row.State, row.Attempts)
	}
}

// unscopedCandidates answers every subscription of the tenant as a
// candidate, so the evaluator's own refusal is witnessed without the store
// query's exclusion of connector rows standing in front of it.
type unscopedCandidates struct{ *MemorySubscriptionStore }

func (u unscopedCandidates) ListCandidates(ctx context.Context, ev Event) ([]Subscription, error) {
	return u.ListByTenant(ctx, ev.TenantID)
}

// Whatever a store answers, the evaluator fires no connector subscription
// from the bus — one recognised by its connector key, one by its invocation
// kind — each refused on its own.
func TestTheEvaluatorRefusesConnectorSubscriptionsItIsHanded(t *testing.T) {
	for name, sub := range map[string]Subscription{
		"keyed":            {ID: "keyed", BotID: "keyed-bot", Enabled: true, ConnectionID: "conn-a", TriggerID: "slack.message.posted"},
		"connector-invoke": {ID: "kind", BotID: "kind-bot", Enabled: true, Invocation: bundle.InvocationKindConnector},
	} {
		t.Run(name, func(t *testing.T) {
			st := unscopedCandidates{seed(t, sub)}
			fl := &fakeLauncher{}
			ev := NewEvaluator(st, WithLauncher(fl))
			_ = ev.Handle(context.Background(), Event{Source: SourceCustom, Kind: "anything"})
			if len(fl.plans) != 0 {
				t.Fatalf("a bus event fired the %s subscription: %+v", name, fl.plans)
			}
		})
	}
}

// The bus never evaluates a connector event, whatever a store would answer
// for it: a match-everything subscription stays silent.
func TestTheBusNeverEvaluatesAConnectorEvent(t *testing.T) {
	st := unscopedCandidates{seed(t, Subscription{ID: "wide", BotID: "wide-bot", Enabled: true})}
	fl := &fakeLauncher{}
	ev := NewEvaluator(st, WithLauncher(fl))
	_ = ev.Handle(context.Background(), Event{Source: SourceConnector, Kind: "slack.message.posted", ConnectionID: "conn-a"})
	if len(fl.plans) != 0 {
		t.Fatalf("a connector event was evaluated on the bus: %+v", fl.plans)
	}
	// The control: the same subscription fires on a bus event.
	_ = ev.Handle(context.Background(), Event{Source: SourceCustom, Kind: "anything"})
	if len(fl.plans) != 1 {
		t.Fatalf("the control fired %d times, want 1", len(fl.plans))
	}
}

package trigger

import (
	"context"
	"testing"
	"time"
)

// #2644. A subscription's credential pins (KeyOverrides / SecretOverrides)
// are carried from the stored subscription to the launch on BOTH spine lanes
// — the event lane through Evaluator.buildPlan, the timer lane through the
// Scheduler's own plan — and from there onto the run's LaunchSpec
// (pkg/server/trigger_launcher.go). What the spine has never had is a route
// that WRITES them; which carrier should write them is the ticket's open
// product question, but the carriage is not, and it is untested on both
// lanes. Without these two witnesses the writer would land on a conveyor
// nobody has ever watched run: a dropped copy on one lane reads exactly like
// a subscription that pins nothing, because an unresolvable pin produces no
// refusal anywhere downstream (secrets.Resolve reports a key it withholds,
// never a pin it never saw).

func pinnedSubscription(id, bot string) Subscription {
	return Subscription{
		ID: id, BotID: bot, Enabled: true,
		KeyOverrides:    map[string]string{"anthropic": "key-pinned-by-the-operator"},
		SecretOverrides: map[string]string{"forge_token": "sec-pinned-by-the-operator"},
	}
}

// assertCarriesThePins reads the pins off a plan the spine built. It asserts
// the VALUES, not merely a non-nil map: a lane that passed an empty map
// through would satisfy "the field is set" while sealing the team's default.
func assertCarriesThePins(t *testing.T, lane string, plan LaunchPlan) {
	t.Helper()
	if got := plan.KeyOverrides["anthropic"]; got != "key-pinned-by-the-operator" {
		t.Errorf("%s: plan.KeyOverrides[anthropic] = %q, want the subscription's pin — a run launched from this plan would seal the team's default key", lane, got)
	}
	if got := plan.SecretOverrides["forge_token"]; got != "sec-pinned-by-the-operator" {
		t.Errorf("%s: plan.SecretOverrides[forge_token] = %q, want the subscription's pin — the run would post under the team's default forge identity", lane, got)
	}
}

func TestTheEventLaneCarriesASubscriptionsCredentialPins(t *testing.T) {
	sub := pinnedSubscription("direct-pinned", "review-pr")
	sub.Mode = "direct"
	sub.Match = Matcher{Kinds: []string{KindCardMoved}}
	st := seed(t, sub)
	fl := &fakeLauncher{}
	ev := NewEvaluator(st, WithLauncher(fl), WithBoardEffect(&fakeBoard{}))

	if err := ev.Handle(context.Background(), Event{
		Source:  SourceBoard,
		Kind:    KindCardMoved,
		Subject: Subject{Type: "card", ID: "card7", State: "ready"},
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(fl.plans) != 1 {
		t.Fatalf("launched plans = %d, want 1", len(fl.plans))
	}
	assertCarriesThePins(t, "event lane", fl.plans[0])
}

func TestTheScheduleLaneCarriesASubscriptionsCredentialPins(t *testing.T) {
	sub := pinnedSubscription("nightly-pinned", "docs-refresh")
	sub.Invocation = "schedule"
	sub.Mode = "direct"
	sub.Cron = "0 2 * * *"
	st := NewMemorySubscriptionStore()
	if err := st.Create(context.Background(), sub); err != nil {
		t.Fatalf("seed: %v", err)
	}
	fl := &fakeLauncher{}
	clk := time.Date(2026, 6, 25, 1, 0, 0, 0, time.UTC)
	sch := NewScheduler(st, fl, WithSchedulerClock(func() time.Time { return clk }))

	sch.tick(context.Background(), clk) // arms only
	clk = time.Date(2026, 6, 25, 2, 0, 30, 0, time.UTC)
	sch.tick(context.Background(), clk)

	if len(fl.plans) != 1 {
		t.Fatalf("fired plans = %d, want 1", len(fl.plans))
	}
	assertCarriesThePins(t, "schedule lane", fl.plans[0])
}

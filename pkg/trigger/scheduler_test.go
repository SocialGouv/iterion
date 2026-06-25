package trigger

import (
	"context"
	"testing"
	"time"
)

func TestSchedulerFiresDueSubscription(t *testing.T) {
	st := NewMemorySubscriptionStore()
	_ = st.Create(context.Background(), Subscription{
		ID: "nightly", BotID: "docs-refresh", Invocation: "schedule", Mode: "direct",
		Enabled: true, Cron: "0 2 * * *", // 02:00 daily
		Vars: map[string]string{"scope": "all"},
	})
	fl := &fakeLauncher{}

	// Clock we advance by hand; arm at 01:00, then jump past 02:00.
	clk := time.Date(2026, 6, 25, 1, 0, 0, 0, time.UTC)
	sch := NewScheduler(st, fl, WithSchedulerClock(func() time.Time { return clk }))

	// First tick only ARMS (computes next fire = 02:00); must not fire.
	sch.tick(context.Background(), clk)
	if len(fl.plans) != 0 {
		t.Fatalf("armed tick fired %d, want 0", len(fl.plans))
	}

	// Advance past the due instant → fires once.
	clk = time.Date(2026, 6, 25, 2, 0, 30, 0, time.UTC)
	sch.tick(context.Background(), clk)
	if len(fl.plans) != 1 || fl.plans[0].BotID != "docs-refresh" {
		t.Fatalf("due tick plans = %+v, want one docs-refresh", fl.plans)
	}
	if fl.plans[0].Vars["scope"] != "all" {
		t.Fatalf("schedule vars not propagated: %+v", fl.plans[0].Vars)
	}

	// Same minute, not yet at the NEXT due instant → no re-fire.
	sch.tick(context.Background(), clk)
	if len(fl.plans) != 1 {
		t.Fatalf("re-fired within slot: %d", len(fl.plans))
	}
}

func TestSchedulerIgnoresNonScheduleAndDisabled(t *testing.T) {
	st := NewMemorySubscriptionStore()
	_ = st.Create(context.Background(), Subscription{ID: "board", BotID: "x", Invocation: "board", Enabled: true})
	_ = st.Create(context.Background(), Subscription{ID: "off", BotID: "y", Invocation: "schedule", Cron: "* * * * *", Enabled: false})
	fl := &fakeLauncher{}
	clk := time.Date(2026, 6, 25, 1, 0, 0, 0, time.UTC)
	sch := NewScheduler(st, fl, WithSchedulerClock(func() time.Time { return clk }))

	sch.tick(context.Background(), clk)
	clk = clk.Add(2 * time.Minute)
	sch.tick(context.Background(), clk)
	if len(fl.plans) != 0 {
		t.Fatalf("non-schedule/disabled fired: %+v", fl.plans)
	}
}

func TestSchedulerNoLauncherIsNoop(t *testing.T) {
	st := NewMemorySubscriptionStore()
	_ = st.Create(context.Background(), Subscription{ID: "s", BotID: "x", Invocation: "schedule", Cron: "* * * * *", Enabled: true})
	sch := NewScheduler(st, nil) // nil launcher
	// Must not panic.
	sch.tick(context.Background(), time.Now())
}

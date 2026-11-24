package cloudsched_test

import (
	"context"
	"github.com/SocialGouv/iterion/pkg/cloudsched"
	"github.com/SocialGouv/iterion/pkg/schedgate"
	"github.com/SocialGouv/iterion/pkg/trigger"
	"testing"
	"time"
)

func TestRound2ScheduleHoldSurvivesTickerRestart(t *testing.T) {
	ctx := context.Background()
	st := cloudsched.NewMemoryStore()
	now := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	if err := st.Create(ctx, cloudsched.ScheduledBot{ID: "daily", BotID: "bot", Cron: "0 9 * * *", NextFireAt: now, Vars: map[string]string{"data": "original"}}); err != nil {
		t.Fatal(err)
	}
	hold := true
	launches := 0
	launch := func(_ context.Context, sb cloudsched.ScheduledBot) error {
		if hold {
			return trigger.ErrMaintenanceHold
		}
		launches++
		if sb.Vars["data"] != "original" {
			t.Errorf("lost vars: %+v", sb)
		}
		return nil
	}
	ticker := &cloudsched.Ticker{Store: st, Launch: launch, Now: func() time.Time { return now }}
	if _, err := ticker.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	sb, _ := st.Get(ctx, "daily")
	if !sb.NextFireAt.After(now.Add(time.Hour)) {
		t.Fatal("calendar did not advance")
	}
	hold = false
	now = now.Add(time.Minute)
	ticker = &cloudsched.Ticker{Store: st, Launch: launch, Now: func() time.Time { return now }}
	if _, err := ticker.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := ticker.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if launches != 1 {
		t.Fatalf("09:01 launches=%d, want 1 without waiting until tomorrow", launches)
	}
}

func TestRound2HeldIntentDoesNotRerunConsumedGuard(t *testing.T) {
	ctx := context.Background()
	st := cloudsched.NewMemoryStore()
	// Pinned base, never the wall clock: a time.Now() base can land within the
	// minute before the daily 09:00 cron instant, and the stepped +1m then
	// materializes that NEW slot in the second tick — a second guard execution
	// that is correct per-slot behavior, not a re-run of the held slot's. The
	// invariant is per-slot: the HELD slot's consumed guard never runs again.
	now := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	slot := now
	if err := st.Create(ctx, cloudsched.ScheduledBot{ID: "guarded", BotID: "bot", Cron: "0 9 * * *", NextFireAt: now, Guard: "consume", GuardVar: "input"}); err != nil {
		t.Fatal(err)
	}
	hold := true
	guardRuns, launches := map[time.Time]int{}, 0
	ticker := &cloudsched.Ticker{Store: st, Now: func() time.Time { return now }, Gate: func(_ context.Context, sb cloudsched.ScheduledBot) (bool, string, schedgate.TickRecord) {
		if sb.Guard == "" {
			return true, "", schedgate.TickRecord{}
		}
		guardRuns[sb.NextFireAt]++
		return guardRuns[sb.NextFireAt] == 1, "original", schedgate.TickRecord{}
	}, Launch: func(_ context.Context, sb cloudsched.ScheduledBot) error {
		if hold {
			return trigger.ErrMaintenanceHold
		}
		launches++
		if sb.Vars["input"] != "original" {
			t.Fatal("lost guard payload")
		}
		return nil
	}}
	if _, err := ticker.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	hold = false
	now = now.Add(time.Minute)
	if _, err := ticker.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(guardRuns) != 1 {
		t.Fatalf("guards ran for slots %v, want the one pinned slot — the base drifted onto a cron boundary", guardRuns)
	}
	if guardRuns[slot] != 1 || launches != 1 {
		t.Fatalf("held slot guard executions=%d launches=%d, want 1/1", guardRuns[slot], launches)
	}
}

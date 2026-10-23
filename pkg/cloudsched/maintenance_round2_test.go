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
	now := time.Now().UTC()
	if err := st.Create(ctx, cloudsched.ScheduledBot{ID: "guarded", BotID: "bot", Cron: "0 9 * * *", NextFireAt: now, GuardVar: "input"}); err != nil {
		t.Fatal(err)
	}
	hold := true
	guards, launches := 0, 0
	ticker := &cloudsched.Ticker{Store: st, Now: func() time.Time { return now }, Gate: func(context.Context, cloudsched.ScheduledBot) (bool, string, schedgate.TickRecord) {
		guards++
		return guards == 1, "original", schedgate.TickRecord{}
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
	if launches != 1 || guards != 1 {
		t.Fatalf("launches=%d guard executions=%d", launches, guards)
	}
}

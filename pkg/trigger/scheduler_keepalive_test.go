package trigger

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/schedgate"
	"github.com/SocialGouv/iterion/pkg/store"
)

// keepaliveLister is a schedgate lister with per-run UpdatedAt control,
// so staleness (silence past StaleAfter) is exercisable.
type keepaliveLister struct {
	ids  map[string][]string
	runs map[string]*store.Run
}

func (k *keepaliveLister) ListRunsBySchedule(_ context.Context, id string) ([]string, error) {
	return k.ids[id], nil
}

func (k *keepaliveLister) LoadRun(_ context.Context, id string) (*store.Run, error) {
	r, ok := k.runs[id]
	if !ok {
		return nil, store.ErrRunNotFound
	}
	return r, nil
}

func keepaliveSub(id string, sec int) Subscription {
	return Subscription{
		ID: id, BotID: "bots/daemon", Invocation: bundle.InvocationKindKeepalive, Mode: "direct",
		Enabled: true, IntervalSeconds: sec,
		Overlap: schedgate.OverlapKeepalive, StaleAfter: "5m",
	}
}

func TestScheduler_KeepaliveSubMinuteCadence(t *testing.T) {
	st := NewMemorySubscriptionStore()
	_ = st.Create(context.Background(), keepaliveSub("daemon", 30))
	fl := &fakeLauncher{}
	clk := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	sch := NewScheduler(st, fl,
		WithSchedulerClock(func() time.Time { return clk }),
		WithSchedulerInterval(5*time.Second),
		WithSchedulerGate(&ScheduleGate{Lister: &keepaliveLister{}}),
	)

	sch.tick(context.Background(), clk) // arm: next fire = +30s
	if len(fl.plans) != 0 {
		t.Fatalf("armed tick fired %d, want 0", len(fl.plans))
	}
	// +10s: not yet due (sub-minute — a cron scheduler could not express this).
	clk = clk.Add(10 * time.Second)
	sch.tick(context.Background(), clk)
	if len(fl.plans) != 0 {
		t.Fatalf("fired before interval elapsed: %d", len(fl.plans))
	}
	// +35s total: due → fires once.
	clk = clk.Add(25 * time.Second)
	sch.tick(context.Background(), clk)
	if len(fl.plans) != 1 || fl.plans[0].BotID != "bots/daemon" {
		t.Fatalf("due keepalive tick plans = %+v, want one daemon", fl.plans)
	}
}

func TestScheduler_KeepaliveAtMostOneLive(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	st := NewMemorySubscriptionStore()
	_ = st.Create(context.Background(), keepaliveSub("daemon", 30))
	fl := &fakeLauncher{}
	// A fresh, actively-running run blocks the relaunch.
	lister := &keepaliveLister{
		ids:  map[string][]string{"daemon": {"live"}},
		runs: map[string]*store.Run{"live": {ID: "live", Status: store.RunStatusRunning, UpdatedAt: now.Add(-1 * time.Minute)}},
	}
	clk := now
	sch := NewScheduler(st, fl,
		WithSchedulerClock(func() time.Time { return clk }),
		WithSchedulerInterval(5*time.Second),
		WithSchedulerGate(&ScheduleGate{Lister: lister}),
	)
	sch.tick(context.Background(), clk) // arm
	clk = clk.Add(31 * time.Second)
	sch.tick(context.Background(), clk) // due, but a healthy run is live
	if len(fl.plans) != 0 {
		t.Fatalf("keepalive must not relaunch over a healthy run, got %d", len(fl.plans))
	}
}

func TestScheduler_KeepaliveReapsStaleAndRelaunches(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	st := NewMemorySubscriptionStore()
	_ = st.Create(context.Background(), keepaliveSub("daemon", 30))
	fl := &fakeLauncher{}
	// A silent (stale) running run: 10m since last progress, StaleAfter=5m.
	lister := &keepaliveLister{
		ids:  map[string][]string{"daemon": {"zombie"}},
		runs: map[string]*store.Run{"zombie": {ID: "zombie", Status: store.RunStatusRunning, UpdatedAt: now.Add(-10 * time.Minute)}},
	}
	var reaped []string
	clk := now
	sch := NewScheduler(st, fl,
		WithSchedulerClock(func() time.Time { return clk }),
		WithSchedulerInterval(5*time.Second),
		WithSchedulerGate(&ScheduleGate{
			Lister: lister,
			Reap:   func(_ context.Context, ids []string) { reaped = append(reaped, ids...) },
		}),
	)
	sch.tick(context.Background(), clk) // arm
	clk = clk.Add(31 * time.Second)
	sch.tick(context.Background(), clk) // due: stale run doesn't block

	if len(fl.plans) != 1 {
		t.Fatalf("stale keepalive run must relaunch, got %d plans", len(fl.plans))
	}
	if len(reaped) != 1 || reaped[0] != "zombie" {
		t.Fatalf("reaped = %v, want [zombie]", reaped)
	}
}

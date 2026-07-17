package trigger

import (
	"context"
	"testing"
	"time"
)

// panicLauncher panics on the first launch, then records.
type panicLauncher struct {
	calls  int
	panics int
}

func (p *panicLauncher) Launch(context.Context, LaunchPlan) (string, error) {
	p.calls++
	if p.calls <= p.panics {
		panic("boom")
	}
	return "run-ok", nil
}

func TestScheduler_PanicInOneFireDoesNotKillTheTick(t *testing.T) {
	st := NewMemorySubscriptionStore()
	// Two due subscriptions: the first fire panics, the second must
	// still launch within the SAME tick.
	_ = st.Create(context.Background(), minuteSub("boom"))
	_ = st.Create(context.Background(), minuteSub("fine"))
	pl := &panicLauncher{panics: 1}

	clk := time.Date(2026, 7, 17, 1, 0, 0, 0, time.UTC)
	sch := NewScheduler(st, pl, WithSchedulerClock(func() time.Time { return clk }))
	sch.tick(context.Background(), clk) // arm both
	clk = clk.Add(2 * time.Minute)
	sch.tick(context.Background(), clk) // both due; one panics

	if pl.calls != 2 {
		t.Fatalf("launch calls = %d, want 2 (panic contained per subscription)", pl.calls)
	}
}

func TestScheduler_StatusReportsLiveness(t *testing.T) {
	st := NewMemorySubscriptionStore()
	_ = st.Create(context.Background(), minuteSub("s1"))
	fl := &fakeLauncher{}
	clk := time.Date(2026, 7, 17, 1, 0, 0, 0, time.UTC)
	sch := NewScheduler(st, fl, WithSchedulerClock(func() time.Time { return clk }))

	if got := sch.Status(); !got.LastTickAt.IsZero() {
		t.Fatalf("pre-tick LastTickAt = %v, want zero", got.LastTickAt)
	}
	sch.tick(context.Background(), clk)
	got := sch.Status()
	if !got.LastTickAt.Equal(clk) || got.Subscriptions != 1 || got.Armed != 1 {
		t.Fatalf("status = %+v", got)
	}
	if got.IntervalSeconds != 60 {
		t.Fatalf("interval = %v, want 60s", got.IntervalSeconds)
	}

	// Nil receiver is safe (unwired coordinator).
	var nilSch *Scheduler
	if got := nilSch.Status(); got.Subscriptions != 0 {
		t.Fatalf("nil status = %+v", got)
	}
}

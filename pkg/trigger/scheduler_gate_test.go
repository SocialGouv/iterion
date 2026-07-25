package trigger

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/schedgate"
	"github.com/SocialGouv/iterion/pkg/store"
)

// fakeScheduleLister satisfies schedgate.ScheduleRunLister with a fixed
// live-run set per schedule id.
type fakeScheduleLister struct {
	liveBySchedule map[string][]string
}

func (f *fakeScheduleLister) ListRunsBySchedule(_ context.Context, scheduleID string) ([]string, error) {
	return f.liveBySchedule[scheduleID], nil
}

func (f *fakeScheduleLister) LoadRun(_ context.Context, id string) (*store.Run, error) {
	return &store.Run{ID: id, Status: store.RunStatusRunning}, nil
}

// fireDue arms then advances the scheduler clock so the subscription
// fires exactly once through the gate.
func fireDue(t *testing.T, st SubscriptionStore, fl Launcher, gate *ScheduleGate) {
	t.Helper()
	clk := time.Date(2026, 7, 17, 1, 0, 0, 0, time.UTC)
	sch := NewScheduler(st, fl,
		WithSchedulerClock(func() time.Time { return clk }),
		WithSchedulerGate(gate),
	)
	sch.tick(context.Background(), clk) // arm
	clk = clk.Add(2 * time.Minute)
	sch.tick(context.Background(), clk) // due (cron * * * * *)
}

func minuteSub(id string) Subscription {
	return Subscription{
		ID: id, BotID: "bots/demo", Invocation: "schedule", Mode: "direct",
		Enabled: true, Cron: "* * * * *",
	}
}

func TestScheduler_OverlapGateSkipsAndAudits(t *testing.T) {
	st := NewMemorySubscriptionStore()
	_ = st.Create(context.Background(), minuteSub("sub1"))
	fl := &fakeLauncher{}
	var audits []schedgate.TickRecord
	gate := &ScheduleGate{
		Lister: &fakeScheduleLister{liveBySchedule: map[string][]string{"sub1": {"r_live"}}},
		Audit:  func(rec schedgate.TickRecord) { audits = append(audits, rec) },
	}

	fireDue(t, st, fl, gate)

	if len(fl.plans) != 0 {
		t.Fatalf("launch must be gated by overlap, got %d plans", len(fl.plans))
	}
	if len(audits) != 1 {
		t.Fatalf("want 1 audit record, got %d", len(audits))
	}
	rec := audits[0]
	if rec.Decision != schedgate.TickSkippedOverlap || rec.BlockingRunID != "r_live" ||
		rec.Surface != schedgate.SurfaceTrigger || rec.ScheduleID != "sub1" {
		t.Fatalf("audit mismatch: %+v", rec)
	}
}

func TestScheduler_StampsSourceRefOnPlan(t *testing.T) {
	st := NewMemorySubscriptionStore()
	_ = st.Create(context.Background(), minuteSub("sub2"))
	fl := &fakeLauncher{}
	var audits []schedgate.TickRecord
	gate := &ScheduleGate{
		Lister: &fakeScheduleLister{liveBySchedule: map[string][]string{}},
		Audit:  func(rec schedgate.TickRecord) { audits = append(audits, rec) },
	}

	fireDue(t, st, fl, gate)

	if len(fl.plans) != 1 {
		t.Fatalf("want 1 plan, got %d", len(fl.plans))
	}
	src := fl.plans[0].SourceRef
	if src == nil || src.Kind != store.RunSourceKindSchedule || src.ScheduleID != "sub2" {
		t.Fatalf("SourceRef = %+v, want schedule provenance", src)
	}
	if len(audits) != 1 || audits[0].Decision != schedgate.TickFired || audits[0].RunID == "" {
		t.Fatalf("fired audit mismatch: %+v", audits)
	}
}

func TestScheduler_SourceRefStampedWithoutGate(t *testing.T) {
	// Provenance is not conditional on the gate being wired: a bare
	// scheduler still stamps SourceRef so the next process that DOES
	// gate can count these runs.
	st := NewMemorySubscriptionStore()
	_ = st.Create(context.Background(), minuteSub("sub3"))
	fl := &fakeLauncher{}

	fireDue(t, st, fl, nil)

	if len(fl.plans) != 1 || fl.plans[0].SourceRef == nil || fl.plans[0].SourceRef.ScheduleID != "sub3" {
		t.Fatalf("plans = %+v, want SourceRef stamped without gate", fl.plans)
	}
}

func TestScheduler_GuardMergesStdoutIntoVars(t *testing.T) {
	st := NewMemorySubscriptionStore()
	sub := minuteSub("sub4")
	sub.Guard = "echo hi"
	sub.GuardVar = "msg"
	_ = st.Create(context.Background(), sub)
	fl := &fakeLauncher{}
	gate := &ScheduleGate{Lister: &fakeScheduleLister{}}

	fireDue(t, st, fl, gate)

	if len(fl.plans) != 1 {
		t.Fatalf("want 1 plan, got %d", len(fl.plans))
	}
	if got := fl.plans[0].Vars["msg"]; got != "hi\n" {
		t.Fatalf("Vars[msg] = %q, want %q", got, "hi\n")
	}
}

func TestScheduler_GuardBlockedSkipsLaunch(t *testing.T) {
	st := NewMemorySubscriptionStore()
	sub := minuteSub("sub5")
	sub.Guard = "exit 4"
	_ = st.Create(context.Background(), sub)
	fl := &fakeLauncher{}
	var audits []schedgate.TickRecord
	gate := &ScheduleGate{
		Lister: &fakeScheduleLister{},
		Audit:  func(rec schedgate.TickRecord) { audits = append(audits, rec) },
	}

	fireDue(t, st, fl, gate)

	if len(fl.plans) != 0 {
		t.Fatalf("guard block must not launch, got %d plans", len(fl.plans))
	}
	if len(audits) != 1 || audits[0].Decision != schedgate.TickGuardBlocked {
		t.Fatalf("audit: %+v", audits)
	}
	if audits[0].GuardExit == nil || *audits[0].GuardExit != 4 {
		t.Fatalf("GuardExit = %v, want 4", audits[0].GuardExit)
	}
}

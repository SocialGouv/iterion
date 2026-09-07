package cloudsched

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A tick that could not launch — an org launch-gate denial above all — must be
// readable on the schedule itself. Before this the only trace was one Warn in
// whichever replica happened to win the CAS, so a schedule silently stopped
// producing runs and `iterion remote schedules list` still showed it healthy.

func dueSchedule(t *testing.T, s Store, now time.Time) ScheduledBot {
	t.Helper()
	sb := ScheduledBot{ID: "sb-1", TenantID: "t1", BotID: "probe", Cron: "* * * * *", NextFireAt: now.Add(-time.Minute)}
	if err := s.Create(context.Background(), sb); err != nil {
		t.Fatal(err)
	}
	return sb
}

func TestTicker_LaunchRefusalLandsOnTheSchedule(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	now := time.Now().UTC()
	dueSchedule(t, st, now)
	refusal := errors.New("launch gate: concurrency_cap_exceeded: org has 3 active runs (cap 3)")
	tk := &Ticker{
		Store:  st,
		Launch: func(context.Context, ScheduledBot) error { return refusal },
		Now:    func() time.Time { return now },
	}

	if fired, err := tk.Tick(ctx); err != nil || fired != 1 {
		t.Fatalf("Tick = (%d, %v), want (1, nil)", fired, err)
	}
	got, err := st.Get(ctx, "sb-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.LastError == "" {
		t.Fatal("the refused tick left no last_error on the schedule — the operator has nothing to read but a pod log")
	}
	if got.LastError != refusal.Error() {
		t.Errorf("last_error = %q, want the refusal verbatim %q", got.LastError, refusal.Error())
	}
	if got.LastErrorAt == nil {
		t.Error("last_error carries no instant — an operator cannot tell today's refusal from last month's")
	}
}

func TestTicker_ATickThatLaunchesClearsTheFlag(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	now := time.Now().UTC()
	dueSchedule(t, st, now)
	if err := st.MarkLaunchError(ctx, "sb-1", "launch gate: monthly_run_quota_exceeded", now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	tk := &Ticker{
		Store:  st,
		Launch: func(context.Context, ScheduledBot) error { return nil },
		Now:    func() time.Time { return now },
	}

	if _, err := tk.Tick(ctx); err != nil {
		t.Fatalf("Tick = %v, want nil", err)
	}
	got, _ := st.Get(ctx, "sb-1")
	if got.LastError != "" || got.LastErrorAt != nil {
		t.Errorf("a tick that launched left last_error=%q at=%v — a stale refusal reads as a live one", got.LastError, got.LastErrorAt)
	}
}

// The health write is targeted: an operator retuning the cron between the CAS
// and the record must not lose the edit to it.
func TestMemoryStore_MarkLaunchErrorDoesNotClobberAnEdit(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	now := time.Now().UTC()
	dueSchedule(t, st, now)
	cron := "0 4 * * *"
	if _, err := st.Update(ctx, "sb-1", SchedulePatch{Cron: &cron, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkLaunchError(ctx, "sb-1", "boom", now); err != nil {
		t.Fatal(err)
	}
	got, _ := st.Get(ctx, "sb-1")
	if got.Cron != cron {
		t.Errorf("cron = %q, want %q — the health write replaced the row instead of setting its two fields", got.Cron, cron)
	}
	if got.LastError != "boom" || got.LastErrorAt == nil {
		t.Errorf("health = (%q, %v), want (\"boom\", set)", got.LastError, got.LastErrorAt)
	}
	if err := st.MarkLaunchError(ctx, "ghost", "boom", now); !errors.Is(err, ErrNotFound) {
		t.Errorf("MarkLaunchError on an unknown id = %v, want ErrNotFound", err)
	}
}

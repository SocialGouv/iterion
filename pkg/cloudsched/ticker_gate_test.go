package cloudsched

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/schedgate"
)

// gateTickerFixture seeds one due schedule and returns a ticker whose
// Launch and Audit calls are recorded.
type gateTickerFixture struct {
	store    *MemoryStore
	launches []ScheduledBot
	audits   []schedgate.TickRecord
	now      time.Time
}

func newGateTicker(t *testing.T, sb ScheduledBot, gate GateFunc) (*Ticker, *gateTickerFixture) {
	t.Helper()
	f := &gateTickerFixture{
		store: NewMemoryStore(),
		now:   time.Date(2026, 7, 17, 3, 0, 30, 0, time.UTC),
	}
	sb.NextFireAt = f.now.Add(-time.Minute)
	if sb.Cron == "" {
		sb.Cron = "* * * * *"
	}
	if err := f.store.Create(context.Background(), sb); err != nil {
		t.Fatal(err)
	}
	tk := &Ticker{
		Store: f.store,
		Now:   func() time.Time { return f.now },
		Launch: func(_ context.Context, got ScheduledBot) error {
			f.launches = append(f.launches, got)
			return nil
		},
		Gate:  gate,
		Audit: func(rec schedgate.TickRecord) { f.audits = append(f.audits, rec) },
	}
	return tk, f
}

func TestTicker_GateSkipStillConsumesSlot(t *testing.T) {
	sb := ScheduledBot{ID: "sb1", TenantID: "t1", BotID: "bots/demo"}
	gate := func(_ context.Context, got ScheduledBot) (bool, string, schedgate.TickRecord) {
		rec := schedgate.NewTickRecord(schedgate.SurfaceCloud, got.ID, time.Now(), schedgate.TickSkippedOverlap)
		rec.BlockingRunID = "r_live"
		rec.Reason = "blocked by live run r_live"
		return false, "", rec
	}
	tk, f := newGateTicker(t, sb, gate)

	fired, err := tk.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(f.launches) != 0 {
		t.Fatalf("gate skip must not launch, got %d", len(f.launches))
	}
	if fired != 1 {
		t.Fatalf("fired = %d, want 1 (slot consumed even on skip)", fired)
	}
	// The CAS advanced NextFireAt: the slot is consumed, no re-fire at `now`.
	due, _ := f.store.ListDue(context.Background(), f.now, 0)
	if len(due) != 0 {
		t.Fatalf("schedule still due after gated skip — CAS not advanced")
	}
	if len(f.audits) != 1 || f.audits[0].Decision != schedgate.TickSkippedOverlap || f.audits[0].BlockingRunID != "r_live" {
		t.Fatalf("audit: %+v", f.audits)
	}
}

func TestTicker_GateProceedInjectsGuardVarAndAuditsFired(t *testing.T) {
	sb := ScheduledBot{ID: "sb2", TenantID: "t1", BotID: "bots/demo",
		GuardVar: "work", Vars: map[string]string{"keep": "me"}}
	gate := func(context.Context, ScheduledBot) (bool, string, schedgate.TickRecord) {
		return true, `{"issues":2}`, schedgate.TickRecord{}
	}
	tk, f := newGateTicker(t, sb, gate)

	if _, err := tk.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(f.launches) != 1 {
		t.Fatalf("want 1 launch, got %d", len(f.launches))
	}
	got := f.launches[0]
	if got.Vars["work"] != `{"issues":2}` || got.Vars["keep"] != "me" {
		t.Fatalf("Vars = %+v, want guard stdout under 'work' + originals kept", got.Vars)
	}
	// The stored row's Vars map must NOT have been mutated (copy-on-write).
	stored, err := f.store.Get(context.Background(), "sb2")
	if err != nil {
		t.Fatal(err)
	}
	if _, leaked := stored.Vars["work"]; leaked {
		t.Fatalf("guard stdout leaked into the stored schedule row: %+v", stored.Vars)
	}
	if len(f.audits) != 1 || f.audits[0].Decision != schedgate.TickFired {
		t.Fatalf("audit: %+v", f.audits)
	}
}

func TestTicker_NoGateFiresAndAudits(t *testing.T) {
	sb := ScheduledBot{ID: "sb3", TenantID: "t1", BotID: "bots/demo"}
	tk, f := newGateTicker(t, sb, nil)

	if _, err := tk.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(f.launches) != 1 {
		t.Fatalf("want 1 launch, got %d", len(f.launches))
	}
	if len(f.audits) != 1 || f.audits[0].Decision != schedgate.TickFired || f.audits[0].Surface != schedgate.SurfaceCloud {
		t.Fatalf("audit: %+v", f.audits)
	}
}

func TestScheduledBotPolicyAndPatch(t *testing.T) {
	sb := ScheduledBot{Overlap: "allow", MaxConcurrent: 2, Guard: "true", GuardTimeout: "5s", GuardVar: "v"}
	p := sb.Policy()
	if p.Overlap != schedgate.OverlapAllow || p.MaxConcurrent != 2 || p.GuardVar != "v" {
		t.Fatalf("Policy() = %+v", p)
	}
	// Zero-value row normalizes to skip.
	if p := (ScheduledBot{}).Policy(); p.Overlap != schedgate.OverlapSkip || p.GuardVar != schedgate.DefaultGuardVar {
		t.Fatalf("zero Policy() = %+v", p)
	}

	guard := "gh issue list"
	overlap := "allow"
	maxC := 3
	patch := SchedulePatch{Overlap: &overlap, MaxConcurrent: &maxC, Guard: &guard, UpdatedAt: time.Now()}
	applySchedulePatch(&sb, patch)
	if sb.Overlap != "allow" || sb.MaxConcurrent != 3 || sb.Guard != guard {
		t.Fatalf("patched = %+v", sb)
	}
}

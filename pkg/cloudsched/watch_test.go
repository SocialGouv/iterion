package cloudsched

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/schedgate"
)

func TestAssessScheduleStall(t *testing.T) {
	now := time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)
	grace := 2 * time.Hour
	errAt := now.Add(-3 * time.Hour)
	healthy := ScheduledBot{ID: "sb-1", BotID: "feed-watch", Cron: "0 7 * * *",
		NextFireAt: now.Add(22 * time.Hour), LastRunID: "run-ok", LastRunStatus: "finished"}
	with := func(mut func(*ScheduledBot)) ScheduledBot {
		sb := healthy
		mut(&sb)
		return sb
	}
	cases := []struct {
		name        string
		sb          ScheduledBot
		wantCause   StallCause // "" = healthy
		wantEpisode string
		wantReason  string
	}{
		{name: "healthy", sb: healthy},
		{name: "never fired", sb: with(func(sb *ScheduledBot) { sb.NextFireAt = time.Time{}; sb.LastRunID, sb.LastRunStatus = "", "" })},
		{name: "due within the grace", sb: with(func(sb *ScheduledBot) { sb.NextFireAt = now.Add(-grace) })},
		{name: "overdue past the grace", sb: with(func(sb *ScheduledBot) { sb.NextFireAt = now.Add(-grace - time.Minute) }),
			wantCause: StallOverdue, wantEpisode: "overdue:", wantReason: "overdue"},
		{name: "launch refused", sb: with(func(sb *ScheduledBot) {
			sb.LastError = "launch gate: concurrency_cap_exceeded"
			sb.LastErrorAt = &errAt
		}),
			wantCause: StallLaunchRefused, wantEpisode: "launch:", wantReason: "concurrency_cap_exceeded"},
		{name: "launch refused without its instant", sb: with(func(sb *ScheduledBot) { sb.LastError = "boom" }),
			wantCause: StallLaunchRefused, wantEpisode: "launch:e:", wantReason: "boom"},
		{name: "last run failed", sb: with(func(sb *ScheduledBot) {
			sb.LastRunID, sb.LastRunStatus, sb.LastRunError, sb.LastRunErrorCode = "run-bad", "failed", "provider 401", "AUTH_FAILED"
		}), wantCause: StallRunFailed, wantEpisode: "run:run-bad:failed", wantReason: "[AUTH_FAILED] provider 401"},
		{name: "last run parked", sb: with(func(sb *ScheduledBot) { sb.LastRunID, sb.LastRunStatus = "run-park", "failed_resumable" }),
			wantCause: StallRunFailed, wantEpisode: "run:run-park:failed_resumable", wantReason: "failed_resumable"},
		{name: "last run cancelled", sb: with(func(sb *ScheduledBot) { sb.LastRunID, sb.LastRunStatus = "run-c", "cancelled" })},
		{name: "resumed after a long pause", sb: with(func(sb *ScheduledBot) {
			sb.NextFireAt = now.Add(-26 * time.Hour)
			sb.UpdatedAt = now.Add(-time.Minute)
		})},
		{name: "resumed, then scheduler dead", sb: with(func(sb *ScheduledBot) {
			sb.NextFireAt = now.Add(-26 * time.Hour)
			sb.UpdatedAt = now.Add(-grace - time.Minute)
		}), wantCause: StallOverdue, wantEpisode: "overdue:", wantReason: "26h0m0s in the past"},
		{name: "disabled and broken", sb: with(func(sb *ScheduledBot) {
			sb.Disabled = true
			sb.NextFireAt = now.Add(-48 * time.Hour)
			sb.LastError = "boom"
			sb.LastRunID, sb.LastRunStatus = "run-bad", "failed"
		})},
		{name: "overdue masks the older verdicts", sb: with(func(sb *ScheduledBot) {
			sb.NextFireAt = now.Add(-5 * time.Hour)
			sb.LastError = "boom"
			sb.LastRunID, sb.LastRunStatus = "run-bad", "failed"
		}), wantCause: StallOverdue, wantEpisode: "overdue:"},
		{name: "refusal masks the older failed run", sb: with(func(sb *ScheduledBot) {
			sb.LastError = "boom"
			sb.LastErrorAt = &errAt
			sb.LastRunID, sb.LastRunStatus = "run-bad", "failed"
		}), wantCause: StallLaunchRefused, wantEpisode: "launch:"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := AssessScheduleStall(tc.sb, now, grace)
			if tc.wantCause == "" {
				if got != nil {
					t.Fatalf("AssessScheduleStall = %+v, want healthy (nil)", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("AssessScheduleStall = nil, want cause %s", tc.wantCause)
			}
			if got.Cause != tc.wantCause {
				t.Fatalf("cause = %s, want %s", got.Cause, tc.wantCause)
			}
			if !strings.HasPrefix(got.Episode, tc.wantEpisode) {
				t.Fatalf("episode = %q, want prefix %q", got.Episode, tc.wantEpisode)
			}
			if !strings.Contains(got.Reason, tc.sb.BotID) || !strings.Contains(got.Reason, tc.sb.Cron) || !strings.Contains(got.Reason, tc.wantReason) {
				t.Fatalf("reason = %q, want the bot, the cron and %q", got.Reason, tc.wantReason)
			}
		})
	}
}

// The episode is what dedups the alert: stable while the row keeps showing
// the same stall, new when the row shows another one.
func TestAssessScheduleStallEpisodes(t *testing.T) {
	now := time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)
	grace := 2 * time.Hour
	stuck := ScheduledBot{ID: "sb-1", BotID: "b", Cron: "0 * * * *", NextFireAt: now.Add(-3 * time.Hour)}
	first := AssessScheduleStall(stuck, now, grace)
	later := AssessScheduleStall(stuck, now.Add(6*time.Hour), grace)
	if first == nil || later == nil || first.Episode != later.Episode {
		t.Fatalf("a slot stuck across sweeps changed episode: %+v → %+v", first, later)
	}
	at1, at2 := now.Add(-time.Hour), now.Add(-10*time.Minute)
	refused := ScheduledBot{ID: "sb-1", BotID: "b", Cron: "0 * * * *", NextFireAt: now.Add(time.Hour), LastError: "boom", LastErrorAt: &at1}
	again := refused
	again.LastErrorAt = &at2 // cleared by a healthy tick, then raised again
	if a, b := AssessScheduleStall(refused, now, grace), AssessScheduleStall(again, now, grace); a.Episode == b.Episode {
		t.Fatalf("a refusal raised again after a clear reused episode %q", a.Episode)
	}
	failed := ScheduledBot{ID: "sb-1", BotID: "b", Cron: "0 * * * *", NextFireAt: now.Add(time.Hour), LastRunID: "run-1", LastRunStatus: "failed"}
	next := failed
	next.LastRunID = "run-2"
	if a, b := AssessScheduleStall(failed, now, grace), AssessScheduleStall(next, now, grace); a.Episode == b.Episode {
		t.Fatalf("a new failed run reused episode %q", a.Episode)
	}
}

// A slot the operator's armed maintenance fence holds is not consumed, so
// the held launch is no verdict on the schedule: the row's last_error stays
// what it was.
func TestTickerHeldSlotWritesNoLaunchVerdict(t *testing.T) {
	for _, prior := range []string{"", "launch gate: concurrency_cap_exceeded"} {
		t.Run("prior="+prior, func(t *testing.T) {
			held := fmt.Errorf("launch gate: maintenance_fence: armed: %w", schedgate.ErrMaintenanceFenceArmed)
			sb := tickOnce(t, prior, held)
			if sb.LastError != prior {
				t.Fatalf("last_error after a held slot = %q, want it unchanged (%q)", sb.LastError, prior)
			}
		})
	}
}

// A launch refused because its holder lost the schedule's lease is no verdict
// either: the successor that took the lease over owns the slot and records
// its own verdict, which the loser must not overwrite from its stale read.
func TestTickerLostLeaseWritesNoLaunchVerdict(t *testing.T) {
	for _, prior := range []string{"", "launch gate: concurrency_cap_exceeded"} {
		t.Run("prior="+prior, func(t *testing.T) {
			lost := fmt.Errorf("cloudpublisher: save run: %w", fmt.Errorf("schedule persistence: %w", schedgate.ErrScheduleLeaseLost))
			sb := tickOnce(t, prior, lost)
			if sb.LastError != prior {
				t.Fatalf("last_error after a lost lease = %q, want it unchanged (%q)", sb.LastError, prior)
			}
		})
	}
}

// A run that could not be persisted while the lease was held repeats on
// every tick while nothing launches: the ticker records it as the schedule's
// launch verdict, so the watch sees the refusal. The verdict is the hold's
// stable text, not the driver's error, so a driver message that varies from
// tick to tick (a connection id) keeps one verdict and one episode.
func TestTickerPersistenceHoldWritesStableLaunchVerdict(t *testing.T) {
	st, tick, launchErr := tickRig(t)
	for i, driver := range []string{"connection(10.0.0.7:27017[-41]) incomplete read", "connection(10.0.0.7:27017[-42]) incomplete read"} {
		*launchErr = fmt.Errorf("cloudpublisher: save run: %w", fmt.Errorf("%w: schedule persistence: %s", schedgate.ErrLaunchNotPersisted, driver))
		tick(i)
	}
	sb := getRow(t, st)
	if want := schedgate.ErrLaunchNotPersisted.Error(); sb.LastError != want {
		t.Fatalf("last_error after a persistence hold = %q, want %q", sb.LastError, want)
	}
	if sb.LastErrorAt == nil || !sb.LastErrorAt.Equal(tickRigNow) {
		t.Fatalf("last_error_at = %v, want the first hold's tick %v: a varying driver text re-opened the episode", sb.LastErrorAt, tickRigNow)
	}
}

// classedRefusal is a refusal that names its stable class apart from a text
// carrying a figure measured at the tick.
type classedRefusal struct{ active int }

func (e classedRefusal) Error() string {
	return fmt.Sprintf("launch gate: concurrency_cap_exceeded: org has %d active runs (cap 1)", e.active)
}
func (classedRefusal) LaunchVerdict() string { return "launch gate: concurrency_cap_exceeded" }

var _ schedgate.LaunchVerdictError = classedRefusal{}

// A refusal whose text changes with a measured figure records its stable
// class: the row keeps the first refusal's last_error_at, so the watch sees
// one episode while the refusal lasts.
func TestTickerClassedRefusalWritesStableLaunchVerdict(t *testing.T) {
	st, tick, launchErr := tickRig(t)
	for i, active := range []int{1, 2, 3} {
		*launchErr = fmt.Errorf("launch scheduled bot: %w", classedRefusal{active: active})
		tick(i)
	}
	sb := getRow(t, st)
	if want := "launch gate: concurrency_cap_exceeded"; sb.LastError != want {
		t.Fatalf("last_error after a classed refusal = %q, want its class %q", sb.LastError, want)
	}
	if sb.LastErrorAt == nil || !sb.LastErrorAt.Equal(tickRigNow) {
		t.Fatalf("last_error_at = %v, want the first refusal's tick %v: a changing active-run count re-opened the episode", sb.LastErrorAt, tickRigNow)
	}
}

// A row that holds a classed refusal's whole text, as a scheduler that does
// not read the class records it, keeps that text and its last_error_at when
// a tick records the same class: the switch between the two texts is no new
// episode for the watch.
func TestTickerClassedRefusalKeepsWholeTextOfSameClass(t *testing.T) {
	const whole = "launch gate: concurrency_cap_exceeded: org has 3 active runs (cap 1)"
	sb := tickOnce(t, whole, fmt.Errorf("launch scheduled bot: %w", classedRefusal{active: 3}))
	if sb.LastError != whole {
		t.Fatalf("last_error = %q, want the same class's whole text kept (%q)", sb.LastError, whole)
	}
	if want := tickRigNow.Add(-time.Hour); sb.LastErrorAt == nil || !sb.LastErrorAt.Equal(want) {
		t.Fatalf("last_error_at = %v, want the recorded episode's %v: the class re-opened the episode", sb.LastErrorAt, want)
	}
}

// A refusal that names no class records its whole text whenever it changes,
// even when the recorded text extends the new one.
func TestTickerPlainRefusalPrefixOfRecordedTextWrites(t *testing.T) {
	sb := tickOnce(t, "x: y", errors.New("x"))
	if sb.LastError != "x" {
		t.Fatalf("last_error = %q, want the plain refusal's whole text %q", sb.LastError, "x")
	}
	if sb.LastErrorAt == nil || !sb.LastErrorAt.Equal(tickRigNow) {
		t.Fatalf("last_error_at = %v, want this tick's %v", sb.LastErrorAt, tickRigNow)
	}
}

// emptyClassRefusal names an empty class, which means no class.
type emptyClassRefusal struct{}

func (emptyClassRefusal) Error() string         { return "refused: a real refusal" }
func (emptyClassRefusal) LaunchVerdict() string { return "" }

// A refusal whose class is empty records its whole text: it never reads as
// a launch that clears the row.
func TestTickerEmptyClassRecordsWholeText(t *testing.T) {
	sb := tickOnce(t, "", emptyClassRefusal{})
	if sb.LastError != "refused: a real refusal" {
		t.Fatalf("last_error = %q, want the whole text of a refusal with an empty class", sb.LastError)
	}
	if sb.LastErrorAt == nil {
		t.Fatal("last_error_at = nil: the refusal reads as a launch")
	}
}

// An unreadable fence record is a verdict, but the armed fence that follows
// it, once the record reads again, supersedes it: the ticks the operator's
// fence holds clear the "could not be read" verdict instead of leaving it on
// the row for the whole maintenance window.
func TestTickerArmedFenceClearsUnreadableFenceVerdict(t *testing.T) {
	st, tick, launchErr := tickRig(t)
	*launchErr = fmt.Errorf("launch gate: maintenance_fence: read failed: %w", schedgate.ErrMaintenanceFenceUnreadable)
	tick(0)
	if sb := getRow(t, st); sb.LastError != schedgate.ErrMaintenanceFenceUnreadable.Error() {
		t.Fatalf("last_error after an unreadable fence = %q, want %q", sb.LastError, schedgate.ErrMaintenanceFenceUnreadable.Error())
	}
	*launchErr = fmt.Errorf("launch gate: maintenance_fence: armed: %w", schedgate.ErrMaintenanceFenceArmed)
	tick(1)
	if sb := getRow(t, st); sb.LastError != "" {
		t.Fatalf("last_error after an armed-fence tick = %q, want the unreadable-fence verdict cleared", sb.LastError)
	}
}

// A tick whose context ends during the launch records nothing: the replica
// is stopping, and the slot is held for whoever ticks next.
func TestTickerCancelledLaunchWritesNoLaunchVerdict(t *testing.T) {
	const prior = "launch gate: concurrency_cap_exceeded"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st := NewMemoryStore()
	if err := st.Create(ctx, ScheduledBot{ID: "sb-1", BotID: "b", Cron: "* * * * *", NextFireAt: tickRigNow.Add(-30 * time.Second)}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := st.MarkLaunchError(ctx, "sb-1", prior, tickRigNow.Add(-time.Hour)); err != nil {
		t.Fatalf("MarkLaunchError: %v", err)
	}
	launched := 0
	tk := &Ticker{Store: st, Now: func() time.Time { return tickRigNow },
		Launch: func(context.Context, ScheduledBot) error {
			launched++
			cancel()
			return errors.New("launch: context canceled")
		}}
	_, _ = tk.Tick(ctx) // the cancelled tick's own error is not under test
	if launched != 1 {
		t.Fatalf("launched %d time(s), want 1", launched)
	}
	if sb := getRow(t, st); sb.LastError != prior {
		t.Fatalf("last_error after a cancelled launch = %q, want it unchanged (%q)", sb.LastError, prior)
	}
}

var tickRigNow = time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)

// tickRig creates one schedule due every minute and returns its store, a
// tick(i) that ticks i minutes after tickRigNow, and the error every launch
// returns, which the test sets before each tick.
func tickRig(t *testing.T) (*MemoryStore, func(int), *error) {
	t.Helper()
	ctx := context.Background()
	st := NewMemoryStore()
	if err := st.Create(ctx, ScheduledBot{ID: "sb-1", BotID: "b", Cron: "* * * * *", NextFireAt: tickRigNow.Add(-30 * time.Second)}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	var launchErr error
	tick := func(i int) {
		t.Helper()
		now := tickRigNow.Add(time.Duration(i) * time.Minute)
		tk := &Ticker{Store: st, Now: func() time.Time { return now },
			Launch: func(context.Context, ScheduledBot) error { return launchErr }}
		if _, err := tk.Tick(ctx); err != nil {
			t.Fatalf("Tick %d: %v", i, err)
		}
	}
	return st, tick, &launchErr
}

func getRow(t *testing.T, st Store) ScheduledBot {
	t.Helper()
	sb, err := st.Get(context.Background(), "sb-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return sb
}

// tickOnce runs one Tick over a single due schedule whose launch returns
// lerr, after recording prior as its last_error when non-empty, and returns
// the stored row.
func tickOnce(t *testing.T, prior string, lerr error) ScheduledBot {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)
	st := NewMemoryStore()
	if err := st.Create(ctx, ScheduledBot{ID: "sb-1", BotID: "b", Cron: "* * * * *", NextFireAt: now.Add(-30 * time.Second)}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if prior != "" {
		if err := st.MarkLaunchError(ctx, "sb-1", prior, now.Add(-time.Hour)); err != nil {
			t.Fatalf("MarkLaunchError: %v", err)
		}
	}
	tk := &Ticker{Store: st, Now: func() time.Time { return now },
		Launch: func(context.Context, ScheduledBot) error { return lerr }}
	if _, err := tk.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	sb, err := st.Get(ctx, "sb-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return sb
}

// assertListEnabledContract is the shared check of ListEnabled, run against
// both store twins on a store holding no other schedule: every enabled row
// of every tenant, oldest next_fire_at first, ties on the id, disabled rows
// excluded.
func assertListEnabledContract(t *testing.T, st Store) {
	t.Helper()
	ctx := context.Background()
	base := time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)
	rows := []ScheduledBot{
		{ID: "le-late", TenantID: "t1", BotID: "b", Cron: "* * * * *", NextFireAt: base.Add(time.Hour)},
		{ID: "le-off", TenantID: "t1", BotID: "b", Cron: "* * * * *", NextFireAt: base.Add(-time.Hour), Disabled: true},
		{ID: "le-early-b", TenantID: "t2", BotID: "b", Cron: "* * * * *", NextFireAt: base},
		{ID: "le-early-a", TenantID: "t3", BotID: "b", Cron: "* * * * *", NextFireAt: base},
	}
	for _, sb := range rows {
		if err := st.Create(ctx, sb); err != nil {
			t.Fatalf("Create %s: %v", sb.ID, err)
		}
	}
	got, err := st.ListEnabled(ctx)
	if err != nil {
		t.Fatalf("ListEnabled: %v", err)
	}
	var ids []string
	for _, sb := range got {
		ids = append(ids, sb.ID)
	}
	if want := "le-early-a,le-early-b,le-late"; strings.Join(ids, ",") != want {
		t.Fatalf("ListEnabled = %v, want %s", ids, want)
	}
	for _, sb := range rows {
		if err := st.Delete(ctx, sb.ID); err != nil {
			t.Fatalf("Delete %s: %v", sb.ID, err)
		}
	}
}

func TestMemoryStoreListEnabled(t *testing.T) { assertListEnabledContract(t, NewMemoryStore()) }

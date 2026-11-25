package server

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/alert"
	"github.com/SocialGouv/iterion/pkg/cloudsched"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/platformcfg"
	"github.com/SocialGouv/iterion/pkg/schedgate"
	"github.com/SocialGouv/iterion/pkg/usernotify"
)

// watchReplica builds one server replica running the schedule-outcome
// watch against a shared schedule store, claim store and alert channel.
func watchReplica(st cloudsched.Store, claims usernotify.SentStore, sink alert.Sink) *Server {
	return &Server{
		cfg:    Config{ScheduledBots: st, PublicURL: "https://iterion.example"},
		logger: iterlog.Nop(),
		opsAlerts: &alert.OpsDispatcher{
			Claims: claims,
			Sinks:  []alert.Sink{sink},
			Logger: iterlog.Nop(),
		},
	}
}

func (s *advSink) snapshot() []alert.Alert {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]alert.Alert(nil), s.calls...)
}

func TestScheduleOutcomeWatch(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)
	grace := scheduleStallGraceFor(time.Minute)
	st := cloudsched.NewMemoryStore()
	sink := &advSink{}
	s := watchReplica(st, usernotify.NewMemSentStore(), sink)
	create := func(sb cloudsched.ScheduledBot) {
		t.Helper()
		sb.TenantID, sb.Cron = "t1", "0 7 * * *"
		if sb.NextFireAt.IsZero() {
			sb.NextFireAt = now.Add(22 * time.Hour)
		}
		if err := st.Create(ctx, sb); err != nil {
			t.Fatalf("Create %s: %v", sb.ID, err)
		}
	}
	sweep := func() []alert.Alert {
		s.scheduleOutcomeSweep(ctx, now, grace)
		return sink.snapshot()
	}

	create(cloudsched.ScheduledBot{ID: "sb-digest", BotID: "feed-watch"})
	create(cloudsched.ScheduledBot{ID: "sb-paused", BotID: "paused-bot", Disabled: true, NextFireAt: now.Add(-48 * time.Hour)})
	if got := sweep(); len(got) != 0 {
		t.Fatalf("healthy and disabled schedules alerted: %+v", got)
	}

	// A failed run: one alert, however many sweeps see it.
	if err := st.MarkRunOutcome(ctx, "sb-digest", "run-1", "failed", "provider 401", "AUTH_FAILED", now.Add(-time.Hour)); err != nil {
		t.Fatalf("MarkRunOutcome: %v", err)
	}
	sweep()
	got := sweep()
	if len(got) != 1 {
		t.Fatalf("a failed run seen by two sweeps delivered %d alerts, want 1: %+v", len(got), got)
	}
	a := got[0]
	if a.Kind != alert.KindScheduleStalled || a.RunID != "run-1" || a.RunName != "feed-watch" ||
		a.FailureCode != "AUTH_FAILED" || a.Link != "https://iterion.example/studio/runs/run-1" {
		t.Fatalf("alert = %+v, want schedule_stalled for run-1 of feed-watch with its code and link", a)
	}

	// The next run fails too: a new episode, one more alert.
	if err := st.MarkRunOutcome(ctx, "sb-digest", "run-2", "failed", "provider 401", "AUTH_FAILED", now.Add(-30*time.Minute)); err != nil {
		t.Fatalf("MarkRunOutcome: %v", err)
	}
	sweep()
	if got := sweep(); len(got) != 2 || got[1].RunID != "run-2" {
		t.Fatalf("a second failed run must re-alert once: %+v", got)
	}

	// Recovery: a finished run clears the row, nothing more is said.
	if err := st.MarkRunOutcome(ctx, "sb-digest", "run-3", "finished", "", "", now.Add(-10*time.Minute)); err != nil {
		t.Fatalf("MarkRunOutcome: %v", err)
	}
	if got := sweep(); len(got) != 2 {
		t.Fatalf("a recovered schedule kept alerting: %+v", got)
	}

	// A refused launch: no run exists, the row's last_error alone alerts.
	if err := st.MarkLaunchError(ctx, "sb-digest", "launch gate: concurrency_cap_exceeded", now.Add(-5*time.Minute)); err != nil {
		t.Fatalf("MarkLaunchError: %v", err)
	}
	sweep()
	got = sweep()
	if len(got) != 3 || got[2].RunID != "" || got[2].Link != "" {
		t.Fatalf("a refused launch must alert once, naming no run: %+v", got)
	}
	if err := st.MarkLaunchError(ctx, "sb-digest", "", now); err != nil {
		t.Fatalf("MarkLaunchError(clear): %v", err)
	}
	if got := sweep(); len(got) != 3 {
		t.Fatalf("a cleared refusal kept alerting: %+v", got)
	}

	// A slot no scheduler claimed: overdue alerts once.
	create(cloudsched.ScheduledBot{ID: "sb-stuck", BotID: "stuck-bot", NextFireAt: now.Add(-grace - time.Minute)})
	sweep()
	if got := sweep(); len(got) != 4 || got[3].RunName != "stuck-bot" {
		t.Fatalf("an overdue schedule must alert once: %+v", got)
	}
}

// Every replica sweeps; the shared claim delivers one alert per episode.
func TestScheduleOutcomeWatchReplicasDeliverOnce(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)
	st := cloudsched.NewMemoryStore()
	if err := st.Create(ctx, cloudsched.ScheduledBot{ID: "sb-1", TenantID: "t1", BotID: "b", Cron: "0 7 * * *",
		NextFireAt: now.Add(time.Hour), LastRunID: "run-1", LastRunStatus: "failed"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	claims := usernotify.NewMemSentStore()
	sink := &advSink{}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		r := watchReplica(st, claims, sink)
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.scheduleOutcomeSweep(ctx, now, scheduleStallGrace)
		}()
	}
	wg.Wait()
	if got := sink.snapshot(); len(got) != 1 {
		t.Fatalf("four replicas delivered %d alerts for one episode, want 1", len(got))
	}
}

// A delivery that fails on every channel is released, so the next sweep
// delivers it instead of the episode being silenced.
func TestScheduleOutcomeWatchRetriesAFailedDelivery(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)
	st := cloudsched.NewMemoryStore()
	if err := st.Create(ctx, cloudsched.ScheduledBot{ID: "sb-1", TenantID: "t1", BotID: "b", Cron: "0 7 * * *",
		NextFireAt: now.Add(time.Hour), LastError: "boom"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	sink := &advSink{fail: true}
	s := watchReplica(st, usernotify.NewMemSentStore(), sink)
	s.scheduleOutcomeSweep(ctx, now, scheduleStallGrace)
	sink.mu.Lock()
	sink.fail = false
	sink.mu.Unlock()
	s.scheduleOutcomeSweep(ctx, now, scheduleStallGrace)
	s.scheduleOutcomeSweep(ctx, now, scheduleStallGrace)
	if got := sink.snapshot(); len(got) != 2 {
		t.Fatalf("attempts = %d, want 2 (the failed one, then one delivery)", len(got))
	}
}

func TestScheduleStallGraceForFollowsALongTick(t *testing.T) {
	if got := scheduleStallGraceFor(time.Minute); got != scheduleStallGrace {
		t.Fatalf("grace for a 1m tick = %s, want %s", got, scheduleStallGrace)
	}
	if got := scheduleStallGraceFor(time.Hour); got != 3*time.Hour {
		t.Fatalf("grace for a 1h tick = %s, want 3h", got)
	}
}

// Resuming a schedule paused past the grace leaves its stale slot for the
// ticker to catch up; a sweep landing before that tick does not read it as
// a dead scheduler, and the real tick then launches it.
func TestScheduleOutcomeWatchResumeAfterLongPause(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)
	grace := scheduleStallGraceFor(time.Minute)
	st := cloudsched.NewMemoryStore()
	if err := st.Create(ctx, cloudsched.ScheduledBot{ID: "sb-paused", TenantID: "t1", BotID: "feed-watch", Cron: "0 7 * * *",
		NextFireAt: now.Add(-26 * time.Hour), Disabled: true}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	sink := &advSink{}
	s := watchReplica(st, usernotify.NewMemSentStore(), sink)
	enabled := false
	if _, err := st.Update(ctx, "sb-paused", cloudsched.SchedulePatch{Disabled: &enabled, UpdatedAt: now}); err != nil {
		t.Fatalf("Update(resume): %v", err)
	}
	s.scheduleOutcomeSweep(ctx, now.Add(5*time.Second), grace)
	if got := sink.snapshot(); len(got) != 0 {
		t.Fatalf("a schedule resumed 5s ago alerted before the ticker's next tick: %+v", got)
	}
	launched := 0
	tk := &cloudsched.Ticker{Store: st, Now: func() time.Time { return now.Add(30 * time.Second) },
		Launch: func(context.Context, cloudsched.ScheduledBot) error { launched++; return nil }}
	if _, err := tk.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if launched != 1 {
		t.Fatalf("the ticker launched %d run(s) for the resumed slot, want 1", launched)
	}
}

// An armed maintenance fence holds every due slot: a postponement the
// operator chose, not a stalled schedule.
func TestScheduleOutcomeWatchMaintenanceFenceIsSilent(t *testing.T) {
	fence := platformcfg.NewMemoryStore[platformcfg.Maintenance]()
	if err := fence.Put(context.Background(), platformcfg.Maintenance{FenceLaunches: true, Reason: "deploy v7.35"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got := sweepBehindMaintenance(t, fence); len(got) != 0 {
		t.Fatalf("an armed maintenance fence raised %d schedule_stalled alerts, want 0: %+v", len(got), got)
	}
}

// A maintenance store that cannot be read holds every due slot too, but no
// operator chose it and it lasts as long as the outage: each schedule raises
// one launch_refused alert.
func TestScheduleOutcomeWatchUnreadableFenceAlerts(t *testing.T) {
	got := sweepBehindMaintenance(t, brokenMaintenanceStore{})
	if len(got) != 3 {
		t.Fatalf("an unreadable maintenance fence raised %d schedule_stalled alerts, want 3: %+v", len(got), got)
	}
	for _, a := range got {
		if a.Kind != alert.KindScheduleStalled || !strings.Contains(a.Reason, "launches no run since") ||
			!strings.Contains(a.Reason, "maintenance.fence_launches could not be read") {
			t.Fatalf("alert = %+v, want a launch_refused schedule_stalled alert naming the unreadable fence", a)
		}
	}
}

// One unreadable fence read during an armed maintenance is superseded by the
// armed ticks that follow it: the row does not keep "could not be read" for
// the whole window, and a sweep after the next armed tick raises nothing.
func TestScheduleOutcomeWatchFenceReadBlipClearedByNextArmedTick(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)
	st := cloudsched.NewMemoryStore()
	if err := st.Create(ctx, cloudsched.ScheduledBot{ID: "sb-blip", TenantID: "t1", BotID: "feed-watch", Cron: "* * * * *",
		NextFireAt: now.Add(-30 * time.Second)}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	fence := &blipMaintenanceStore{failures: 1, rec: platformcfg.Maintenance{FenceLaunches: true, Reason: "schema rollout"}}
	sink := &advSink{}
	s := watchReplica(st, usernotify.NewMemSentStore(), sink)
	s.maintenanceStore = fence
	for i := 0; i < 10; i++ {
		at := now.Add(time.Duration(i) * time.Minute)
		tk := &cloudsched.Ticker{Store: st, Now: func() time.Time { return at },
			Launch: func(ctx context.Context, _ cloudsched.ScheduledBot) error {
				return s.gateMaintenance(ctx, launchSubject{}).err()
			}}
		if _, err := tk.Tick(ctx); err != nil {
			t.Fatalf("Tick %d: %v", i, err)
		}
	}
	if fence.reads() < 2 {
		t.Fatalf("the fence was read %d time(s), want the blip and at least one armed read", fence.reads())
	}
	s.scheduleOutcomeSweep(ctx, now.Add(10*time.Minute), scheduleStallGraceFor(time.Minute))
	if got := sink.snapshot(); len(got) != 0 {
		t.Fatalf("one fence-read blip under an armed fence raised %d alert(s), want 0: %+v", len(got), got)
	}
	row, err := st.Get(ctx, "sb-blip")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if row.LastError != "" || row.LastErrorAt != nil {
		t.Fatalf("an armed tick left the blip's verdict on the row: last_error=%q", row.LastError)
	}
}

// A sweep that reads the fence armed does not alert on an unreadable-fence
// verdict: the slot is held by the operator's fence either way. A sweep that
// cannot read the fence either still alerts, since the store may be down. An
// overdue row alerts whatever its verdict: no scheduler claims its slot.
func TestScheduleOutcomeWatchUnreadableVerdictUnderArmedFence(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)
	errAt := now.Add(-time.Minute)
	newRowWith := func(t *testing.T, lastError string) cloudsched.Store {
		st := cloudsched.NewMemoryStore()
		if err := st.Create(ctx, cloudsched.ScheduledBot{ID: "sb-fenced", TenantID: "t1", BotID: "feed-watch", Cron: "* * * * *",
			NextFireAt: now.Add(30 * time.Second), LastError: lastError, LastErrorAt: &errAt}); err != nil {
			t.Fatalf("Create: %v", err)
		}
		return st
	}
	newRow := func(t *testing.T) cloudsched.Store {
		return newRowWith(t, schedgate.ErrMaintenanceFenceUnreadable.Error())
	}
	armed := platformcfg.NewMemoryStore[platformcfg.Maintenance]()
	if err := armed.Put(ctx, platformcfg.Maintenance{FenceLaunches: true, Reason: "schema rollout"}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	sink := &advSink{}
	s := watchReplica(newRow(t), usernotify.NewMemSentStore(), sink)
	s.maintenanceStore = armed
	s.scheduleOutcomeSweep(ctx, now, scheduleStallGraceFor(time.Minute))
	if got := sink.snapshot(); len(got) != 0 {
		t.Fatalf("a sweep reading the fence armed alerted on an unreadable-fence verdict: %+v", got)
	}

	sink = &advSink{}
	s = watchReplica(newRow(t), usernotify.NewMemSentStore(), sink)
	s.maintenanceStore = brokenMaintenanceStore{}
	s.scheduleOutcomeSweep(ctx, now, scheduleStallGraceFor(time.Minute))
	if got := sink.snapshot(); len(got) != 1 {
		t.Fatalf("a sweep that cannot read the fence raised %d alert(s) on an unreadable-fence verdict, want 1: %+v", len(got), got)
	}

	sink = &advSink{}
	s = watchReplica(newRow(t), usernotify.NewMemSentStore(), sink)
	s.maintenanceStore = platformcfg.NewMemoryStore[platformcfg.Maintenance]()
	s.scheduleOutcomeSweep(ctx, now, scheduleStallGraceFor(time.Minute))
	if got := sink.snapshot(); len(got) != 1 {
		t.Fatalf("a sweep reading the fence disarmed raised %d alert(s) on an unreadable-fence verdict, want 1: %+v", len(got), got)
	}

	sink = &advSink{}
	s = watchReplica(newRowWith(t, "launch gate: concurrency_cap_exceeded"), usernotify.NewMemSentStore(), sink)
	s.maintenanceStore = armed
	s.scheduleOutcomeSweep(ctx, now, scheduleStallGraceFor(time.Minute))
	if got := sink.snapshot(); len(got) != 1 {
		t.Fatalf("a sweep reading the fence armed raised %d alert(s) on another refusal, want 1: %+v", len(got), got)
	}

	sink = &advSink{}
	overdue := cloudsched.NewMemoryStore()
	if err := overdue.Create(ctx, cloudsched.ScheduledBot{ID: "sb-fenced", TenantID: "t1", BotID: "feed-watch", Cron: "* * * * *",
		NextFireAt: now.Add(-3 * time.Hour), LastError: schedgate.ErrMaintenanceFenceUnreadable.Error(), LastErrorAt: &errAt}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	s = watchReplica(overdue, usernotify.NewMemSentStore(), sink)
	s.maintenanceStore = armed
	s.scheduleOutcomeSweep(ctx, now, scheduleStallGraceFor(time.Minute))
	if got := sink.snapshot(); len(got) != 1 || !strings.Contains(got[0].Reason, "is overdue") {
		t.Fatalf("a sweep reading the fence armed hid an overdue row behind an unreadable-fence verdict: %+v", got)
	}
}

// blipMaintenanceStore fails its first `failures` reads, then returns rec.
type blipMaintenanceStore struct {
	mu       sync.Mutex
	failures int
	n        int
	rec      platformcfg.Maintenance
}

func (b *blipMaintenanceStore) Get(context.Context) (*platformcfg.Maintenance, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.n++
	if b.n <= b.failures {
		return nil, errors.New("read timed out")
	}
	rec := b.rec
	return &rec, nil
}

func (b *blipMaintenanceStore) Put(context.Context, platformcfg.Maintenance) error {
	return errors.New("blipMaintenanceStore is read-only")
}

func (b *blipMaintenanceStore) reads() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.n
}

// sweepBehindMaintenance creates three due schedules, ticks them through the
// server's maintenance gate backed by store, and returns the alerts of one
// sweep a minute later.
func sweepBehindMaintenance(t *testing.T, store platformcfg.Store[platformcfg.Maintenance]) []alert.Alert {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)
	st := cloudsched.NewMemoryStore()
	for _, id := range []string{"sb-a", "sb-b", "sb-c"} {
		if err := st.Create(ctx, cloudsched.ScheduledBot{ID: id, TenantID: "t-" + id, BotID: "bot-" + id, Cron: "* * * * *",
			NextFireAt: now.Add(-30 * time.Second)}); err != nil {
			t.Fatalf("Create %s: %v", id, err)
		}
	}
	sink := &advSink{}
	s := watchReplica(st, usernotify.NewMemSentStore(), sink)
	s.maintenanceStore = store
	tk := &cloudsched.Ticker{Store: st, Now: func() time.Time { return now },
		Launch: func(ctx context.Context, _ cloudsched.ScheduledBot) error {
			return s.gateMaintenance(ctx, launchSubject{}).err()
		}}
	if _, err := tk.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	s.scheduleOutcomeSweep(ctx, now.Add(time.Minute), scheduleStallGraceFor(time.Minute))
	return sink.snapshot()
}

// Schedules stalled in the same way at the same instant share an episode;
// the schedule id in the claim key still delivers one alert each.
func TestScheduleOutcomeWatchSameEpisodeOnTwoSchedules(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 24, 7, 0, 0, 0, time.UTC)
	grace := scheduleStallGraceFor(time.Minute)

	t.Run("refused in one tick", func(t *testing.T) {
		st := cloudsched.NewMemoryStore()
		for _, id := range []string{"sb-x", "sb-y"} {
			if err := st.Create(ctx, cloudsched.ScheduledBot{ID: id, TenantID: "t1", BotID: "bot-" + id, Cron: "0 7 * * *",
				NextFireAt: now}); err != nil {
				t.Fatalf("Create %s: %v", id, err)
			}
		}
		tk := &cloudsched.Ticker{Store: st, Now: func() time.Time { return now },
			Launch: func(context.Context, cloudsched.ScheduledBot) error {
				return errors.New("launch gate: org_suspended: org cannot launch runs (suspended or read-only)")
			}}
		if _, err := tk.Tick(ctx); err != nil {
			t.Fatalf("Tick: %v", err)
		}
		sink := &advSink{}
		watchReplica(st, usernotify.NewMemSentStore(), sink).scheduleOutcomeSweep(ctx, now.Add(time.Minute), grace)
		if got := sink.snapshot(); len(got) != 2 {
			t.Fatalf("two schedules refused in one tick delivered %d alerts, want 2: %+v", len(got), got)
		}
	})

	t.Run("overdue on one slot", func(t *testing.T) {
		st := cloudsched.NewMemoryStore()
		for _, id := range []string{"sb-x", "sb-y"} {
			if err := st.Create(ctx, cloudsched.ScheduledBot{ID: id, TenantID: "t1", BotID: "bot-" + id, Cron: "0 7 * * *",
				NextFireAt: now.Add(-grace - time.Hour)}); err != nil {
				t.Fatalf("Create %s: %v", id, err)
			}
		}
		sink := &advSink{}
		watchReplica(st, usernotify.NewMemSentStore(), sink).scheduleOutcomeSweep(ctx, now, grace)
		if got := sink.snapshot(); len(got) != 2 {
			t.Fatalf("two schedules overdue on one slot delivered %d alerts, want 2: %+v", len(got), got)
		}
	})
}

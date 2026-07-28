package trigger

import (
	"context"
	"runtime/debug"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/bundle"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/schedgate"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Scheduler is the in-process timer source for schedule-kind subscriptions:
// it ticks each schedule subscription's Cron and fires the due ones via the
// Launcher. It is the local single-host counterpart to cloudsched (which owns
// the multi-replica CAS path for non-empty tenants) — it scopes itself to the
// local tenant "", so in cloud mode (where subscriptions carry real tenants)
// it is a no-op and cloudsched remains authoritative.
//
// A schedule subscription is its own launch target, so the scheduler fires it
// DIRECTLY via the launcher rather than publishing an event the matcher would
// re-resolve (which is the round-trip board/forge events need but schedules do
// not). Firing a finished scheduled run still emits a run-completion event, so
// chaining off a scheduled run works through the run source.
type Scheduler struct {
	subs     SubscriptionStore
	launcher Launcher
	logger   *iterlog.Logger
	interval time.Duration
	now      func() time.Time
	gate     *ScheduleGate

	mu       sync.Mutex
	nextFire map[string]time.Time // sub.ID -> next due instant (in-memory)
	// lastTickAt / lastSubsSeen back Status(): the observable proof the
	// scheduler loop is alive (a dead loop shows a frozen tick instant
	// instead of going silent).
	lastTickAt   time.Time
	lastSubsSeen int

	stop chan struct{}
	done chan struct{}
}

// SchedulerStatus is the read-only health snapshot behind
// /api/v1/triggers/health.
type SchedulerStatus struct {
	// LastTickAt is when tick() last ran (zero before the first tick).
	LastTickAt time.Time `json:"last_tick_at"`
	// Subscriptions is the schedule-kind subscription count seen on the
	// last tick.
	Subscriptions int `json:"subscriptions"`
	// Armed is how many subscriptions currently hold a next-fire slot.
	Armed int `json:"armed"`
	// IntervalSeconds is the tick cadence, so a reader can judge
	// staleness ("last tick 3 intervals ago = dead").
	IntervalSeconds float64 `json:"interval_seconds"`
}

// Status reports the scheduler's liveness snapshot. Safe on nil (zero
// value) so callers don't have to gate on wiring.
func (s *Scheduler) Status() SchedulerStatus {
	if s == nil {
		return SchedulerStatus{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return SchedulerStatus{
		LastTickAt:      s.lastTickAt,
		Subscriptions:   s.lastSubsSeen,
		Armed:           len(s.nextFire),
		IntervalSeconds: s.interval.Seconds(),
	}
}

// ScheduleGate bundles the overlap/guard dependencies (pkg/schedgate)
// the scheduler consults before firing a due subscription. Lister
// counts the schedule's live runs (nil disables the overlap check);
// Audit receives one TickRecord per decision (nil disables auditing).
// GuardDir is the working directory guards run in (the workspace the
// server was started from when empty).
type ScheduleGate struct {
	Lister   schedgate.ScheduleRunLister
	Audit    func(schedgate.TickRecord)
	GuardDir string
	// Reap cancels keepalive runs found stale at a tick (so the zombie
	// frees resources once a fresh run has relaunched). Nil is safe — the
	// stale run simply lingers but no longer blocks relaunch.
	Reap func(ctx context.Context, runIDs []string)
}

// SchedulerOption configures a Scheduler.
type SchedulerOption func(*Scheduler)

// WithSchedulerLogger sets the leveled logger (nil-safe).
func WithSchedulerLogger(l *iterlog.Logger) SchedulerOption {
	return func(s *Scheduler) { s.logger = l }
}

// WithSchedulerClock injects a clock for tests.
func WithSchedulerClock(fn func() time.Time) SchedulerOption {
	return func(s *Scheduler) { s.now = fn }
}

// WithSchedulerGate wires the overlap/guard gate (nil-safe: a nil gate
// keeps the pre-gate fire-always behavior).
func WithSchedulerGate(g *ScheduleGate) SchedulerOption {
	return func(s *Scheduler) { s.gate = g }
}

// WithSchedulerInterval overrides the loop tick resolution (default 1
// minute). Set it below a minute (e.g. 5s) so sub-minute keepalive
// subscriptions can actually fire at their cadence — the loop can only
// fire a subscription as often as it ticks. Values <= 0 are ignored.
func WithSchedulerInterval(d time.Duration) SchedulerOption {
	return func(s *Scheduler) {
		if d > 0 {
			s.interval = d
		}
	}
}

// NewScheduler builds a scheduler over a subscription store + launcher.
func NewScheduler(subs SubscriptionStore, launcher Launcher, opts ...SchedulerOption) *Scheduler {
	s := &Scheduler{
		subs:     subs,
		launcher: launcher,
		interval: time.Minute,
		now:      time.Now,
		nextFire: map[string]time.Time{},
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Start launches the ticking goroutine. No-op-safe to skip when launcher is
// nil (nothing to fire).
func (s *Scheduler) Start() {
	go s.loop()
}

// Stop halts the ticker (idempotent-safe for a single call).
func (s *Scheduler) Stop() {
	close(s.stop)
	<-s.done
}

func (s *Scheduler) loop() {
	defer close(s.done)
	// Cancel the tick context when Stop fires so a tick blocked in the
	// store list or a launch (both context-aware) unwinds instead of
	// pinning the loop — otherwise Stop's <-s.done would hang on it.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-s.stop
		cancel()
	}()
	tk := time.NewTicker(s.interval)
	defer tk.Stop()
	for {
		s.tick(ctx, s.now())
		select {
		case <-s.stop:
			return
		case <-tk.C:
		}
	}
}

// tick fires every schedule subscription whose next-fire instant has passed.
// First sighting of a subscription only arms it (computes the next fire from
// now) — it does not fire immediately, so adding a schedule never back-fires.
func (s *Scheduler) tick(ctx context.Context, now time.Time) {
	if s.launcher == nil {
		return
	}
	subs, err := s.subs.ListByTenant(ctx, "")
	if err != nil {
		s.warn("scheduler: list subscriptions: %v", err)
		return
	}
	scheduleSubs := 0
	seen := map[string]bool{}
	for _, sub := range subs {
		if !sub.Enabled {
			continue
		}
		// The subscription owns its cadence (cron or keepalive interval) —
		// see Subscription.NextFire. ok=false = not a timer sub; err = bad cron.
		next, ok, err := sub.NextFire(now)
		if !ok {
			continue
		}
		if err != nil {
			s.warn("scheduler: bad cron %q on %s: %v", sub.Cron, sub.ID, err)
			continue
		}
		scheduleSubs++
		seen[sub.ID] = true

		s.mu.Lock()
		nf, armed := s.nextFire[sub.ID]
		if !armed {
			s.nextFire[sub.ID] = next
			s.mu.Unlock()
			continue
		}
		if now.Before(nf) {
			s.mu.Unlock()
			continue
		}
		s.nextFire[sub.ID] = next
		s.mu.Unlock()

		s.fireIsolated(ctx, sub)
	}
	s.mu.Lock()
	s.lastTickAt = now
	s.lastSubsSeen = scheduleSubs
	s.mu.Unlock()
	// GC next-fire entries for removed/disabled subscriptions.
	s.mu.Lock()
	for id := range s.nextFire {
		if !seen[id] {
			delete(s.nextFire, id)
		}
	}
	s.mu.Unlock()
}

// fireIsolated contains a panic from one subscription's fire (a bad
// guard closure, a launcher bug) so one poisoned entry cannot kill the
// whole scheduler loop — the next tick still serves every other
// subscription. The panic is logged with its stack, never swallowed
// silently.
func (s *Scheduler) fireIsolated(ctx context.Context, sub Subscription) {
	defer func() {
		if r := recover(); r != nil {
			s.warn("scheduler: PANIC firing %s (%s): %v\n%s", sub.ID, sub.BotID, r, debug.Stack())
		}
	}()
	s.fire(ctx, sub)
}

func (s *Scheduler) fire(ctx context.Context, sub Subscription) {
	vars := make(map[string]string, len(sub.Vars))
	for k, v := range sub.Vars {
		vars[k] = v
	}

	// Overlap + guard gate (pkg/schedgate). A skipped tick is a normal,
	// audited outcome — the cron slot simply passes.
	policy := sub.Policy()
	var lister schedgate.ScheduleRunLister
	if s.gate != nil {
		lister = s.gate.Lister
	}
	out := schedgate.Apply(ctx, schedgate.GateInput{
		Policy:     policy,
		Lister:     lister,
		ScheduleID: sub.ID,
		Record:     s.tickRecord(sub, ""),
		GuardDir:   s.guardDir(),
		GuardEnv: []string{
			"ITERION_SCHEDULE=" + sub.ID,
			"ITERION_SCHEDULE_BOT=" + sub.BotID,
		},
		Logger: s.logger,
		Now:    s.now(),
	})
	if !out.Proceed {
		s.audit(out.Record)
		switch out.Record.Decision {
		case schedgate.TickSkippedOverlap:
			s.warn("scheduler: %s (%s) skipped — %s", sub.ID, sub.BotID, out.Record.Reason)
		case schedgate.TickGuardError:
			s.warn("scheduler: %s (%s) guard error: %s", sub.ID, sub.BotID, out.Record.Error)
		}
		return
	}
	if out.GuardRan {
		vars[policy.GuardVar] = out.GuardStdout
	}
	// Reap keepalive zombies (stale runs the gate dropped so this fresh
	// launch could proceed) so they free resources.
	if len(out.ReapRunIDs) > 0 {
		s.warn("scheduler: %s (%s) reaping %d stale keepalive run(s): %v", sub.ID, sub.BotID, len(out.ReapRunIDs), out.ReapRunIDs)
		if s.gate != nil && s.gate.Reap != nil {
			s.gate.Reap(ctx, out.ReapRunIDs)
		}
	}

	plan := LaunchPlan{
		BotID:           sub.BotID,
		TenantID:        sub.TenantID,
		Repo:            sub.Repo,
		Mode:            bundle.ExecutionDirect,
		Vars:            vars,
		KeyOverrides:    sub.KeyOverrides,
		SecretOverrides: sub.SecretOverrides,
		Retry:           sub.RetryPolicy(),
		Event: Event{
			ID:         "schedule:" + sub.ID,
			Source:     SourceSchedule,
			Kind:       "cron",
			TenantID:   sub.TenantID,
			Repo:       sub.Repo,
			Subject:    Subject{Type: "schedule", ID: sub.ID},
			OccurredAt: s.now(),
		},
		// Typed provenance: the overlap gate counts this schedule's live
		// runs through source.schedule_id on the launched run.
		SourceRef: &store.RunSource{
			Kind:         store.RunSourceKindSchedule,
			ScheduleID:   sub.ID,
			ScheduleName: sub.BotID,
		},
	}
	runID, err := s.launcher.Launch(ctx, plan)
	rec := s.tickRecord(sub, schedgate.TickFired)
	rec.RunID = runID
	if err != nil {
		rec.Error = err.Error()
		s.warn("scheduler: launch %s (%s): %v", sub.ID, sub.BotID, err)
	}
	s.audit(rec)
}

// tickRecord stamps the invariant trigger-surface audit fields.
func (s *Scheduler) tickRecord(sub Subscription, decision schedgate.TickDecision) schedgate.TickRecord {
	rec := schedgate.NewTickRecord(schedgate.SurfaceTrigger, sub.ID, s.now(), decision)
	rec.ScheduleName = sub.BotID
	rec.BotID = sub.BotID
	rec.TenantID = sub.TenantID
	rec.Cron = sub.Cron
	return rec
}

func (s *Scheduler) audit(rec schedgate.TickRecord) {
	if s.gate != nil && s.gate.Audit != nil {
		s.gate.Audit(rec)
	}
}

func (s *Scheduler) guardDir() string {
	if s.gate != nil {
		return s.gate.GuardDir
	}
	return ""
}

func (s *Scheduler) warn(format string, args ...any) {
	if s.logger != nil {
		s.logger.Warn(format, args...)
	}
}

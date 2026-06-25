package trigger

import (
	"context"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/SocialGouv/iterion/pkg/bundle"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
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

	mu       sync.Mutex
	nextFire map[string]time.Time // sub.ID -> next due instant (in-memory)

	stop chan struct{}
	done chan struct{}
}

// SchedulerOption configures a Scheduler.
type SchedulerOption func(*Scheduler)

// WithSchedulerInterval sets the tick cadence (default 1 minute — cron's
// finest granularity).
func WithSchedulerInterval(d time.Duration) SchedulerOption {
	return func(s *Scheduler) { s.interval = d }
}

// WithSchedulerLogger sets the leveled logger (nil-safe).
func WithSchedulerLogger(l *iterlog.Logger) SchedulerOption {
	return func(s *Scheduler) { s.logger = l }
}

// WithSchedulerClock injects a clock for tests.
func WithSchedulerClock(fn func() time.Time) SchedulerOption {
	return func(s *Scheduler) { s.now = fn }
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
	tk := time.NewTicker(s.interval)
	defer tk.Stop()
	for {
		s.tick(context.Background(), s.now())
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
	seen := map[string]bool{}
	for _, sub := range subs {
		if sub.Invocation != bundle.InvocationKindSchedule || !sub.Enabled || sub.Cron == "" {
			continue
		}
		sched, perr := cron.ParseStandard(sub.Cron)
		if perr != nil {
			s.warn("scheduler: bad cron %q on %s: %v", sub.Cron, sub.ID, perr)
			continue
		}
		seen[sub.ID] = true

		s.mu.Lock()
		nf, armed := s.nextFire[sub.ID]
		if !armed {
			s.nextFire[sub.ID] = sched.Next(now)
			s.mu.Unlock()
			continue
		}
		if now.Before(nf) {
			s.mu.Unlock()
			continue
		}
		s.nextFire[sub.ID] = sched.Next(now)
		s.mu.Unlock()

		s.fire(ctx, sub)
	}
	// GC next-fire entries for removed/disabled subscriptions.
	s.mu.Lock()
	for id := range s.nextFire {
		if !seen[id] {
			delete(s.nextFire, id)
		}
	}
	s.mu.Unlock()
}

func (s *Scheduler) fire(ctx context.Context, sub Subscription) {
	vars := make(map[string]string, len(sub.Vars))
	for k, v := range sub.Vars {
		vars[k] = v
	}
	plan := LaunchPlan{
		BotID:           sub.BotID,
		TenantID:        sub.TenantID,
		Repo:            sub.Repo,
		Mode:            bundle.ExecutionDirect,
		Vars:            vars,
		KeyOverrides:    sub.KeyOverrides,
		SecretOverrides: sub.SecretOverrides,
		Event: Event{
			ID:         "schedule:" + sub.ID,
			Source:     SourceSchedule,
			Kind:       "cron",
			TenantID:   sub.TenantID,
			Repo:       sub.Repo,
			Subject:    Subject{Type: "schedule", ID: sub.ID},
			OccurredAt: s.now(),
		},
	}
	if _, err := s.launcher.Launch(ctx, plan); err != nil {
		s.warn("scheduler: launch %s (%s): %v", sub.ID, sub.BotID, err)
	}
}

func (s *Scheduler) warn(format string, args ...any) {
	if s.logger != nil {
		s.logger.Warn(format, args...)
	}
}

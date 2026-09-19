package server

import (
	"context"
	"os"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/eventbus"
	"github.com/SocialGouv/iterion/pkg/internal/jsonl"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/schedgate"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// defaultSchedulerTickInterval is the in-process scheduler's tick
// resolution. It is well below a minute so sub-minute keepalive
// subscriptions fire near their cadence out of the box; a cron
// subscription is unaffected (its next-fire is minute-aligned, so a
// faster tick never over-fires it). Override with ITERION_SCHEDULER_INTERVAL
// (a Go duration).
const defaultSchedulerTickInterval = 15 * time.Second

func schedulerTickInterval() time.Duration {
	if v := os.Getenv("ITERION_SCHEDULER_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return defaultSchedulerTickInterval
}

// TriggerCoordinator wires the event-driven trigger spine for the native
// board: an in-process eventbus, a board source tailing the shared
// events.jsonl, and an evaluator that matches each transition against the
// stored subscriptions and promotes matching cards. It is the local/self-host
// counterpart to the cloud NATS bus; the same trigger.Evaluator drives both.
//
// It sits alongside watchCoordinator (which fans state transitions to
// subscribed RUNS); this one fans them to trigger SUBSCRIPTIONS that LAUNCH
// bots. Both are best-effort enhancements over the dispatcher poll — a host
// without fsnotify simply falls back to the 30s poll.
type TriggerCoordinator struct {
	bus       eventbus.Bus
	source    *trigger.BoardSource
	scheduler *trigger.Scheduler
	cancelSub func(context.Context)
	logger    *iterlog.Logger
}

// StartTriggerCoordinator builds the spine and begins tailing. Returns nil
// (a no-op) when a prerequisite is missing or the events tail can't start —
// the dispatcher poll remains the backstop. nudger (the dispatcher Manager)
// and launcher (direct-mode runs) may be nil; a nil nudger just means a
// promoted card waits for the next poll instead of being dispatched now.
// bus, when non-nil, is the process-wide event spine to ride (the cloud
// NATSBus) so every run-outcome consumer — evaluator, usernotify — sees one
// stream; nil builds the local InProcBus.
func StartTriggerCoordinator(ns *native.Store, subs trigger.SubscriptionStore, nudger trigger.Nudger, launcher trigger.Launcher, gate *trigger.ScheduleGate, bus eventbus.Bus, logger *iterlog.Logger) *TriggerCoordinator {
	if ns == nil || subs == nil {
		return nil
	}
	if bus == nil {
		bus = eventbus.NewInProcBus(logger)
	}
	effect := trigger.NewNativeBoardEffect(ns, nudger, logger)
	eval := trigger.NewEvaluator(subs,
		trigger.WithBoardEffect(effect),
		trigger.WithLauncher(launcher),
		trigger.WithLogger(logger),
	)
	cancelSub, err := bus.Subscribe("trigger-evaluator", trigger.Matcher{}, eval.Handle)
	if err != nil {
		if logger != nil {
			logger.Warn("server: trigger spine disabled (bus subscribe failed): %v", err)
		}
		return nil
	}
	src := trigger.StartBoardSource(ns, bus, logger)
	if src == nil {
		// Boot-time abort — no shared shutdown ctx yet; the default budget
		// applies internally.
		cancelSub(context.Background())
		return nil
	}
	tc := &TriggerCoordinator{bus: bus, source: src, cancelSub: cancelSub, logger: logger}
	// Schedule source: fire schedule-kind subscriptions on their cron. Only
	// useful when a launcher is wired (something to launch); scoped to the
	// local tenant "" so it's a no-op in cloud mode (cloudsched owns that).
	if launcher != nil {
		tc.scheduler = trigger.NewScheduler(subs, launcher,
			trigger.WithSchedulerLogger(logger),
			trigger.WithSchedulerGate(gate),
			trigger.WithSchedulerInterval(schedulerTickInterval()),
		)
		tc.scheduler.Start()
	}
	return tc
}

// scheduleGate wires the trigger scheduler's overlap/guard gate onto
// the server's run store and the local tick-audit JSONL (the same file
// `iterion schedule audit` reads). Audit appends are best-effort: a
// failed append is logged, never turns a tick decision into a failure.
func (s *Server) scheduleGate() *trigger.ScheduleGate {
	// The overlap/staleness gate needs the run store. runview owns it in
	// both modes — RunStore() returns the injected cfg.Store in cloud and
	// the storeDir-built store locally — so it is the single canonical
	// accessor every run-touching call site in this package uses. (Reaching
	// for cfg.Store directly misses the local case, where it is nil.)
	if s.runs == nil {
		return nil
	}
	rs := s.runs.RunStore()
	if rs == nil {
		return nil
	}
	gate := &trigger.ScheduleGate{Lister: rs, GuardDir: s.cfg.WorkDir}
	// Reap keepalive zombies: a stale run the gate dropped is CAS-flipped
	// running→failed_resumable so it stops counting as live and frees its
	// resources (resumable so an operator can still inspect/continue it).
	gate.Reap = func(ctx context.Context, ids []string) {
		for _, id := range ids {
			changed, rerr := rs.UpdateRunStatusIf(ctx, id, store.RunStatusFailedResumable,
				"keepalive: run silent past stale_after — reaped so a fresh run could relaunch",
				[]store.RunStatus{store.RunStatusRunning})
			if rerr != nil {
				s.logger.Warn("server: keepalive reap %s failed: %v", id, rerr)
			} else if changed {
				s.logger.Info("server: keepalive reaped stale run %s", id)
			}
		}
	}
	auditPath, err := schedgate.DefaultLocalAuditPath()
	if err != nil {
		s.logger.Warn("server: trigger tick audit disabled: %v", err)
		return gate
	}
	gate.Audit = func(rec schedgate.TickRecord) {
		if aerr := jsonl.AppendJSON(auditPath, rec); aerr != nil {
			s.logger.Warn("server: tick audit append failed (%s): %v", auditPath, aerr)
		}
	}
	return gate
}

// SchedulerStatus reports the schedule-scheduler's liveness snapshot.
// Nil-safe (zero value when the coordinator or scheduler is absent).
func (t *TriggerCoordinator) SchedulerStatus() trigger.SchedulerStatus {
	if t == nil {
		return trigger.SchedulerStatus{}
	}
	return t.scheduler.Status()
}

// SchedulerRunning reports whether a schedule scheduler is wired at
// all (false in dispatch-only mode where launcher is nil). Nil-safe.
func (t *TriggerCoordinator) SchedulerRunning() bool {
	return t != nil && t.scheduler != nil
}

// Bus returns the coordinator's event bus so other producers (the run
// service's run-completion source) publish onto the same bus the evaluator
// consumes. The concrete type is InProcBus for local/self-host and NATSBus in
// cloud multi-replica; callers only Publish, so they take the interface.
// Returns nil for a nil coordinator.
func (t *TriggerCoordinator) Bus() eventbus.Bus {
	if t == nil {
		return nil
	}
	return t.bus
}

// StopSource drains the non-bus halves — the schedule Scheduler and the
// board Source — WITHOUT touching the bus subscription. Under a graceful
// shutdown the caller drives these drains BEFORE opening the shared
// subCancelCtx, so an unbounded ctx-less Stop cannot eat the bus-cancel
// budget the arithmetic upstream depends on (#1477 R503821).
func (t *TriggerCoordinator) StopSource() {
	if t == nil {
		return
	}
	if t.scheduler != nil {
		t.scheduler.Stop()
	}
	if t.source != nil {
		t.source.Stop()
	}
}

// CancelSub unsubscribes the evaluator from the bus. The ctx bounds the
// bus-subscription cancel's in-flight wait; under a shared shutdown budget
// the caller passes a joinCtx so this cancel composes with the peer
// subscribers' cancels — see eventbus.Bus.Subscribe's ctx contract.
func (t *TriggerCoordinator) CancelSub(ctx context.Context) {
	if t == nil {
		return
	}
	if t.cancelSub != nil {
		t.cancelSub(ctx)
	}
}

// Close is the convenience wrapper for callers outside of Server.Shutdown
// (dispatch.go's defer, tests). It sequences the two halves the way a
// standalone caller expects: drain the source, THEN unsubscribe. Server
// shutdown uses StopSource + CancelSub separately so it can open the
// subCancelCtx between the two.
func (t *TriggerCoordinator) Close(ctx context.Context) {
	t.StopSource()
	t.CancelSub(ctx)
}

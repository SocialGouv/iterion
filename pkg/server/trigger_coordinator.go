package server

import (
	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/eventbus"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

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
	bus       *eventbus.InProcBus
	source    *trigger.BoardSource
	scheduler *trigger.Scheduler
	cancelSub func()
	logger    *iterlog.Logger
}

// StartTriggerCoordinator builds the spine and begins tailing. Returns nil
// (a no-op) when a prerequisite is missing or the events tail can't start —
// the dispatcher poll remains the backstop. nudger (the dispatcher Manager)
// and launcher (direct-mode runs) may be nil; a nil nudger just means a
// promoted card waits for the next poll instead of being dispatched now.
func StartTriggerCoordinator(ns *native.Store, subs trigger.SubscriptionStore, nudger trigger.Nudger, launcher trigger.Launcher, logger *iterlog.Logger) *TriggerCoordinator {
	if ns == nil || subs == nil {
		return nil
	}
	bus := eventbus.NewInProcBus(logger)
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
		cancelSub()
		return nil
	}
	tc := &TriggerCoordinator{bus: bus, source: src, cancelSub: cancelSub, logger: logger}
	// Schedule source: fire schedule-kind subscriptions on their cron. Only
	// useful when a launcher is wired (something to launch); scoped to the
	// local tenant "" so it's a no-op in cloud mode (cloudsched owns that).
	if launcher != nil {
		tc.scheduler = trigger.NewScheduler(subs, launcher, trigger.WithSchedulerLogger(logger))
		tc.scheduler.Start()
	}
	return tc
}

// Bus returns the coordinator's event bus so other producers (the run
// service's run-completion source) publish onto the same bus the evaluator
// consumes. Returns nil for a nil coordinator.
func (t *TriggerCoordinator) Bus() *eventbus.InProcBus {
	if t == nil {
		return nil
	}
	return t.bus
}

// Close tears down the board source and unsubscribes the evaluator.
func (t *TriggerCoordinator) Close() {
	if t == nil {
		return
	}
	if t.scheduler != nil {
		t.scheduler.Stop()
	}
	if t.source != nil {
		t.source.Stop()
	}
	if t.cancelSub != nil {
		t.cancelSub()
	}
}

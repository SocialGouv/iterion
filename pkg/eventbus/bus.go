// Package eventbus is the internal publish/subscribe spine that carries
// trigger.Event values from producers (native board, run completion, forge
// webhooks, schedule ticks, custom ingress) to consumers (the trigger
// Evaluator). It has two interchangeable implementations selected at wiring
// time — InProcBus for local single-host (CLI/studio) and NATSBus for
// cloud multi-tenant fan-out — so the same trigger.Evaluator consumes events
// identically in both modes.
//
// The bus is a fan-out NOTIFICATION channel, deliberately separate from the
// run WORK queue (pkg/queue, iterion.queue.runs): events are at-least-once
// and lossy under back-pressure, runs are exactly-once and locked. They have
// different delivery semantics, so they get different transports.
package eventbus

import (
	"context"
	"fmt"
	"runtime/debug"

	"github.com/SocialGouv/iterion/pkg/trigger"
)

// Handler processes one event. It runs on a per-subscriber worker goroutine,
// so it may do store I/O without stalling the publisher. A returned error is
// logged and otherwise ignored — the bus does not retry (the producer's own
// reconciliation path, e.g. the dispatcher poll, is the safety net).
type Handler func(ctx context.Context, ev trigger.Event) error

// Bus is the publish/subscribe contract. Publish never blocks on a slow
// subscriber (lossy fan-out); Subscribe registers a durable-named handler
// pre-filtered by a Matcher and returns a cancel func.
type Bus interface {
	Publish(ctx context.Context, ev trigger.Event) error
	// Subscribe delivers events matching filter to h. name identifies the
	// subscriber (used as the durable consumer name by NATSBus; informational
	// for InProcBus). An empty Matcher matches every event.
	Subscribe(name string, filter trigger.Matcher, h Handler) (cancel func(), err error)
}

// deliver runs one handler and converts a panic into an error, so a defect in
// ONE subscriber cannot take the process down with every other subscriber and
// the HTTP surface. The bus is a fan-out to independent consumers — the
// trigger evaluator, notifications, the gate reconciler, the gate auto-fix —
// which share nothing but this dispatch; a crash here is the widest possible
// blast radius for the narrowest possible bug.
//
// It is loud, not silent: the panic surfaces as an error on the event that
// caused it, with the stack, and that delivery is dropped exactly as a
// returned error already is. Recovering does not paper over the defect — it
// scopes it to the one event it belongs to.
func deliver(ctx context.Context, h Handler, ev trigger.Event) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("handler panicked: %v\n%s", r, debug.Stack())
		}
	}()
	return h(ctx, ev)
}

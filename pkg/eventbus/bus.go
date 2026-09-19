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
	"time"

	"github.com/SocialGouv/iterion/pkg/trigger"
)

// DefaultSubscribeCancelBudget bounds how long the cancel returned by
// Subscribe waits for an in-flight handler when the caller passes a context
// without a deadline. Under a graceful shutdown the caller SHOULD thread a
// shared joinCtx so all N subscriptions cancel under ONE budget (mirroring
// pkg/server.joinBackgroundWorkers' single joinCtx and its per-loop-budget
// arbitration in #1257); the default is a safety net for callers that
// cancel a single subscription outside of a shutdown.
const DefaultSubscribeCancelBudget = 500 * time.Millisecond

// overrunPostCheckWindow is a small grace period the cancel path gives
// the worker after the budget expired: enough for a scheduler wake so a
// no-in-flight worker closes its done channel, small enough that N
// subscribers under one shared budget do not eat the budget by
// composing the grace. Without it, a same-tick coin-flip between
// waitCtx.Done() and s.done names a clean subscriber as an overrun
// ~half the time (#1477 R6aac0e).
//
// 20 ms is calibrated between two constraints identified by the
// adversarial rounds:
//
//   - Go's async-preemption slice is 10 ms, and a saturated shared
//     runner can delay a worker's `defer close(s.done)` past a shorter
//     window (round 3 reproduced the miss at 2 ms on a 32-core box
//     under 32×`yes`).
//   - The shared shutdown budget covers N cancels together, so each
//     grace period composes into the total wall-clock. At N = 10 the
//     added cost is 200 ms — significant next to a 500 ms budget but
//     recoverable within the chart's 60 s terminationGracePeriodSeconds.
//
// 20 ms is well above the 10 ms preemption slice and leaves the
// aggregate cost bounded.
const overrunPostCheckWindow = 20 * time.Millisecond

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
	//
	// The returned cancel signals the handler's context, unsubscribes the
	// transport, and waits for an in-flight delivery within the caller's
	// ctx.Deadline (DefaultSubscribeCancelBudget when ctx has none). A
	// handler that ignores its context is named in a warning and left
	// behind rather than waited out, so a defective subscriber cannot hold
	// the process past the grace period. Cancel does NOT guarantee
	// delivery of buffered events — the bus is a lossy fan-out and its
	// producer's reconciliation path is the safety net (see the package
	// doc; every subscriber pkg/server wires has a matching sweep, so
	// unwinding an in-flight write is sufficient).
	//
	// Under a graceful shutdown the caller SHOULD thread ONE joinCtx across
	// all cancels so N subscriptions share the same deadline; passing them
	// separate contexts (or the same context sequentially with a per-call
	// timer) composes the budget and eats the grace period upstream. That
	// mirrors pkg/server.joinBackgroundWorkers' single joinCtx arbitration.
	// Idempotent.
	Subscribe(name string, filter trigger.Matcher, h Handler) (cancel func(ctx context.Context), err error)
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

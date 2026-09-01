// Package alert implements run-health alerting for the iterion studio /
// server. It observes the run event stream, detects four conditions —
// node stall (>stallTimeout with no activity), any budget axis crossing
// the warning threshold, budget exceeded, and run failure — and fans the
// resulting Alert out to a set of sinks (generic incoming webhook,
// in-process browser/desktop delivery).
//
// Detection is intentionally decoupled from the runtime: budget and
// failure triggers ride the existing runtime events (budget_warning,
// budget_exceeded, run_failed), while stall is timer-driven off a
// per-run liveness heartbeat advanced by *every* observed event
// (including tool events), mirroring the dispatcher's heartbeat pattern.
package alert

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Kind enumerates the alert conditions.
type Kind string

const (
	// KindStall fires when a non-terminal run sees no activity for
	// longer than the configured stall timeout.
	KindStall Kind = "stall"
	// KindStallRecovered fires once when a previously-stalled run shows
	// activity again — the closing bracket of a stall episode, so the
	// persisted timeline shows both edges.
	KindStallRecovered Kind = "stall_recovered"
	// KindBudgetWarning fires the first time a budget axis crosses the
	// runtime warning threshold (80%) or an advisory warn threshold
	// (e.g. warn_tokens), which reports without ever blocking.
	KindBudgetWarning Kind = "budget_warning"
	// KindBudgetExceeded fires when a budget axis hits its hard cap.
	KindBudgetExceeded Kind = "budget_exceeded"
	// KindRunFailed fires when a run transitions to a failed status.
	KindRunFailed Kind = "run_failed"
	// KindRunParked fires when a run parks failed_resumable — waiting out a
	// provider/usage window (an armed retry, with an ETA in the reason) or
	// waiting for an operator resume (no retry armed). The 2026-08 pattern
	// this closes: five digests parked on a usage cap over a whole morning
	// with nobody told.
	KindRunParked Kind = "run_parked"
)

// Alert is the structured payload delivered to every sink. It carries
// the run name, a deep link to /runs/<id>, the current node, a
// human-readable reason, and the budget axis + percentage consumed when
// the trigger is budget-related.
type Alert struct {
	Kind    Kind   `json:"kind"`
	RunID   string `json:"run_id"`
	RunName string `json:"run_name,omitempty"`
	NodeID  string `json:"node_id,omitempty"`
	Reason  string `json:"reason,omitempty"`
	// FailureCode is the run's persisted typed classification (ADR-095)
	// when one exists — a machine consumer reads it here, never by
	// parsing Reason.
	FailureCode string    `json:"failure_code,omitempty"`
	Axis        string    `json:"axis,omitempty"`       // budget dimension (tokens/cost_usd/...)
	BudgetPct   float64   `json:"budget_pct,omitempty"` // 0..100, budget alerts only
	Link        string    `json:"link,omitempty"`       // <baseURL>/runs/<id>
	Timestamp   time.Time `json:"timestamp"`
}

// Title is a short one-line headline suitable for a toast or
// notification title.
func (a Alert) Title() string {
	name := a.RunName
	if name == "" {
		name = a.RunID
	}
	switch a.Kind {
	case KindStall:
		return fmt.Sprintf("Run stalled: %s", name)
	case KindStallRecovered:
		return fmt.Sprintf("Run recovered: %s", name)
	case KindBudgetWarning:
		return fmt.Sprintf("Budget warning: %s", name)
	case KindBudgetExceeded:
		return fmt.Sprintf("Budget exceeded: %s", name)
	case KindRunFailed:
		return fmt.Sprintf("Run failed: %s", name)
	case KindRunParked:
		return fmt.Sprintf("Run parked: %s", name)
	default:
		return fmt.Sprintf("Run alert: %s", name)
	}
}

// WebhookText renders the Slack/Discord-compatible plain-text body. Both
// platforms accept a top-level {"text": ...} payload, so a single
// multi-line string covers the generic incoming-webhook contract.
func (a Alert) WebhookText() string {
	var b strings.Builder
	b.WriteString(a.Title())
	if a.NodeID != "" {
		fmt.Fprintf(&b, "\nNode: %s", a.NodeID)
	}
	if a.Reason != "" {
		fmt.Fprintf(&b, "\nReason: %s", a.Reason)
		if a.FailureCode != "" {
			fmt.Fprintf(&b, " [%s]", a.FailureCode)
		}
	} else if a.FailureCode != "" {
		// No prose to annotate — the code gets its own line rather
		// than visually qualifying the node or the run name above.
		fmt.Fprintf(&b, "\nCode: %s", a.FailureCode)
	}
	// Only when there IS a ratio. A dimension whose budget_warning payload
	// carries no used/limit pair (cost_usd_unpriced: tokens burned at an
	// unknown price, against a dollar ceiling) arrives here with BudgetPct
	// 0, and "Budget: cost_usd_unpriced at 0%" reads as "0% of the cost
	// budget used" — the opposite of what the alert says. The axis name
	// still travels on the event data and to errtrack; it is the percentage
	// that is meaningless, so it is the percentage that is dropped.
	if a.Axis != "" && a.BudgetPct > 0 {
		fmt.Fprintf(&b, "\nBudget: %s at %.0f%%", a.Axis, a.BudgetPct)
	}
	if a.Link != "" {
		fmt.Fprintf(&b, "\n%s", a.Link)
	}
	return b.String()
}

// AsEventData renders the alert as a flat map for the in-process `alert`
// store event (the browser delivery path). Keys mirror the JSON tags so
// the SPA can read them directly off RunEvent.data.
func (a Alert) AsEventData() map[string]any {
	d := map[string]any{
		"kind":      string(a.Kind),
		"run_id":    a.RunID,
		"title":     a.Title(),
		"timestamp": a.Timestamp.Format(time.RFC3339Nano),
	}
	if a.RunName != "" {
		d["run_name"] = a.RunName
	}
	if a.NodeID != "" {
		d["node_id"] = a.NodeID
	}
	if a.Reason != "" {
		d["reason"] = a.Reason
	}
	if a.FailureCode != "" {
		d["failure_code"] = a.FailureCode
	}
	if a.Axis != "" {
		d["axis"] = a.Axis
		// Mirrors the `omitempty` on BudgetPct and WebhookText's guard: an
		// axis-less dimension has no ratio, and publishing 0 would let the
		// SPA render a percentage nothing measured.
		if a.BudgetPct > 0 {
			d["budget_pct"] = a.BudgetPct
		}
	}
	if a.Link != "" {
		d["link"] = a.Link
	}
	return d
}

// Sink delivers an Alert to a destination. Implementations must be
// safe for concurrent use; the Manager invokes each sink in its own
// goroutine with a bounded context so a slow destination cannot stall
// detection.
type Sink interface {
	Notify(ctx context.Context, a Alert)
}

// FuncSink adapts a plain function to the Sink interface. Used for
// in-process delivery (broker publish for browser toasts, Wails
// EventsEmit for desktop notifications).
type FuncSink func(ctx context.Context, a Alert)

// Notify implements Sink.
func (f FuncSink) Notify(ctx context.Context, a Alert) { f(ctx, a) }

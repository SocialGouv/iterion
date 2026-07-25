// Package usernotify delivers user-addressed notifications for run
// lifecycle moments — a run pausing on a human form, finishing, failing —
// to per-user channels (web push first; OS notifications for the desktop
// app and email are future sinks). It consumes run-outcome trigger.Events
// from the eventbus spine, resolves recipients (run owner + team-wide
// opt-ins), and fans out to its sinks.
//
// It is distinct from pkg/notify (run-completion webhooks to a caller-owned
// callback URL) and pkg/alert (run-health liveness alerts, broadcast to
// operator-wide sinks): usernotify targets identified USERS.
package usernotify

import "context"

// Kind classifies what happened; sinks may render each differently.
type Kind string

const (
	KindHumanInputRequested Kind = "human_input_requested"
	KindRunFinished         Kind = "run_finished"
	KindRunFailed           Kind = "run_failed"
	KindRunCancelled        Kind = "run_cancelled"
)

// Notification is one user-facing notification, already resolved to its
// recipients and rendered to display strings. Sinks only deliver it.
type Notification struct {
	Kind     Kind
	TenantID string
	// UserIDs are the resolved recipients (run owner + team-wide opt-ins).
	UserIDs []string
	Title   string
	Body    string
	// Link is the absolute (or scope-relative) URL opened on click,
	// deep-linking to the run console where the pending form renders.
	Link  string
	RunID string
	// Tag is the collapse key — one visible notification per run.
	Tag string
	// Data carries structured extras for sinks that can use them
	// (interaction_id, node_id).
	Data map[string]string
}

// Sink delivers a Notification through one channel. Implementations must be
// safe for concurrent use. Web push is the first sink; the desktop (Wails)
// OS-notification sink and an email sink plug in here without dispatcher
// changes.
type Sink interface {
	Name() string
	Deliver(ctx context.Context, n Notification) error
}

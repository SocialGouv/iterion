package alert

import (
	"context"
	"fmt"

	sentry "github.com/getsentry/sentry-go"

	"github.com/SocialGouv/iterion/pkg/errtrack"
)

// TrackerSink forwards alerts to the error tracker so a run that fails,
// stalls or burns its budget lands in the same incident stream as a
// panic — instead of only in a webhook an operator may not have wired.
type TrackerSink struct{}

// NewTrackerSink returns the tracker sink, or nil when error tracking
// is disabled. Nil means "don't append me": the caller pattern matches
// NewWebhookSink's, so a manager built without a DSN carries exactly
// the sinks it carried before.
func NewTrackerSink() Sink {
	if !errtrack.Enabled() {
		return nil
	}
	return TrackerSink{}
}

// Notify implements Sink. The message is the alert KIND, not its
// title: the title embeds the run name, which would fragment one
// condition into one tracker issue per run. Everything identifying
// travels as context instead.
func (TrackerSink) Notify(_ context.Context, a Alert) {
	fields := map[string]any{
		"run_id": a.RunID,
		"title":  a.Title(),
	}
	if a.RunName != "" {
		fields["run_name"] = a.RunName
	}
	if a.NodeID != "" {
		fields["node_id"] = a.NodeID
	}
	if a.Reason != "" {
		fields["reason"] = a.Reason
	}
	if a.Axis != "" {
		fields["axis"] = a.Axis
		fields["budget_pct"] = a.BudgetPct
	}
	if a.Link != "" {
		fields["link"] = a.Link
	}

	msg := fmt.Sprintf("run alert: %s", a.Kind)
	switch a.Kind {
	case KindRunFailed, KindBudgetExceeded:
		errtrack.CaptureMessage(sentry.LevelError, msg, fields)
	case KindStall, KindBudgetWarning, KindRunParked:
		// Parked is a real incident for the operator (a run waiting out a
		// window or a resume) — a breadcrumb alone is never sent to the
		// tracker, and the webhook must not be the only channel.
		errtrack.CaptureMessage(sentry.LevelWarning, msg, fields)
	default:
		// A recovery (stall_recovered) closes an episode; it is context
		// for the next incident, not an incident of its own.
		errtrack.AddBreadcrumb(sentry.LevelInfo, msg, fields)
	}
}

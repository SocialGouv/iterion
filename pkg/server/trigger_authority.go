package server

import (
	"context"

	"github.com/SocialGouv/iterion/pkg/eventbus"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// The trigger bus is a relay: a run-outcome event carries a claimed
// tenant and, through a matching subscription, launches a run under it
// with payload-derived vars (D14 F5/F6). The publisher is not only the
// server — the cloud runner publishes its own runs' outcomes with the
// platform credential — so a compromised pod could forge
// iterion.events.run.finished.<victim> and fire the victim's
// subscriptions. The entry check re-derives the authority from the
// store, which the bus cannot forge: the run must exist, carry the
// event's claimed tenant, and sit in a status consistent with the
// claimed kind. A forged or stale event is dropped with a Warn, never
// matched.

// kindConsistentWithStatus reports whether a run.<outcome> kind is what
// the run document's persisted status can produce (the mirror of
// trigger.BuildRunOutcome's classification).
func kindConsistentWithStatus(kind string, status store.RunStatus) bool {
	switch kind {
	case trigger.KindRunFinished:
		return status == store.RunStatusFinished
	case trigger.KindRunFailed:
		return status == store.RunStatusFailed || status == store.RunStatusFailedResumable
	case trigger.KindRunCancelled:
		return status == store.RunStatusCancelled
	case trigger.KindRunPaused:
		return status.IsPaused()
	}
	return false
}

// verifyRunEventAuthority wraps a bus handler with the relay's entry
// check for run-lifecycle events. A nil runs store (the local
// single-process dispatch path, where the only publisher is this
// process) skips the check.
func verifyRunEventAuthority(runs store.RunStore, logger *iterlog.Logger, next eventbus.Handler) eventbus.Handler {
	if runs == nil {
		return next
	}
	return func(ctx context.Context, ev trigger.Event) error {
		if ev.Source == trigger.SourceRun {
			r, err := runs.LoadRun(store.WithoutTenantFilter(ctx), ev.Subject.ID)
			switch {
			case err != nil:
				if logger != nil {
					logger.Warn("trigger: dropped %s event for unknown run %q (claimed tenant %q): %v", ev.Kind, ev.Subject.ID, ev.TenantID, err)
				}
				return nil
			case r.TenantID != ev.TenantID:
				if logger != nil {
					logger.Warn("trigger: dropped %s event for run %q — tenant %q does not match the document's %q", ev.Kind, ev.Subject.ID, ev.TenantID, r.TenantID)
				}
				return nil
			case !kindConsistentWithStatus(ev.Kind, r.Status):
				if logger != nil {
					logger.Warn("trigger: dropped %s event for run %q — status %q cannot produce it", ev.Kind, ev.Subject.ID, r.Status)
				}
				return nil
			}
		}
		return next(ctx, ev)
	}
}

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

// verifyRunEventAuthority wraps a bus handler with the relay's entry
// check for run-lifecycle events. A nil runs store (the local
// single-process dispatch path, where the only publisher is this
// process) skips the check — the wiring warns loudly when that happens
// on a bus that could be cross-process.
//
// The check does NOT trust the wire copy: a terminal run's status is
// immutable, so a replayed (runID, tenant, kind) triple would pass any
// consistency test forever while the payload — vars, args, subject
// title — is attacker-controlled on the wire and feeds the launch plan.
// The relayed event is REBUILT from the run document
// (trigger.BuildRunOutcome derives payload, subject and repo from the
// store), so the wire event contributes only "this run exited"; a
// genuine replay re-delivers the doc-derived event and the episode id
// dedups it downstream. A claim that disagrees with the document
// (tenant, kind, unknown run) is a forgery: dropped with a Warn.
func verifyRunEventAuthority(runs store.RunStore, logger *iterlog.Logger, next eventbus.Handler) eventbus.Handler {
	if runs == nil {
		return next
	}
	return func(ctx context.Context, ev trigger.Event) error {
		if ev.Source == trigger.SourceRun {
			rebuilt := trigger.BuildRunOutcome(ctx, runs, ev.Subject.ID, nil)
			if rebuilt.TenantID == "" || rebuilt.TenantID != ev.TenantID || rebuilt.Kind != ev.Kind {
				if logger != nil {
					logger.Warn("trigger: dropped %s event for run %q claiming tenant %q — the document says tenant %q, status-derived kind %q", ev.Kind, ev.Subject.ID, ev.TenantID, rebuilt.TenantID, rebuilt.Kind)
				}
				return nil
			}
			// A running or queued run has not exited: BuildRunOutcome's
			// default would classify it run.finished (R0a963c), firing the
			// victim's finished-subscriptions prematurely. Only a status
			// that IS an exit — or a human-gate pause — relays.
			if !runStatusRelayable(store.RunStatus(rebuilt.Subject.State)) {
				if logger != nil {
					logger.Warn("trigger: dropped %s event for run %q — status %q is not an exit, the run is still going", ev.Kind, ev.Subject.ID, rebuilt.Subject.State)
				}
				return nil
			}
			return next(ctx, rebuilt)
		}
		return next(ctx, ev)
	}
}

// runStatusRelayable reports whether a persisted status IS a run exit
// (or a human-gate pause, which relays run.paused): a running or queued
// run implies no outcome at all.
func runStatusRelayable(s store.RunStatus) bool {
	switch s {
	case store.RunStatusFinished, store.RunStatusFailed, store.RunStatusFailedResumable, store.RunStatusCancelled:
		return true
	}
	return s.IsPaused()
}

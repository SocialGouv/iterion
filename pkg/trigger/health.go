package trigger

import (
	"context"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// healthWriteTimeout bounds the detached last_error write.
const healthWriteTimeout = 5 * time.Second

// recordLaunchVerdict persists on the subscription itself what became of the
// direct launch it just asked for: a refusal — an org launch-gate denial above
// all — raises last_error, and the next launch that goes through clears it.
// EVERY direct-launch site in this package calls it (the evaluator's launch
// arm and the scheduler's tick), because a warn line in one replica's log is
// not a surface: without this a subscription silently stops producing runs and
// the triggers API still shows it healthy.
//
// Written only when the verdict CHANGES — an outbox retry re-reports the same
// refusal on every backoff, and this row is read by an operator, not by a hot
// loop. Failing to write it is logged, never fatal: the launch's own outcome
// is what the caller is waiting on.
func recordLaunchVerdict(ctx context.Context, subs SubscriptionStore, logger *iterlog.Logger, sub Subscription, launchErr error) {
	if subs == nil || sub.ID == "" {
		return
	}
	msg := ""
	if launchErr != nil {
		msg = launchErr.Error()
	}
	if msg == sub.LastError {
		return
	}
	// Detached ctx: a cancelled parent (this replica draining mid-launch) must
	// not be what decides whether the operator ever sees the refusal.
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), healthWriteTimeout)
	defer cancel()
	if err := subs.MarkLaunchError(wctx, sub.ID, msg, time.Now().UTC()); err != nil && logger != nil {
		logger.Warn("trigger: recording the launch verdict of subscription %s failed: %v", sub.ID, err)
	}
}

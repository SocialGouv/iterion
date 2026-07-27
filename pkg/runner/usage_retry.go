package runner

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	natsq "github.com/SocialGouv/iterion/pkg/queue/nats"
	"github.com/SocialGouv/iterion/pkg/retrypolicy"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Usage-window carve-out.
//
// A run that dies because the provider's quota window is exhausted must NOT
// go down the generic nak path. Naking hands it back to JetStream, which
// redelivers up to MaxDeliver times within AckWait each — one fresh pod per
// attempt, each re-hitting a wall that cannot move for hours or days, then a
// DLQ park. Measured on 2026-07-27: seven scheduled runs, eight pods each,
// against a reset ~35h away.
//
// So we ACK (the run is already a good failed_resumable checkpoint) and
// persist WHEN to come back. The server-side sweeper owns the wait, because
// it is the only thing that outlives the pod by days.
//
// This mirrors the ErrBudgetExceeded carve-out next to it in
// classifyExecResult: same reasoning (redelivery cannot help, and costs a
// pod), different remedy (a budget needs a human to raise a cap; a quota
// window just needs time).

const (
	// usageWindowFloor keeps a retry from being scheduled effectively now.
	// The provider's notice may name a non-UTC zone while the parser reads
	// it as UTC, so a "reset" instant can land slightly in the past; a
	// small floor absorbs that without a second guess at the timezone.
	usageWindowFloor = 5 * time.Minute
	// usageWindowBlindWait is the fallback when the provider told us a
	// window is exhausted but nothing in the text parses as a reset time.
	// Deliberately bounded and short-ish: one wasted pod an hour beats
	// either giving up on the run or waiting a speculative week.
	usageWindowBlindWait = time.Hour
)

// usageWindowRetryAt decides WHEN a usage-window failure should be retried,
// or reports false when it should not be. Pure, so the ordering of its
// evidence sources is testable.
//
// The sources are tried structure-first, string-last on purpose. The typed
// error is authoritative when it survives to here; the code is authoritative
// when a recovery dispatcher classified it; and the flattened message is a
// last resort for a host that has neither — which is not hypothetical, since
// a runner with no dispatcher wired classifies nothing at all.
func usageWindowRetryAt(execErr error, pol retrypolicy.Policy, now time.Time) (time.Time, string, bool) {
	if execErr == nil || !pol.Enabled() {
		return time.Time{}, "", false
	}

	var at time.Time
	var source string

	var rl *delegate.ErrRateLimited
	switch {
	case errors.As(execErr, &rl) && rl.Kind == delegate.RateLimitKindUsageWindow:
		at, source = rl.ResetAt, "typed_error"
	case runtimeCodeOf(execErr) == runtime.ErrCodeUsageLimitBlocked:
		source = "runtime_code"
	default:
		return time.Time{}, "", false
	}

	// A usage window with no usable reset instant still gets a retry — the
	// window is real, only its end is unknown.
	if at.IsZero() {
		if parsed, ok := delegate.ParseResetHint(execErr.Error(), now); ok {
			at, source = parsed, source+"+parsed_text"
		} else {
			at, source = now.Add(usageWindowBlindWait), source+"+blind_wait"
		}
	} else {
		// Come back just after the reset, not exactly on it.
		at = at.Add(time.Minute)
	}

	// Spread runs that share one reset instant. Several schedules commonly
	// die on the same window (five feed-watch digests did), and resuming
	// them simultaneously can exhaust the fresh window immediately.
	if j := pol.JitterDuration(); j > 0 {
		at = at.Add(rand.N(j))
	}
	if floor := now.Add(usageWindowFloor); at.Before(floor) {
		at = floor
	}
	if ceiling := now.Add(pol.MaxWaitDuration()); at.After(ceiling) {
		at = ceiling
	}
	return at.UTC(), source, true
}

// runtimeCodeOf extracts the RuntimeError code from an error chain, or "".
func runtimeCodeOf(err error) runtime.ErrorCode {
	var rtErr *runtime.RuntimeError
	if errors.As(err, &rtErr) && rtErr != nil {
		return rtErr.Code
	}
	return ""
}

// runRetryPolicy resolves the retry policy for a run. The policy was
// resolved across all its layers at LAUNCH and snapshotted on the run doc,
// so the runner just reads it — it never has to know that schedules,
// manifests or bindings exist. A run predating the snapshot (nil) gets the
// package defaults plus the platform ceiling, which is also what a run
// launched by a surface that does not resolve policies gets.
func runRetryPolicy(r *store.Run) retrypolicy.Policy {
	var pol retrypolicy.Policy
	if r != nil && r.RetryPolicy != nil {
		pol = retrypolicy.Policy{
			UsageWindow: r.RetryPolicy.UsageWindow,
			MaxAttempts: r.RetryPolicy.MaxAttempts,
			MaxWait:     r.RetryPolicy.MaxWait,
			Jitter:      r.RetryPolicy.Jitter,
		}
	}
	return retrypolicy.Clamp(retrypolicy.Normalize(pol), retrypolicy.CeilingFromEnv(), nil)
}

// parkUsageLimitRetry arms a durable retry for a usage-window failure and
// acks the delivery. Reports handled=false when this is not that case, or
// when the intent could NOT be persisted — the caller then falls through to
// today's nak/DLQ behaviour. That fall-through is the important half: acking
// without a durable intent would drop the run silently, which is strictly
// worse than the wasted redeliveries this whole change exists to avoid.
func (r *Runner) parkUsageLimitRetry(
	ctx context.Context,
	execErr error,
	delivery *natsq.Delivery,
	msg *queue.RunMessage,
	logger *iterlog.Logger,
) (bool, string) {
	retryStore := store.AsRunRetryStore(r.cfg.Store)
	if retryStore == nil {
		return false, "" // local/filesystem store: no durable retry surface
	}

	runMeta, err := r.cfg.Store.LoadRun(ctx, msg.RunID)
	if err != nil {
		// Without the run doc we cannot read the policy; treating that as
		// "no retry" is the honest degradation.
		logger.Warn("runner: run %s: cannot read retry policy (%v) — falling back to redelivery", msg.RunID, err)
		return false, ""
	}
	pol := runRetryPolicy(runMeta)

	at, source, ok := usageWindowRetryAt(execErr, pol, time.Now().UTC())
	if !ok {
		return false, ""
	}

	scheduled, attempt, err := retryStore.ScheduleRunRetry(ctx, msg.RunID, at, "usage_window", string(runtime.ErrCodeUsageLimitBlocked), pol.MaxAttempts)
	if err != nil {
		logger.Error("runner: run %s: could not persist the usage-window retry (%v) — falling back to redelivery so the run is not lost", msg.RunID, err)
		return false, ""
	}
	if !scheduled {
		// Either the attempt budget is spent or the run is no longer
		// resumable (an operator got there first). Ack either way: a
		// redelivery would re-hit the same wall, and say why in the run's
		// own error field so it does not just go quiet.
		reason := fmt.Sprintf("usage-window retries exhausted (max %d) — resume manually once the provider quota resets", pol.MaxAttempts)
		if abandonErr := retryStore.AbandonRunRetry(ctx, msg.RunID, reason); abandonErr != nil {
			logger.Warn("runner: run %s: could not record the exhausted retry budget: %v", msg.RunID, abandonErr)
		}
		logger.Warn("runner: run %s: %s", msg.RunID, reason)
		ackTerminal(logger, delivery, "ack-usage-limit-exhausted", msg.RunID)
		return true, "usage_limit_exhausted"
	}

	if emitErr := r.emitRetryScheduled(ctx, msg.RunID, at, attempt, pol, source); emitErr != nil {
		// Observational only — the durable intent is already committed.
		logger.Warn("runner: run %s: could not emit run_retry_scheduled: %v", msg.RunID, emitErr)
	}
	logger.Warn("runner: run %s hit the provider usage window — retry %d/%d armed for %s (reset source: %s), NOT redelivering",
		msg.RunID, attempt, pol.MaxAttempts, at.Format(time.RFC3339), source)
	ackTerminal(logger, delivery, "ack-usage-limit-retry", msg.RunID)
	return true, "usage_limit_retry"
}

// emitRetryScheduled records the armed retry on the run's timeline so the
// wait is visible rather than looking like a dead run.
func (r *Runner) emitRetryScheduled(ctx context.Context, runID string, at time.Time, attempt int, pol retrypolicy.Policy, source string) error {
	_, err := r.cfg.Store.AppendEvent(ctx, runID, store.Event{
		Type: store.EventRunRetryScheduled,
		Data: map[string]any{
			"code":         string(runtime.ErrCodeUsageLimitBlocked),
			"reason":       "usage_window",
			"retry_after":  at.Format(time.RFC3339),
			"attempt":      attempt,
			"max_attempts": pol.MaxAttempts,
			"reset_source": source,
		},
	})
	return err
}

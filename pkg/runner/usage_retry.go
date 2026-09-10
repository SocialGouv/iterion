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
	"github.com/SocialGouv/iterion/pkg/retrycoord"
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
	// usageRetryStoreTimeout bounds the detached store writes that arm the
	// retry. Generous enough for a Mongo round trip on a slow day, short
	// enough that a wedged store cannot hold the delivery past its ack
	// deadline.
	usageRetryStoreTimeout = 10 * time.Second
	// The shared circuit is advisory to the mandatory per-run arm. Give it a
	// smaller slice so a slow circuit collection cannot consume the entire
	// detached store budget and make ScheduleRunRetry inherit an expired ctx.
	retryCircuitStoreTimeout = 2 * time.Second
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
//
// skippedReopensAt is when the earliest credential the launch's resolution
// PASSED OVER reopens (store.Run.SkippedCredReopensAt; zero when none). The
// retry arms on the earlier of that and the failed credential's own reset:
// the credential that failed is the one the walk fell THROUGH to, and the
// one it skipped often reopens first — a team key refused on its five-hour
// window reopens the same afternoon while the platform forfait it fell
// through to is walled until Monday. Fourteen reviews slept four days
// instead of three hours on that difference. The resume re-resolves the
// chain, so coming back at the earlier instant lands on the reopened key.
//
// That earlier wake is SPECULATIVE — the skipped credential may be refused
// too — while every arming spends one attempt of the same budget
// (store.RunRetryStore.ScheduleRunRetry increments it and refuses past
// max_attempts). attemptsSpent is how many this run has already armed
// (store.RunRetryState.Attempts), and it is what keeps the two clocks out of
// one purse: see reservesLastAttempt.
func usageWindowRetryAt(execErr error, pol retrypolicy.Policy, now time.Time, skippedReopensAt time.Time, attemptsSpent int) (time.Time, string, bool) {
	if execErr == nil || !pol.Enabled() {
		return time.Time{}, "", false
	}

	at, source, ok := usageWindowEvidence(execErr)
	if !ok {
		return time.Time{}, "", false
	}

	// authoritativeAt is the failed credential's own reset — the wall that
	// actually blocks this run. It stays zero when the provider named no
	// instant: a blind wait is a guess, so there is no wall to reserve an
	// attempt for.
	var authoritativeAt time.Time

	// A usage window with no usable reset instant still gets a retry — the
	// window is real, only its end is unknown.
	if at.IsZero() {
		if parsed, pok := delegate.ParseResetHint(execErr.Error(), now); pok {
			at, source = parsed, source+"+parsed_text"
			authoritativeAt = parsed
		} else {
			at, source = now.Add(usageWindowBlindWait), source+"+blind_wait"
		}
	} else {
		// Come back just after the reset, not exactly on it.
		at = at.Add(time.Minute)
		authoritativeAt = at
	}
	if !skippedReopensAt.IsZero() {
		if alt := skippedReopensAt.Add(time.Minute); alt.Before(at) {
			if reservesLastAttempt(pol, attemptsSpent, authoritativeAt, now) {
				// The budget has one arming left and the wall is still
				// ahead: spending it on another credential's reopening
				// would retire the run before the wall it waits on falls.
				// Keep the authoritative instant, and say so on the event.
				source += "+last_attempt_pinned"
			} else {
				at, source = alt, "skipped_credential"
			}
		}
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

// usageWindowCircuitAt moves a per-run retry behind an open shared circuit
// without discarding the policy guarantees already applied by
// usageWindowRetryAt. A shared OpenUntil is deliberately jittered again:
// otherwise every run participating in the circuit wakes at the same instant.
// The max-wait ceiling remains authoritative even when the deployment-level
// circuit cooldown is longer than the run's resolved retry policy.
func usageWindowCircuitAt(at time.Time, state *store.RetryCircuitState, pol retrypolicy.Policy, now time.Time) (time.Time, bool) {
	if state == nil || state.OpenUntil == nil || !state.OpenUntil.After(at) {
		return at, false
	}
	at = state.OpenUntil.UTC()
	if j := pol.JitterDuration(); j > 0 {
		at = at.Add(rand.N(j))
	}
	if ceiling := now.UTC().Add(pol.MaxWaitDuration()); at.After(ceiling) {
		at = ceiling
	}
	return at.UTC(), true
}

// reservesLastAttempt reports that the arming about to happen is the LAST one
// the budget allows and the authoritative reset is still reachable — the two
// conditions under which the speculative wake must be declined.
//
// The invariant: the attempt budget must not expire before the authoritative
// reset is reachable. Every arming spends one attempt, so a speculative wake
// taken on the last one leaves nothing for the wall that actually blocks the
// run — five attempts on a five-hour cycle cover 25h of a seven-day window.
// Reserving the last one gives the run exactly one try at that wall, and
// leaves the "do not hammer a wall that cannot move" bound untouched.
//
// It reserves nothing in three cases, each because there is no reachable wall
// to reserve for:
//   - the provider named no reset instant (authoritativeAt zero): a blind wait
//     is a guess, not a wall;
//   - the reset is already behind us: the window has reopened;
//   - the reset lies past the policy's horizon — including the jitter, which
//     is added BEFORE the max_wait clamp, so a reservation the spread would
//     clamp back short of the wall is no reservation at all. There the
//     ceiling clamp would land the attempt before the reset anyway, and the
//     speculative wake is the strictly better use of it.
func reservesLastAttempt(pol retrypolicy.Policy, attemptsSpent int, authoritativeAt, now time.Time) bool {
	if pol.MaxAttempts <= 0 || authoritativeAt.IsZero() {
		return false
	}
	if attemptsSpent+1 < pol.MaxAttempts {
		return false // a later attempt can still take the authoritative reset
	}
	if !authoritativeAt.After(now) {
		return false
	}
	return !authoritativeAt.Add(pol.JitterDuration()).After(now.Add(pol.MaxWaitDuration()))
}

// usageWindowEvidence answers the one question "did this error mean the
// provider's quota window is shut, and when does it reopen".
//
// Shared because two consumers ask it: the retry arming below, and the
// credential pool deciding whether to rest a donor. Two copies of an
// evidence chain drift — the pool's did, silently dropping the text-parsed
// reset the retry path relies on.
//
// Sources are tried structure-first, string-last on purpose. The typed
// error is authoritative when it survives to here; the code is
// authoritative when a recovery dispatcher classified it.
func usageWindowEvidence(execErr error) (resetAt time.Time, source string, ok bool) {
	var rl *delegate.ErrRateLimited
	switch {
	case errors.As(execErr, &rl) && rl.Kind == delegate.RateLimitKindUsageWindow:
		return rl.ResetAt, "typed_error", true
	case runtimeCodeOf(execErr) == runtime.ErrCodeUsageLimitBlocked:
		return time.Time{}, "runtime_code", true
	// The flattened message, last. This source was DOCUMENTED above from the
	// start and never implemented, so a host that had neither of the two
	// above got no retry at all — the case the doc calls "not hypothetical".
	// Measured 2026-08-10: four reviews died on one Anthropic weekly cap,
	// every one of them carrying the provider's own words in run.Error, and
	// not one armed a retry; four pull requests waited on a required check
	// nobody would ever answer.
	case delegate.UsageWindowInFlattenedError(execErr.Error()):
		return time.Time{}, "flattened_text", true
	}
	return time.Time{}, "", false
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
	outcome := r.armUsageWindowRetry(ctx, execErr, msg.RunID, logger)
	switch outcome {
	case usageRetryNotApplicable:
		return false, ""
	case usageRetryExhausted:
		ackTerminal(logger, delivery, "ack-usage-limit-exhausted", msg.RunID)
		return true, "usage_limit_exhausted"
	default:
		ackTerminal(logger, delivery, "ack-usage-limit-retry", msg.RunID)
		return true, "usage_limit_retry"
	}
}

// usageRetryOutcome is what armUsageWindowRetry decided, so the delivery
// handling stays in one place and the decision half is testable without a
// live JetStream delivery.
type usageRetryOutcome int

const (
	// usageRetryNotApplicable: not a usage window, or the intent could not
	// be persisted — the caller must fall through to nak/DLQ.
	usageRetryNotApplicable usageRetryOutcome = iota
	// usageRetryArmed: a retry is durably scheduled.
	usageRetryArmed
	// usageRetryExhausted: the attempt budget is spent (or the run is no
	// longer resumable) — ack, but nothing will come back on its own.
	usageRetryExhausted
)

// armUsageWindowRetry is the decide-and-persist half: everything except
// finalizing the delivery.
func (r *Runner) armUsageWindowRetry(
	ctx context.Context,
	execErr error,
	runID string,
	logger *iterlog.Logger,
) usageRetryOutcome {
	retryStore := store.AsRunRetryStore(r.cfg.Store)
	if retryStore == nil {
		return usageRetryNotApplicable // local/filesystem store: no durable retry surface
	}

	// The run context is ALREADY CANCELLED by the time we get here: the
	// caller must stop the heartbeat before finalizing the delivery, and
	// that cancel is what stops it. Every store call below would therefore
	// fail instantly on a cancelled context, we would log a warning and
	// fall through — arming no retry, ever, in production, while the unit
	// tests (which exercise the pure decision and the sweeper) stayed
	// green. So detach: values (tenant/owner identity, needed by the Mongo
	// tenant filter) are preserved, cancellation is not. Same reasoning as
	// the engine's post-cancellation checkpoint write.
	//
	// Detaching HERE rather than at the call site is deliberate — the next
	// caller would otherwise trip the same wire.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), usageRetryStoreTimeout)
	defer cancel()

	runMeta, err := r.cfg.Store.LoadRun(ctx, runID)
	if err != nil {
		// Without the run doc we cannot read the policy; treating that as
		// "no retry" is the honest degradation.
		logger.Warn("runner: run %s: cannot read retry policy (%v) — falling back to redelivery", runID, err)
		return usageRetryNotApplicable
	}
	pol := runRetryPolicy(runMeta)

	var skipped time.Time
	if runMeta.SkippedCredReopensAt != nil {
		skipped = *runMeta.SkippedCredReopensAt
	}
	// Attempts already charged to this run, read from the same document
	// ScheduleRunRetry's CAS increments below — so the decision is made
	// against the count the arming is about to be charged against. The run
	// document is the only place it lives: a final continuation clears
	// retry_after but keeps attempts, so the count is a lifetime total and
	// survives the re-failure that precedes every arming.
	var attemptsSpent int
	if runMeta.RetryState != nil {
		attemptsSpent = runMeta.RetryState.Attempts
	}
	decisionNow := time.Now().UTC()
	at, source, ok := usageWindowRetryAt(execErr, pol, decisionNow, skipped, attemptsSpent)
	if !ok {
		return usageRetryNotApplicable
	}
	if r.cfg.Metrics != nil {
		r.cfg.Metrics.RunsUsageWindowBlocked.Inc()
	}
	// Coordinate the retry wave across runner replicas. The per-run retry
	// ledger remains authoritative for the attempt bound; the shared circuit
	// only moves the next wake-up out of a provider-wide failure storm.
	if key := retrycoord.Key(runMeta); key != "" {
		circuitCtx, circuitCancel := context.WithTimeout(ctx, retryCircuitStoreTimeout)
		circuitState, circuitErr := retrycoord.RecordFailure(circuitCtx, r.cfg.Store, key, runID, decisionNow, retrycoord.FromEnv())
		circuitCancel()
		if circuitErr != nil {
			// A circuit-store outage must not drop a durable per-run retry. The
			// existing ScheduleRunRetry below still provides the safe fallback.
			logger.Warn("runner: run %s: retry circuit update failed (%v) — scheduling per-run retry", runID, circuitErr)
		} else if coordinatedAt, delayed := usageWindowCircuitAt(at, circuitState, pol, decisionNow); delayed {
			at = coordinatedAt
			source += "+circuit_open"
		}
	}

	scheduled, attempt, err := retryStore.ScheduleRunRetry(ctx, runID, at, "usage_window", string(runtime.ErrCodeUsageLimitBlocked), pol.MaxAttempts)
	if err != nil {
		logger.Error("runner: run %s: could not persist the usage-window retry (%v) — falling back to redelivery so the run is not lost", runID, err)
		return usageRetryNotApplicable
	}
	if !scheduled {
		// Either the attempt budget is spent or the run is no longer
		// resumable (an operator got there first). Ack either way: a
		// redelivery would re-hit the same wall, and say why in the run's
		// own error field so it does not just go quiet.
		reason := fmt.Sprintf("usage-window retries exhausted (max %d) — resume manually once the provider quota resets", pol.MaxAttempts)
		if abandonErr := retryStore.AbandonRunRetry(ctx, runID, reason); abandonErr != nil {
			logger.Warn("runner: run %s: could not record the exhausted retry budget: %v", runID, abandonErr)
		}
		logger.Warn("runner: run %s: %s", runID, reason)
		return usageRetryExhausted
	}

	if r.cfg.Metrics != nil {
		r.cfg.Metrics.RunsRetryScheduled.Inc()
	}
	if emitErr := r.emitRetryScheduled(ctx, runID, at, attempt, pol, source, skipped); emitErr != nil {
		// Observational only — the durable intent is already committed.
		logger.Warn("runner: run %s: could not emit run_retry_scheduled: %v", runID, emitErr)
	}
	logger.Warn("runner: run %s hit the provider usage window — retry %d/%d armed for %s (reset source: %s), NOT redelivering",
		runID, attempt, pol.MaxAttempts, at.Format(time.RFC3339), source)
	return usageRetryArmed
}

// emitRetryScheduled records the armed retry on the run's timeline so the
// wait is visible rather than looking like a dead run. skippedReopensAt,
// when set, names the skipped credential's reopening the arming considered
// — so a retry that came back early reads as a decision, not an accident.
func (r *Runner) emitRetryScheduled(ctx context.Context, runID string, at time.Time, attempt int, pol retrypolicy.Policy, source string, skippedReopensAt time.Time) error {
	data := map[string]any{
		"code":         string(runtime.ErrCodeUsageLimitBlocked),
		"reason":       "usage_window",
		"retry_after":  at.Format(time.RFC3339),
		"attempt":      attempt,
		"max_attempts": pol.MaxAttempts,
		"reset_source": source,
	}
	if !skippedReopensAt.IsZero() {
		data["skipped_cred_reopens_at"] = skippedReopensAt.UTC().Format(time.RFC3339)
	}
	_, err := r.cfg.Store.AppendEvent(ctx, runID, store.Event{
		Type: store.EventRunRetryScheduled,
		Data: data,
	})
	return err
}

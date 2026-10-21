package runner

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
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
	// blindWaitSource marks an instant that is a GUESS rather than the
	// provider's word — usageWindowRetryAt appends it when nothing in the
	// refusal parses as a reset. Readers care because a guess must never be
	// written over an instant the provider actually named.
	blindWaitSource = "+blind_wait"
	// announcedRestoredSource marks a reset instant the probe set aside and
	// an arming put back, so the timeline distinguishes it from one the
	// fresh refusal named.
	announcedRestoredSource = "+announced_restored"
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
	if !pol.Enabled() {
		return time.Time{}, "", false
	}
	at, source, authoritativeAt, ok := usageWindowRefusalAt(execErr, now)
	if !ok {
		return time.Time{}, "", false
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

// usageWindowRefusalAt resolves WHEN one usage-window refusal says to come
// back, and names the wall it carried — zero when it carried none.
//
// authoritativeAt is the failed credential's own reset: the wall that actually
// blocks this run. A refusal naming no instant yields a GUESS
// (usageWindowBlindWait) and a zero wall, and both readers of that fact — the
// last-attempt reservation and the probe's restore — take it from HERE rather
// than from the reset source, which later steps rewrite.
func usageWindowRefusalAt(execErr error, now time.Time) (at time.Time, source string, authoritativeAt time.Time, ok bool) {
	if execErr == nil {
		return time.Time{}, "", time.Time{}, false
	}
	at, source, ok = usageWindowEvidence(execErr)
	if !ok {
		return time.Time{}, "", time.Time{}, false
	}
	// A usage window with no usable reset instant still gets a retry — the
	// window is real, only its end is unknown.
	if at.IsZero() {
		if parsed, pok := delegate.ParseResetHint(execErr.Error(), now); pok {
			return parsed, source + "+parsed_text", parsed, true
		}
		return now.Add(usageWindowBlindWait), source + blindWaitSource, time.Time{}, true
	}
	// Come back just after the reset, not exactly on it.
	at = at.Add(time.Minute)
	return at, source, at, true
}

// refusalNamesNoInstant reports that this refusal named no reset of its own,
// so the instant derived from it is a GUESS rather than a wall.
//
// Read from the refusal, never from the reset source: the skipped-credential
// branch REPLACES that string, so a reader keying on it cannot tell a
// wall-less decision from a dated one, and the probe's restore silently stops
// firing on exactly the rows it exists for.
func refusalNamesNoInstant(execErr error, now time.Time) bool {
	_, _, authoritativeAt, ok := usageWindowRefusalAt(execErr, now)
	return ok && authoritativeAt.IsZero()
}

// usageWindowProbedRetryAt is usageWindowRetryAt for a re-failure that
// followed a PULL-FORWARD PROBE, on a row that still holds the instant the
// provider announced — the pair of premises that changes the decision.
//
// Such a row weighs NO other credential's reopening, so none is taken: the
// caller zeroes its own reading of it while the announced instant is live
// (decisionSkipped), and this one passes zero so the branch that would take
// the earlier wake cannot be reached from here at all. With no reopening to
// weigh, there is nothing for the last-attempt reservation to decide either —
// usageWindowRetryAt consults it only against one — so the attempts this
// arming does not spend are not a reading this decision makes.
//
// And `announced` — the instant the probe set aside — beats the one-hour
// fallback a refusal that named no instant falls back to: writing a guess over
// the provider's own word deletes the only wall the run was ever told about,
// and the row then sits inside the probe floor where nothing can pull it
// again. A DATED re-failure still wins, including an EARLIER one — good news
// from the provider is not a probe's to override.
//
// The PAID arming keeps usageWindowRetryAt with the real spent count: on a row
// whose announced instant has passed or was never there, the reopening is
// weighed again and the reservation with it.
//
// Total, and it has to be: usageWindowRetryAt refuses on the error and the
// policy alone — never on the attempt count — and every caller reaches here
// only after that same pair has already said yes.
func usageWindowProbedRetryAt(execErr error, pol retrypolicy.Policy, now, announced time.Time) (time.Time, string) {
	at, source, _ := usageWindowRetryAt(execErr, pol, now, time.Time{}, 0)
	return restoreAnnouncedInstant(at, source, announced, refusalNamesNoInstant(execErr, now))
}

// restoreAnnouncedInstant puts a probe's held-aside instant back when the
// fresh refusal named no instant of its own (noFreshInstant) and the announced
// one is still ahead. Shared by the free re-arm and the paid fall-through: a
// guess written over the wall costs the run its life on either path.
func restoreAnnouncedInstant(at time.Time, source string, announced time.Time, noFreshInstant bool) (time.Time, string) {
	if noFreshInstant && announced.After(at) {
		return announced.UTC(), withAnnouncedRestored(source)
	}
	return at, source
}

// withAnnouncedRestored tags a reset source as carrying the probe's held-aside
// instant, ONCE. Two places restore it — the decision and the refused paid
// arming that falls back to it — and a doubled tag on the run's timeline reads
// as two separate restores.
func withAnnouncedRestored(source string) string {
	if strings.Contains(source, announcedRestoredSource) {
		return source
	}
	return source + announcedRestoredSource
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

// runtimeCodeOf extracts the RuntimeError code from an error chain, or "". An
// undecided effect anywhere in the chain is the code, whatever a wrapper said:
// redelivering it may duplicate the effect.
func runtimeCodeOf(err error) runtime.ErrorCode {
	if runtime.IsAmbiguousEffect(err) {
		return runtime.ErrCodeAmbiguousEffect
	}
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
	delivery jsDelivery,
	msg *queue.RunMessage,
	usage *metricsEmitter,
	logger *iterlog.Logger,
) (bool, string) {
	outcome := r.armUsageWindowRetry(ctx, execErr, msg.RunID, usage.spentOnTheProvider(), logger)
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
//
// spentOnProvider is what the execution now parking got from the provider
// (metricsEmitter.spentOnTheProvider): it is how a probe's refusal, which
// costs nothing, is told apart from a park that followed real work on a
// window that had genuinely reopened.
func (r *Runner) armUsageWindowRetry(
	ctx context.Context,
	execErr error,
	runID string,
	spentOnProvider bool,
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
	// announcedFrom is the instant the provider announced, held aside by the
	// sweeper's pull-forward probe (pkg/server/retry_probe.go) while it asks
	// the preflight whether the window reopened early. It is the only DATED
	// fact this row has about its wall, and it belongs to the ROW, not to
	// whoever pays for the next arming: a refusal that names no instant of
	// its own must never overwrite it, on any path below.
	announcedFrom := runMeta.RetryState.AnnouncedProbeFrom(decisionNow)
	// probeInFlight is the same marker read as the PAYMENT question — "is this
	// park the probe's own speculative refusal?" — which expires with the PULL
	// (store.ProbeMarkerTTL) and has a second input besides. An execution that
	// got past the provider and spent tokens answered it: the window reopened
	// and this run used it, so the park that follows is an ordinary new wall,
	// charged, counted, fed to the shared circuit and announced on the pull
	// request (a PAID arming clears the probe ledger, which is what the pause
	// notice reads). The spend says nothing about WHEN the window reopens,
	// which is why it stops here and does not reach announcedFrom: a node
	// whose every route was refused after burning tokens on one that died
	// reports the same spend (model.dispatchChain folds a failed chain's
	// tokens into the error's Result), and letting that drop the wall replaces
	// days of waiting with hourly guesses that retire the run long before its
	// window reopens.
	// announcedFrom != nil is implied by ProbeInFlight (one is defined over
	// the other, and pkg/store pins the subset); named here so the free
	// arming's read of it below is locally sound.
	probeInFlight := announcedFrom != nil && runMeta.RetryState.ProbeInFlight(decisionNow) && !spentOnProvider
	// decisionSkipped is the skipped credential's reopening as the PAID
	// decision below weighs it — and a row holding the provider's announced
	// instant does not weigh it at all. The free re-arm reaches the same rule
	// without this reading: usageWindowProbedRetryAt weighs no reopening of
	// any other credential, because every row it decides holds that instant.
	// usageWindowRetryAt lets such a reopening REPLACE `at` (the wake is
	// sooner), restoreAnnouncedInstant cannot put the wall back over a
	// refusal that named an instant of its own, and the write that follows
	// consumes probe_from: the only durable copy of the provider's word would
	// be traded for a speculation about another credential, days before the
	// window it names. The row keeps the wall instead, and what
	// that costs the credential is one probe FLOOR while the wait's probe
	// ledger is under its budget: a paid arming resets the ledger and a free
	// re-arm leaves it exactly where the pull left it, so the next probe gives
	// that credential its chance one floor later, free. Once the budget's last
	// probe has been pulled, nothing lists this wait again and the cost is the
	// REST of the wait — the run sleeps to the announced instant whatever
	// another credential does, which is the bound on keeping the wall and is
	// stated as such in docs/scheduling.md. The timeline event below still
	// carries the raw reopening: it is a fact about the run either way.
	decisionSkipped := skipped
	if announcedFrom != nil {
		decisionSkipped = time.Time{}
	}
	at, source, ok := usageWindowRetryAt(execErr, pol, decisionNow, decisionSkipped, attemptsSpent)
	if !ok {
		return usageRetryNotApplicable
	}
	// `at`/`source` are the PAID decision: made with the attempts this run
	// has actually spent, and (on a probed row) with the announced instant
	// restored over a blind guess, because the fall-through below arms that
	// same instant for a price. A DATED re-failure still wins: fresh news
	// from the provider outranks the instant the probe set aside.
	if announcedFrom != nil {
		at, source = restoreAnnouncedInstant(at, source, *announcedFrom, refusalNamesNoInstant(execErr, decisionNow))
	}

	// A re-failure that followed a PROBE spends no attempt. The probe pulled
	// this run forward speculatively and the preflight refused before
	// anything cost money, so charging it would retire the run before the
	// wall it waits on falls — the same "two clocks, one purse" error
	// reservesLastAttempt exists to prevent, with the probe as the second
	// clock. The arming consumes the marker, so the run's NEXT failure is
	// charged normally. Its instant is decided by usageWindowProbedRetryAt,
	// which holds the announced wall over a guess that would replace it.
	if probeInFlight {
		freeAt, freeSource := usageWindowProbedRetryAt(execErr, pol, decisionNow, *announcedFrom)
		rearmed, rearmErr := retryStore.RearmProbedRunRetry(ctx, runID, freeAt)
		switch {
		case rearmErr != nil:
			// Say it and fall through to the ordinary arming: a store blip
			// must not cost the run its durable retry, which is the one
			// thing this whole path exists to keep. The attempt it charges
			// is the price of that.
			logger.Warn("runner: run %s: could not re-arm the probed retry free of charge (%v) — falling back to the ordinary arming, which spends an attempt", runID, rearmErr)
		case rearmed:
			freeSource += "+after_probe"
			r.noteFreeRearm(ctx, runID, freeAt, attemptsSpent, pol, freeSource, skipped, logger)
			logger.Warn("runner: run %s met the provider usage window again after a pull-forward probe — retry re-armed for %s (reset source: %s), attempt %d/%d unchanged",
				runID, freeAt.Format(time.RFC3339), freeSource, attemptsSpent, pol.MaxAttempts)
			return usageRetryArmed
		}
		// rearmed == false: the marker is gone (an operator resumed the run,
		// or the sweeper stood the probe down first), so the ordinary arming
		// below is the right one.
	}

	// Below here the arming is PAID, and so is what it reports: a probe's
	// re-failure is the same wall counted once already, and feeding the
	// shared circuit with up to a whole probe budget of self-inflicted
	// refusals would open a breaker every sibling of this workflow revision
	// then waits behind.
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
		// A row whose announced wall is still ahead must not be retired
		// here, whatever this execution spent. The paid arming is refused
		// precisely when the budget is spent, and AbandonRunRetry deletes
		// probe_from together with retry_after, so the wall this run is
		// actually waiting on would be gone for good, days before it falls —
		// a probe must never shorten a run's life, and retiring it is the
		// shortest life there is. Put the instant back free of charge, the
		// way the sweeper's own stand-down does on the two settlement paths
		// it guards. Nothing is charged either way: the charge was REFUSED.
		if announcedFrom != nil {
			announced := announcedFrom.UTC()
			rearmed, rearmErr := retryStore.RearmProbedRunRetry(ctx, runID, announced)
			switch {
			case rearmErr != nil:
				// Settling is irreversible and this one is not owed: hand the
				// delivery back instead, so another pod answers on a row that
				// still holds its marker.
				logger.Error("runner: run %s: the paid arming was refused and the provider's announced instant %s could not be restored (%v) — falling back to redelivery rather than retiring a run whose wall is still ahead",
					runID, announced.Format(time.RFC3339), rearmErr)
				return usageRetryNotApplicable
			case rearmed:
				source = withAnnouncedRestored(source)
				r.noteFreeRearm(ctx, runID, announced, attemptsSpent, pol, source, skipped, logger)
				logger.Warn("runner: run %s: the attempt budget is spent, so the provider's announced instant %s is back and no attempt was spent (reset source: %s) — a probe must never shorten a run's life",
					runID, announced.Format(time.RFC3339), source)
				return usageRetryArmed
			}
			// rearmed == false: the marker is gone (the sweeper stood the
			// probe down, or an operator resumed the run), so this is no
			// longer a probed row and the ordinary settlement is the right
			// one.
		}
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

// noteFreeRearm records a re-arming that spent no attempt: the park counter
// and the timeline event. Shared by the two places that re-arm a probe-marked
// row — the re-failure that follows a probe, and the refused paid arming that
// would otherwise retire such a row — so neither can drift from the other.
//
// The counter matters as much as the event: the sweeper counts a probe's
// resume in iterion_runs_retry_resumed_total, so a park nobody counts makes
// resumed outrun scheduled permanently, and the growing gap between the two
// is the ONE signal that runs are being armed and never resumed.
func (r *Runner) noteFreeRearm(ctx context.Context, runID string, at time.Time, attemptsSpent int, pol retrypolicy.Policy, source string, skipped time.Time, logger *iterlog.Logger) {
	if r.cfg.Metrics != nil {
		r.cfg.Metrics.RunsRetryScheduled.Inc()
	}
	if emitErr := r.emitRetryScheduled(ctx, runID, at, attemptsSpent, pol, source, skipped); emitErr != nil {
		logger.Warn("runner: run %s: could not emit run_retry_scheduled: %v", runID, emitErr)
	}
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

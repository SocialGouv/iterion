package runner

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/cloud/metrics"
	"github.com/SocialGouv/iterion/pkg/credpool"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/retrypolicy"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The capacity guard (ADR-167 C2): a delivery whose message carries the
// places its admission reserved (queue.RunMessage.Capacity) proves the run
// still holds them before it resumes, on every road that resumes without the
// publisher — a redelivery of the run's message, a launch redelivered onto a
// run that already executed, an orphan adopted under the lock.
//
// It never acquires a place. The ledger entry each hold names was written by
// the publication's admission; the runner re-asserts it (store.ReassertCredSlot
// by the admission's identity), which moves the entry an eviction judged
// before would pull. A place that is gone parks the run on CRED_SLOT_WAIT with
// its retry armed (store.CapacityParker), and the retry sweeper's re-admission
// asks the ledger again.
//
// Three stages, each on the run as it is at that moment:
//
//   - delivery: under the run's lock, before anything executes — the transit
//     write, then the re-assertions (store.EnterCapacityTransit), keyed on the
//     status the write finds, never on the message's kind;
//   - pre_engine: just before the engine loads the run, when the clone and the
//     bundle took more than a quarter of the hold — the transit refreshed;
//   - claim: inside the engine's claim, on the document it claims from
//     (runtime.WithResumeClaimGuard), the re-assertions alone: the claim
//     fences on that document's version, so the guard writes nothing to it.
//
// The hold starts at the engine's own park write (runtime.WithOnResumablePark),
// not after the runner's post-engine steps, and every transit a delivery
// starts, that delivery ends unless a nak the queue will redeliver follows
// (endUnlessRedelivered).

// capacityClaimMargin is the least of the hold a claim must still have: a
// claim this close to the hold's end can lose the place between its proof and
// its write.
const capacityClaimMargin = 30 * time.Second

// capacityReassertNakDelay spaces the redeliveries of a delivery the ledger
// could not answer for.
const capacityReassertNakDelay = 30 * time.Second

// capacityStoreTimeout bounds each store write the guard makes, detached from
// the run's context so a drain does not abandon a transit half-written.
const capacityStoreTimeout = 10 * time.Second

// redeliveryHoldTTL is the length of a transit this runner writes
// (Config.RedeliveryHoldTTL, store.DefaultRedeliveryHoldTTL when unset).
func (r *Runner) redeliveryHoldTTL() time.Duration {
	if r.cfg.RedeliveryHoldTTL > 0 {
		return r.cfg.RedeliveryHoldTTL
	}
	return store.DefaultRedeliveryHoldTTL
}

// transitUntil is the end of a hold written at now.
func (r *Runner) transitUntil(now time.Time) time.Time {
	return store.TransitInstant(now.Add(r.redeliveryHoldTTL()))
}

// capacityGuard is one delivery's capacity transit.
type capacityGuard struct {
	r          *Runner
	msg        *queue.RunMessage
	holds      []store.CapacityHold
	delivered  int
	maxDeliver int
	logger     *iterlog.Logger

	mu sync.Mutex
	// owned is the end of the transit this delivery wrote last; zero when it
	// owns none.
	owned time.Time
	// restore is what ending the transit puts back: final when the transit
	// replaced a final continuation, nothing otherwise.
	restore store.ContinuationState
	// held are the holds with their entries' newest instants — what a
	// release names.
	held []store.ReassertedHold
	// provedAt is when the delivery stage proved the holds.
	provedAt time.Time
}

// newCapacityGuard returns the delivery's guard, or nil when there is nothing
// to guard: a message from a publisher older than the guard (counted legacy,
// resumed as before) or an admission that reserved nothing.
func (r *Runner) newCapacityGuard(msg *queue.RunMessage, delivered int, logger *iterlog.Logger) *capacityGuard {
	if msg.Capacity == nil {
		r.countCapacity(metrics.CapacityStageDelivery, metrics.CapacityLegacy)
		return nil
	}
	if len(msg.Capacity.Holds) == 0 {
		return nil
	}
	return &capacityGuard{
		r: r, msg: msg, holds: msg.Capacity.Holds,
		delivered: delivered, maxDeliver: r.maxDeliver(), logger: logger,
	}
}

// capacityAtPickup is the guard at a delivery's pickup, under the run's lock
// and before anything executes: the delivery's guard (nil when its message
// carries nothing to guard) and, when the delivery does not proceed, its
// disposition. It runs on every guarded delivery — a launch message
// redelivered onto a run that executed as much as a resume message — and
// keys on the status the transit write finds, never on the message's kind.
func (r *Runner) capacityAtPickup(ctx context.Context, msg *queue.RunMessage, delivered int, logger *iterlog.Logger) (*capacityGuard, preconditionOutcome, bool) {
	g := r.newCapacityGuard(msg, delivered, logger)
	if g == nil {
		return nil, preconditionOutcome{proceed: true}, true
	}
	out, proceed := g.deliveryStage(ctx)
	return g, out, proceed
}

// countCapacity increments the re-assertion counter once per subject named
// (a legacy count names none).
func (r *Runner) countCapacity(stage, outcome string, subjects ...string) {
	if r.cfg.Metrics == nil || r.cfg.Metrics.CapacityReassertTotal == nil {
		return
	}
	if len(subjects) == 0 {
		r.cfg.Metrics.CapacityReassertTotal.WithLabelValues(stage, outcome, "none").Inc()
		return
	}
	for _, s := range subjects {
		kind := metrics.CapacityKindKey
		if _, gw := store.GatewayHostOfSubject(s); gw {
			kind = metrics.CapacityKindGateway
		}
		r.cfg.Metrics.CapacityReassertTotal.WithLabelValues(stage, outcome, kind).Inc()
	}
}

func (g *capacityGuard) subjects() []string {
	out := make([]string, 0, len(g.holds))
	for _, h := range g.holds {
		out = append(out, h.Subject)
	}
	return out
}

// lastDelivery reports that the queue will not redeliver after this one.
func (g *capacityGuard) lastDelivery() bool {
	return g.maxDeliver > 0 && g.delivered >= g.maxDeliver
}

// storeCtx is a detached, bounded context carrying the run's identity.
func (g *capacityGuard) storeCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), capacityStoreTimeout)
	return store.WithIdentity(bg, g.msg.TenantID, g.msg.OwnerID), cancel
}

// own records a transit this delivery wrote. restoreFrom, when set, is the
// continuation the write replaced (the first write of the delivery decides
// what an end puts back).
func (g *capacityGuard) own(until time.Time, restoreFrom *store.ContinuationState) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.owned = until
	if restoreFrom != nil {
		g.restore = ""
		if *restoreFrom == store.ContinuationFinal {
			g.restore = store.ContinuationFinal
		}
	}
}

func (g *capacityGuard) setHeld(held []store.ReassertedHold) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.held = held
}

func (g *capacityGuard) ownedTransit() (time.Time, store.ContinuationState, []store.ReassertedHold) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.owned, g.restore, g.held
}

// disown forgets the transit: a park or an end wrote past it.
func (g *capacityGuard) disown() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.owned, g.held = time.Time{}, nil
}

// stores are the two capabilities the guard writes through.
func (g *capacityGuard) stores() (store.CapacityTransitStore, store.CredSlotStore, error) {
	transit := store.AsCapacityTransitStore(g.r.cfg.Store)
	ledger := store.AsCredSlotStore(g.r.cfg.Store)
	if transit == nil || ledger == nil {
		return nil, nil, fmt.Errorf("runner: run %s carries capacity holds, but this run store hosts no capacity transit or no run-admission ledger", g.msg.RunID)
	}
	return transit, ledger, nil
}

// enter writes the transit and re-asserts the holds (store.EnterCapacityTransit).
func (g *capacityGuard) enter(ctx context.Context, expect time.Time) (store.CapacityTransitEntry, time.Time, error) {
	transit, ledger, err := g.stores()
	if err != nil {
		return store.CapacityTransitEntry{}, time.Time{}, err
	}
	sctx, cancel := g.storeCtx(ctx)
	defer cancel()
	until := g.r.transitUntil(time.Now())
	entry, err := store.EnterCapacityTransit(sctx, transit, ledger, g.msg.RunID, g.holds, store.CapacityTransit{
		Until: until, ExpectUntil: expect, ExactQueuedAt: codemodeQuotaQueuedAt(g.msg),
	})
	return entry, until, err
}

// deliveryStage is the guard at pickup, under the run's lock and before
// anything executes. proceed false: the returned outcome is the delivery's
// disposition, every write it needs already made.
func (g *capacityGuard) deliveryStage(ctx context.Context) (preconditionOutcome, bool) {
	entry, until, err := g.enter(ctx, time.Time{})
	if entry.Started {
		g.own(until, &entry.Prior)
	}
	switch {
	case err != nil:
		g.r.countCapacity(metrics.CapacityStageDelivery, metrics.CapacityError, g.subjects()...)
		return g.ledgerUnavailable(ctx, err), false
	case !entry.Started:
		return g.notInTransit(ctx)
	case entry.Lost != "":
		g.r.countCapacity(metrics.CapacityStageDelivery, metrics.CapacityMissing, entry.Lost)
		status := g.parkLostPlace(ctx, entry.Lost)
		g.closeUnexecutedPoolLease()
		return preconditionOutcome{
			finalStatus: status,
			op:          "ack-capacity-place-lost",
			action:      actionAck,
			level:       logWarn,
			logFmt:      "runner: run %s lost its capacity place on %s while the queue redelivered it — parked on CRED_SLOT_WAIT for the retry sweeper (%s), NOT resumed",
			logArgs:     []any{g.msg.RunID, entry.Lost, status},
		}, false
	}
	g.setHeld(entry.Held)
	g.mu.Lock()
	g.provedAt = time.Now()
	g.mu.Unlock()
	g.r.countCapacity(metrics.CapacityStageDelivery, metrics.CapacityHeld, g.subjects()...)
	return preconditionOutcome{proceed: true}, true
}

// notInTransit decides a delivery whose transit write matched nothing, on the
// run as it reads now: queued (a publication's own attempt, its admission
// decided by the publisher) and running (an owner the engine tells apart)
// proceed with no transit; anything else takes the admission rule — an armed
// retry, an operator pause holding capacity, a cancel, a settled run are
// dropped. A failed_resumable run the rule would resume is re-offered: the
// write did not match it, so its places are not proven.
func (g *capacityGuard) notInTransit(ctx context.Context) (preconditionOutcome, bool) {
	lctx, cancel := g.storeCtx(ctx)
	run, err := g.r.cfg.Store.LoadRun(lctx, g.msg.RunID)
	cancel()
	if err != nil || run == nil {
		return preconditionOutcome{
			finalStatus: "store_load_transient",
			op:          "nak-store-load-transient",
			action:      actionNak,
			level:       logWarn,
			logFmt:      "runner: run %s: re-read after its capacity transit did not begin failed (%v) — naking",
			logArgs:     []any{g.msg.RunID, err},
		}, false
	}
	if run.Status.CanStartExecution() {
		return preconditionOutcome{proceed: true}, true
	}
	out := dispositionForStatus(g.msg, run)
	if !out.proceed {
		return out, false
	}
	return preconditionOutcome{
		finalStatus: "capacity_transit_refused",
		op:          "nak-capacity-transit-refused",
		action:      actionNakDelayed,
		delay:       capacityReassertNakDelay,
		level:       logWarn,
		logFmt:      "runner: run %s reads %s (continuation %q) but its capacity transit did not begin — its places are not proven, re-offering in %s",
		logArgs:     []any{g.msg.RunID, run.Status, run.ContinuationState, capacityReassertNakDelay},
	}, false
}

// ledgerUnavailable is the disposition of a delivery whose places the ledger
// could not prove: re-offered after a delay while the queue redelivers, parked
// on its last delivery — it never resumes unproven.
func (g *capacityGuard) ledgerUnavailable(ctx context.Context, cause error) preconditionOutcome {
	if g.lastDelivery() {
		status := g.parkLostPlace(ctx, "")
		g.closeUnexecutedPoolLease()
		return preconditionOutcome{
			finalStatus: status,
			op:          "ack-capacity-ledger-last-delivery",
			action:      actionAck,
			level:       logError,
			logFmt:      "runner: run %s: the run-admission ledger could not prove its capacity places on the last permitted delivery (%v) — parked for the retry sweeper (%s), NOT resumed",
			logArgs:     []any{g.msg.RunID, cause, status},
		}
	}
	return preconditionOutcome{
		finalStatus: "capacity_ledger_unavailable",
		op:          "nak-capacity-ledger-unavailable",
		action:      actionNakDelayed,
		delay:       capacityReassertNakDelay,
		level:       logWarn,
		logFmt:      "runner: run %s: the run-admission ledger could not prove its capacity places (%v) — re-offering in %s, NOT resuming unproven",
		logArgs:     []any{g.msg.RunID, cause, capacityReassertNakDelay},
	}
}

// preEngineStage refreshes the transit just before the engine loads the run,
// when the clone and the bundle took more than a quarter of the hold since
// the delivery stage proved it. A refusal is returned before the engine runs.
func (g *capacityGuard) preEngineStage(ctx context.Context) error {
	owned, _, _ := g.ownedTransit()
	g.mu.Lock()
	provedAt := g.provedAt
	g.mu.Unlock()
	if owned.IsZero() || provedAt.IsZero() || time.Since(provedAt) <= g.r.redeliveryHoldTTL()/4 {
		return nil
	}
	entry, until, err := g.enter(ctx, owned)
	if entry.Started {
		g.own(until, nil)
	}
	switch {
	case err != nil:
		g.r.countCapacity(metrics.CapacityStagePreEngine, metrics.CapacityError, g.subjects()...)
		return fmt.Errorf("%w: run %s: refresh before the engine: %v", store.ErrCapacityLedgerUnavailable, g.msg.RunID, err)
	case !entry.Started:
		g.r.countCapacity(metrics.CapacityStagePreEngine, metrics.CapacityExpired, g.subjects()...)
		return fmt.Errorf("%w: run %s: its capacity transit moved before the engine loaded it", store.ErrCapacityLedgerUnavailable, g.msg.RunID)
	case entry.Lost != "":
		g.r.countCapacity(metrics.CapacityStagePreEngine, metrics.CapacityMissing, entry.Lost)
		return &capacityPlaceLost{runID: g.msg.RunID, subject: entry.Lost, stage: metrics.CapacityStagePreEngine}
	}
	g.setHeld(entry.Held)
	g.mu.Lock()
	g.provedAt = time.Now()
	g.mu.Unlock()
	g.r.countCapacity(metrics.CapacityStagePreEngine, metrics.CapacityHeld, g.subjects()...)
	return nil
}

// capacityBeforeEngine is the pre_engine stage of the delivery's guard, when
// it has one (preEngineStage).
func (r *Runner) capacityBeforeEngine(ctx context.Context) error {
	if g := capacityGuardFrom(ctx); g != nil {
		return g.preEngineStage(ctx)
	}
	return nil
}

// claimGuard is the engine's claim guard (runtime.WithResumeClaimGuard): on
// the document the engine is about to claim from. A queued run is its
// publication's, decided by the publisher's admission. Any other — the
// parked statuses a redelivery claims from, or one the run moved to since the
// delivery stage — must still be in its transit, far enough from the hold's
// end, and every hold still held for this admission. It writes nothing to the
// run document: the claims fence on its version.
func (g *capacityGuard) claimGuard(ctx context.Context, run *store.Run) error {
	if run.Status.IsQueued() {
		return nil
	}
	now := time.Now()
	if run.ContinuationState != store.ContinuationRedeliveryPending || run.ContinuationUntil == nil || run.ContinuationUntil.Sub(now) < capacityClaimMargin {
		g.r.countCapacity(metrics.CapacityStageClaim, metrics.CapacityExpired, g.subjects()...)
		return fmt.Errorf("%w: run %s reads %s with continuation %q until %v — its capacity hold lapsed or is within %s of lapsing before the claim", store.ErrCapacityLedgerUnavailable, run.ID, run.Status, run.ContinuationState, run.ContinuationUntil, capacityClaimMargin)
	}
	_, ledger, err := g.stores()
	if err != nil {
		return fmt.Errorf("%w: %v", store.ErrCapacityLedgerUnavailable, err)
	}
	sctx, cancel := g.storeCtx(ctx)
	defer cancel()
	held, lost, err := store.ReassertCapacityHolds(sctx, ledger, run.ID, g.holds)
	switch {
	case err != nil:
		g.r.countCapacity(metrics.CapacityStageClaim, metrics.CapacityError, g.subjects()...)
		return fmt.Errorf("%w: run %s: %v", store.ErrCapacityLedgerUnavailable, run.ID, err)
	case lost != "":
		g.r.countCapacity(metrics.CapacityStageClaim, metrics.CapacityMissing, lost)
		g.setHeld(nil)
		return &capacityPlaceLost{runID: run.ID, subject: lost, stage: metrics.CapacityStageClaim}
	}
	g.setHeld(held)
	g.r.countCapacity(metrics.CapacityStageClaim, metrics.CapacityHeld, g.subjects()...)
	return nil
}

// onResumablePark is the engine's park hook (runtime.WithOnResumablePark): an
// interruption the queue will redeliver starts the transit at the engine's own
// park write, so the run holds its places through the runner's post-engine
// steps. The post-engine promote writes it again and stays the authority.
func (g *capacityGuard) onResumablePark(ctx context.Context, code store.FailureCode) {
	if code != store.FailureInterrupted || g.maxDeliver <= 0 || g.delivered >= g.maxDeliver {
		return
	}
	sctx, cancel := g.storeCtx(ctx)
	defer cancel()
	until := g.r.transitUntil(time.Now())
	changed, err := g.r.cfg.Store.UpdateRunOutcome(sctx, g.msg.RunID, store.RunStatusFailedResumable, "",
		store.RunOutcomeMeta{Continuation: store.ContinuationRedeliveryPending, ContinuationUntil: until, ExactQueuedAt: codemodeQuotaQueuedAt(g.msg)},
		[]store.RunStatus{store.RunStatusFailedResumable})
	if err != nil {
		g.logger.Warn("runner: run %s: start its capacity transit at the engine's park: %v — the post-engine promote writes it", g.msg.RunID, err)
		return
	}
	if changed {
		// The engine's park cleared every continuation: an end of this
		// transit puts nothing back.
		none := store.ContinuationState("")
		g.own(until, &none)
	}
}

// afterEngine decides a delivery whose execution returned a capacity refusal
// (stage pre_engine or claim): the run is parked when its place is gone, or
// on the last delivery; re-offered after a delay when the ledger did not
// answer. handled false: the error is not the guard's.
func (g *capacityGuard) afterEngine(ctx context.Context, err error, delivery jsDelivery, usage *metricsEmitter) (string, bool) {
	lost := errors.Is(err, store.ErrCapacityPlaceLost)
	unavailable := errors.Is(err, store.ErrCapacityLedgerUnavailable)
	if !lost && !unavailable {
		return "", false
	}
	if unavailable && !g.lastDelivery() {
		g.reportPoolSpend(usage, err, true)
		g.logger.Warn("runner: run %s: %v — re-offering in %s, NOT resuming unproven", g.msg.RunID, err, capacityReassertNakDelay)
		dispatchDelivery(g.logger, delivery, actionNakDelayed, capacityReassertNakDelay, "nak-capacity-ledger-unavailable", g.msg.RunID)
		return "capacity_ledger_unavailable", true
	}
	lostSubject := ""
	var placeLost *capacityPlaceLost
	if errors.As(err, &placeLost) {
		lostSubject = placeLost.subject
	}
	status := g.parkLostPlace(ctx, lostSubject)
	g.reportPoolSpend(usage, err, false)
	g.logger.Warn("runner: run %s: %v — parked on CRED_SLOT_WAIT for the retry sweeper (%s), NOT resumed", g.msg.RunID, err, status)
	dispatchDelivery(g.logger, delivery, actionAck, 0, "ack-capacity-place-lost", g.msg.RunID)
	return status, true
}

// capacityPlaceLost is a stage's refusal when a hold is no longer held: it
// names the subject, and is store.ErrCapacityPlaceLost.
type capacityPlaceLost struct {
	runID, subject, stage string
}

func (e *capacityPlaceLost) Error() string {
	return fmt.Sprintf("%v: run %s: the place on %s was taken (stage %s)", store.ErrCapacityPlaceLost, e.runID, e.subject, e.stage)
}

func (e *capacityPlaceLost) Unwrap() error { return store.ErrCapacityPlaceLost }

// reportPoolSpend closes (or, interim, keeps) the delivery's credential-pool
// lease: through the attempt's usage once the engine ran, with a zero outcome
// when it never did.
func (g *capacityGuard) reportPoolSpend(usage *metricsEmitter, err error, interim bool) {
	if usage != nil {
		g.r.recordPoolSpend(g.msg, usage, err, interim)
		return
	}
	if !interim {
		g.closeUnexecutedPoolLease()
	}
}

// closeUnexecutedPoolLease closes the pool lease of a delivery that executed
// nothing and will not be redelivered: a zero outcome, final. Without it the
// donor's slot and committed allowance stay held to the lease's TTL.
func (g *capacityGuard) closeUnexecutedPoolLease() {
	if g.r.cfg.CredPool == nil || g.msg.PoolGrantless {
		return
	}
	publishedAt, perr := time.Parse(time.RFC3339Nano, g.msg.PublishedAtRFC)
	if perr != nil {
		publishedAt = time.Time{}
	}
	bg, cancel := context.WithTimeout(context.Background(), spendWriteTimeout)
	defer cancel()
	if err := g.r.cfg.CredPool.ReportAttemptID(bg, g.msg.RunID, g.msg.PoolAttemptID, publishedAt, credpool.Outcome{Interim: false}); err != nil {
		g.logger.Warn("runner: run %s: close the credential-pool lease of a delivery that executed nothing: %v", g.msg.RunID, err)
	}
}

// freeCapacityParkSource reports whether a park on capacity is free for a run
// that came back from code: an infrastructure cause the run did not choose
// (an interruption, an orphan, a sandbox that would not start or be placed,
// attachments the store would not hand over). Any other source spends one
// attempt of the run's retry budget, so a run that fails and loses its place
// in a loop stops at its max_attempts.
func freeCapacityParkSource(code store.FailureCode) bool {
	switch code {
	case store.FailureInterrupted, store.FailureProcessOrphaned, store.FailureSandboxSetupTimeout,
		store.FailureSandboxCapacity, store.FailureAttachmentsUnfetchable:
		return true
	}
	return false
}

// parkLostPlace parks the run in this delivery's transit on CRED_SLOT_WAIT,
// its retry armed, in one write (store.CapacityParker) — with the program of
// a started run frozen for its re-admission (freezeProgram) — and reports the
// delivery's final status. lost names the subject whose place is gone ("" when
// the ledger could not answer on the last delivery). A park the store cannot
// arm, or a charged park over a spent budget, ends the transit as final with
// the cause said; a run that moved past the transit is left to whoever moved
// it.
func (g *capacityGuard) parkLostPlace(ctx context.Context, lost string) string {
	owned, _, _ := g.ownedTransit()
	sctx, cancel := g.storeCtx(ctx)
	defer cancel()
	if owned.IsZero() {
		// No transit of this delivery's to park from: the queue hands nothing
		// back, so the run's continuation says nobody resumes it — never a
		// redelivery_pending no redelivery honours.
		if _, err := g.r.cfg.Store.UpdateRunOutcome(sctx, g.msg.RunID, store.RunStatusFailedResumable, "",
			store.RunOutcomeMeta{Continuation: store.ContinuationFinal, ExactQueuedAt: codemodeQuotaQueuedAt(g.msg)},
			[]store.RunStatus{store.RunStatusFailedResumable}); err != nil {
			g.logger.Error("runner: run %s: mark the continuation final after an unproven last delivery: %v", g.msg.RunID, err)
		}
		g.logger.Error("runner: run %s: its capacity places could not be proven on its last delivery and this delivery wrote no transit to park it from — left failed_resumable with no continuation, NOT resumed: resume it by hand", g.msg.RunID)
		return "capacity_place_lost_unowned"
	}
	run, err := g.r.cfg.Store.LoadRun(sctx, g.msg.RunID)
	if err != nil {
		g.logger.Error("runner: run %s: read the run to park it on capacity: %v — its transit ends at %s, NOT resumed", g.msg.RunID, err, owned.Format(time.RFC3339))
		return "capacity_park_unreadable"
	}
	maxAttempts := retrypolicy.DefaultMaxAttempts
	if run.RetryPolicy != nil && run.RetryPolicy.MaxAttempts > 0 {
		maxAttempts = run.RetryPolicy.MaxAttempts
	}
	charge := !freeCapacityParkSource(run.FailureCode)
	what := "its capacity places could not be proven on its last delivery"
	if lost != "" {
		what = fmt.Sprintf("the capacity place its admission reserved on %s was taken while the queue redelivered it", lost)
	}
	parker := store.AsCapacityParker(g.r.cfg.Store)
	if parker == nil {
		g.endFinal(sctx, owned, fmt.Sprintf("parked: %s — this run store hosts no durable retry, so nothing resumes the run by itself: resume it once a slot frees", what))
		return "capacity_place_lost_unarmed"
	}
	// A started run keeps the program it ran: the retry sweeper's
	// re-admission republishes it, as this redelivery would have. Frozen
	// before the park write, whose record names it; a run that never started
	// freezes nothing and is re-admitted on its bot's current program.
	started := run.ProgramStarted()
	var frozen *store.FrozenProgram
	if started {
		frozen = g.freezeProgram(ctx, run)
	}
	retryAfter := time.Now().UTC().Add(store.CredSlotProbeInterval)
	parked, attempt, err := parker.ParkRunOnCapacity(sctx, g.msg.RunID, store.CapacityPark{
		From:         store.RunStatusFailedResumable,
		TransitUntil: owned,
		Code:         store.FailureCredSlotWait,
		Error:        fmt.Sprintf("parked: %s — the run resumes by itself once a slot frees (checked every %s, up to the run's max_wait)", what, store.CredSlotProbeInterval),
		Reason:       store.RetryReasonCredSlot,
		RetryAfter:   retryAfter,
		Charge:       charge,
		MaxAttempts:  maxAttempts,
		Frozen:       frozen,
	})
	if err != nil {
		g.endFinal(sctx, owned, fmt.Sprintf("%s, and the park could not be written (%v) — resume it by hand", what, err))
		return "capacity_park_failed"
	}
	if parked {
		g.disown()
		switch {
		case frozen != nil:
			g.r.countFrozenProgram(metrics.FrozenProgramWritten)
		case !started:
			g.r.countFrozenProgram(metrics.FrozenProgramNotStarted)
		}
		if g.r.cfg.Metrics != nil {
			g.r.cfg.Metrics.RunsRetryScheduled.Inc()
		}
		g.emitCapacityRetryScheduled(sctx, retryAfter, attempt, maxAttempts, charge, lost)
		return "capacity_parked"
	}
	if charge {
		after, rerr := g.r.cfg.Store.LoadRun(sctx, g.msg.RunID)
		if rerr == nil && after.Status == store.RunStatusFailedResumable && after.ContinuationState == store.ContinuationRedeliveryPending &&
			after.ContinuationUntil != nil && after.ContinuationUntil.Equal(owned) {
			g.endFinal(sctx, owned, fmt.Sprintf("%s, and its retry budget is spent (%d attempts) — place lost, retry budget spent: resume it by hand", what, maxAttempts))
			return "capacity_place_lost_budget_spent"
		}
	}
	g.disown()
	g.logger.Warn("runner: run %s moved past its capacity transit before the park — left to whoever moved it", g.msg.RunID)
	return "capacity_place_lost_moved"
}

// capacityFreezeTimeout bounds the freeze of a parked delivery's program: a
// fetch of an out-of-band IR, a compile of its source snapshot, two blob
// writes.
const capacityFreezeTimeout = 30 * time.Second

// freezeProgram freezes the program this delivery carried for the park of
// run, a run that started (runview.FreezeProgram), and returns the record the
// park writes. Any failure parks the run without one, said loudly and
// counted: the park still lands — no nak, no longer transit — and the
// re-admission resumes the bot's current program, refused if it changed.
func (g *capacityGuard) freezeProgram(ctx context.Context, run *store.Run) *store.FrozenProgram {
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), capacityFreezeTimeout)
	defer cancel()
	fctx = store.WithIdentity(fctx, g.msg.TenantID, g.msg.OwnerID)
	fp, err := runview.FreezeProgram(fctx, store.AsIRBlobStore(g.r.cfg.Store), g.msg, run, store.FrozenProgramSourceCapacity)
	if err != nil {
		outcome := metrics.FrozenProgramFreezeFailed
		if errors.Is(err, runview.ErrFreezeHashMismatch) {
			outcome = metrics.FrozenProgramHashMismatch
		}
		g.r.countFrozenProgram(outcome)
		g.logger.Error("runner: run %s started, lost its capacity place, and its program could not be frozen (%v) — parked WITHOUT it: its re-admission resumes the bot's current program, and is refused if that program changed", g.msg.RunID, err)
		return nil
	}
	return fp
}

// countFrozenProgram counts one capacity park's freeze outcome.
func (r *Runner) countFrozenProgram(outcome string) {
	if r.cfg.Metrics == nil || r.cfg.Metrics.CapacityFrozenProgramTotal == nil {
		return
	}
	r.cfg.Metrics.CapacityFrozenProgramTotal.WithLabelValues(store.FrozenProgramSourceCapacity, outcome).Inc()
}

// endFinal ends the owned transit as final — nobody resumes the run by itself
// — and says why on the run's timeline and in the log.
func (g *capacityGuard) endFinal(ctx context.Context, owned time.Time, why string) {
	transit := store.AsCapacityTransitStore(g.r.cfg.Store)
	if transit != nil {
		if _, err := transit.EndCapacityTransit(ctx, g.msg.RunID, owned, store.ContinuationFinal); err != nil {
			g.logger.Error("runner: run %s: end its capacity transit as final: %v — the hold lapses at %s", g.msg.RunID, err, owned.Format(time.RFC3339))
		}
	}
	g.disown()
	g.logger.Error("runner: run %s: %s", g.msg.RunID, why)
	if _, err := g.r.cfg.Store.AppendEvent(ctx, g.msg.RunID, store.Event{
		Type: store.EventRunRetrySkipped,
		Data: map[string]any{
			"reason": "capacity_place_lost",
			"code":   string(store.FailureCredSlotWait),
			"error":  why,
			"hint":   "resume the run once a slot of its capped key or gateway frees",
		},
	}); err != nil {
		g.logger.Warn("runner: run %s: could not emit run_retry_skipped: %v", g.msg.RunID, err)
	}
}

// emitCapacityRetryScheduled records the armed retry on the run's timeline.
func (g *capacityGuard) emitCapacityRetryScheduled(ctx context.Context, at time.Time, attempt, maxAttempts int, charged bool, lost string) {
	data := map[string]any{
		"code":         string(store.FailureCredSlotWait),
		"reason":       store.RetryReasonCredSlot,
		"retry_after":  at.Format(time.RFC3339),
		"attempt":      attempt,
		"max_attempts": maxAttempts,
		"charged":      charged,
	}
	if lost != "" {
		data["subject"] = lost
	}
	if _, err := g.r.cfg.Store.AppendEvent(ctx, g.msg.RunID, store.Event{Type: store.EventRunRetryScheduled, Data: data}); err != nil {
		g.logger.Warn("runner: run %s: could not emit run_retry_scheduled: %v", g.msg.RunID, err)
	}
}

// endUnlessRedelivered ends the transit this delivery owns when the delivery
// resumed nothing and the queue will not hand it back: the continuation it
// replaced put back, its re-asserted holds given back by their newest
// instants. A claim moved the run out of the transit's filter and is left
// alone with its places; so is anything else that moved it.
func (g *capacityGuard) endUnlessRedelivered(redelivered bool) {
	owned, restore, held := g.ownedTransit()
	if redelivered || owned.IsZero() {
		return
	}
	transit, ledger, err := g.stores()
	if err != nil {
		g.logger.Warn("runner: run %s: %v — its capacity transit lapses at %s", g.msg.RunID, err, owned.Format(time.RFC3339))
		return
	}
	ctx, cancel := g.storeCtx(context.Background())
	defer cancel()
	ended, err := transit.EndCapacityTransit(ctx, g.msg.RunID, owned, restore)
	if err != nil {
		g.logger.Warn("runner: run %s: end the capacity transit of a delivery that resumed nothing: %v — it lapses at %s", g.msg.RunID, err, owned.Format(time.RFC3339))
		return
	}
	g.disown()
	outcome := metrics.CapacityTransitClaimedOrMoved
	if ended {
		outcome = metrics.CapacityTransitRestored
		if err := store.ReleaseReassertedHolds(ctx, ledger, g.msg.RunID, held); err != nil {
			g.logger.Warn("runner: run %s: give back the capacity places of a delivery that resumed nothing: %v — the ledger's reconcile evicts them", g.msg.RunID, err)
		}
	}
	if g.r.cfg.Metrics != nil && g.r.cfg.Metrics.CapacityTransitEndTotal != nil {
		g.r.cfg.Metrics.CapacityTransitEndTotal.WithLabelValues(outcome).Inc()
	}
}

// capacityGuardKey carries a delivery's guard to executeRun.
type capacityGuardKey struct{}

func withCapacityGuard(ctx context.Context, g *capacityGuard) context.Context {
	if g == nil {
		return ctx
	}
	return context.WithValue(ctx, capacityGuardKey{}, g)
}

func capacityGuardFrom(ctx context.Context) *capacityGuard {
	g, _ := ctx.Value(capacityGuardKey{}).(*capacityGuard)
	return g
}

// nakRecorder notes whether a delivery was handed back to the queue, whichever
// exit of the delivery's handling did it: the transit's end reads it.
type nakRecorder struct {
	jsDelivery
	nakked bool
}

func (d *nakRecorder) Nak() error { d.nakked = true; return d.jsDelivery.Nak() }

func (d *nakRecorder) NakWithDelay(delay time.Duration) error {
	d.nakked = true
	return d.jsDelivery.NakWithDelay(delay)
}

package runner

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/errtrack"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	natsq "github.com/SocialGouv/iterion/pkg/queue/nats"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// acquireRunLock claims the distributed run lock guarding against two
// runners executing the same run. Any failure to take it — ErrLockHeld
// (a sibling demonstrably has it) or a lock store that could not answer —
// is retried with a delay while attempts remain, then archived on
// exhaustion. No branch here changes the run: without the lock this pod
// is not entitled to write its outcome or its continuation, whether or
// not somebody else turns out to own it. Returns (lock, true, "") on
// success; (nil, false, finalStatus) when the caller must abandon the
// delivery (finalStatus is the metric label).
func (r *Runner) acquireRunLock(runCtx context.Context, msg *queue.RunMessage, delivery jsDelivery, logger *iterlog.Logger) (store.RunLock, bool, string) {
	// Acquire the distributed lock. Two competing runners on the
	// same run is the contention this guards against.
	lock, err := r.cfg.Store.LockRun(runCtx, msg.RunID)
	if err != nil {
		// AcquireLock maps ONLY jetstream.ErrKeyExists to ErrLockHeld, so
		// held is CONFIRMED contention; every other lock error (KV bucket
		// missing, marshal failure, a network blip on the Create) leaves
		// ownership unknown — a sibling may hold the lease and its collision
		// simply never got reported. Classify once here so the metric label
		// and the archived reason cannot drift apart.
		held := errors.Is(err, natsq.ErrLockHeld)
		status := "failed"
		if held {
			status = "lock_held"
		}
		// The same classification picks the log LEVEL, because pkg/log
		// dispatches its hook at warn+ but errtrack.LogHook turns an ERROR
		// line into a tracker event and a WARN line into a mere breadcrumb.
		// A sibling holding the lease is expected traffic on a healthy fleet;
		// a lock store that cannot answer is an infrastructure failure, and
		// this is the only line either class logs on the deferral branch.
		if held {
			logger.Warn("runner: lock held for %s: %v", msg.RunID, err)
		} else {
			logger.Error("runner: lock %s: %v", msg.RunID, err)
		}
		if max := r.maxDeliver(); max > 0 && delivery.NumDelivered() >= max {
			r.archiveLockFailure(msg, delivery, logger, err, held)
		} else {
			delay := natsq.DefaultLockTTL
			if r.cfg.NATS != nil {
				delay = r.cfg.NATS.LockTTL()
			}
			if held {
				delay = natsq.HeldLockRetryDelay(delay, LeaseUnwindCeiling, r.maxDeliver(), delivery.NumDelivered())
			}
			logDeliveryErr(logger, "nak-lock-deferred", msg.RunID, delivery.NakWithDelay(delay))
		}
		return nil, false, status
	}
	return lock, true, ""
}

// leaseRefresher is a run lock whose lease expires unless refreshed: the
// NATS KV lock.
type leaseRefresher interface {
	Refresh(ctx context.Context) error
}

// progressReporter is a delivery whose ack deadline a heartbeat holds open.
type progressReporter interface {
	InProgress() error
}

// executeHoldingLease executes a delivery's run with its lease held by the
// heartbeat, and ends that hold before returning — before the caller Acks or
// Naks the delivery, so no InProgress() from the heartbeat lands after it.
//
// On refresh failure the heartbeat cancels runCtx WITH the interrupted cause
// so the engine unwinds to failed_resumable — better to lose progress than to
// let the lease expire while the engine is still writing to the store (which
// would invite split-brain when JetStream redelivers to a sibling pod). The
// cause makes the redelivery auto-resume without manual intervention.
func (r *Runner) executeHoldingLease(runCtx context.Context, runCancel context.CancelCauseFunc, msg *queue.RunMessage, preRun *store.Run, lock store.RunLock, delivery progressReporter, usageOut **metricsEmitter) error {
	hold := r.startLeaseHeartbeat(runCtx, runCancel, msg.RunID, lock, delivery)
	// nil cause: the run has already returned here, so this is teardown — the
	// engine never reads the cause. Also the panic net.
	defer func() {
		runCancel(nil)
		hold.stop()
	}()
	// Stamped under the lock, before any work: the pair (launcher build,
	// runner build) is what makes a version skew readable from the run
	// itself, and an IR that will not load must not be the first place an
	// operator learns of one.
	r.recordRunnerBuild(runCtx, msg, preRun)
	return r.executeRun(runCtx, msg, usageOut, hold.engineReturned)
}

// leaseHold is a run's lease held by its heartbeat.
type leaseHold struct {
	// engineReturned hands the hold over from the engine to the runner's own
	// post-engine steps: from then on it lasts at most postEngineCeiling,
	// cancelled run or not. Idempotent.
	engineReturned func()
	// stop ends the hold and waits for the heartbeat to exit. Idempotent.
	stop func()
}

// startLeaseHeartbeat refreshes the run's lease for as long as the run needs
// it — its engine's teardown and the runner's post-engine steps included —
// and no longer than LeaseUnwindCeiling once the run can be resumed. A run
// that is cancelled (a drain, an operator's cancel) or parks (a human gate, a
// failed_resumable death) still unwinds: the engine exports the workspace and
// banks its scratch, then the runner records the git snapshot, banks the
// work and uploads the artifacts. Held through that, the lease keeps a resume
// or a redelivery on the lock until what it reads is written.
//
// Two ceilings bound the hold, one per phase: engineUnwindCeiling from the
// run's cancellation to the engine's return, postEngineCeiling from the
// engine's return — which a park reaches without any cancellation. At either
// ceiling the lease is let go as when a refresh fails: no longer refreshed,
// and runCtx cancelled as interrupted, so what still works under it stops
// rather than writing past it. A refresh that fails still cancels runCtx
// (heartbeat).
func (r *Runner) startLeaseHeartbeat(runCtx context.Context, runCancel context.CancelCauseFunc, runID string, lock store.RunLock, delivery progressReporter) leaseHold {
	hbCtx, hbCancel := context.WithCancel(context.WithoutCancel(runCtx))
	release := func(why string) {
		r.cfg.Logger.Warn("runner: run %s %s — its lease is no longer held", runID, why)
		hbCancel()
		runCancel(runtime.ErrRunInterrupted)
	}
	returned := make(chan struct{})
	stopUnwindCeiling := context.AfterFunc(runCtx, func() {
		awaitCeiling(hbCtx, returned, engineUnwindCeiling, func() {
			release(fmt.Sprintf("still unwinds %s after its cancellation", engineUnwindCeiling))
		})
	})
	var once sync.Once
	engineReturned := func() {
		once.Do(func() {
			close(returned)
			errtrack.Go("runner.postEngineCeiling", func() {
				awaitCeiling(hbCtx, nil, postEngineCeiling, func() {
					release(fmt.Sprintf("still works %s after its engine returned", postEngineCeiling))
				})
			})
		})
	}
	done := make(chan struct{})
	errtrack.Go("runner.heartbeat", func() { r.heartbeat(hbCtx, runCancel, lock, delivery, done) })
	return leaseHold{
		engineReturned: engineReturned,
		stop: func() {
			stopUnwindCeiling()
			hbCancel()
			<-done
		},
	}
}

// awaitCeiling calls expire once ceiling has passed — unless the hold ended
// first (ctx), or the phase the ceiling bounds did (over).
func awaitCeiling(ctx context.Context, over <-chan struct{}, ceiling time.Duration, expire func()) {
	t := time.NewTimer(ceiling)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-over:
	case <-t.C:
		expire()
	}
}

// LeaseUnwindCeiling bounds how long a run's lease is still held once the run
// can be resumed — from its cancellation, or from the park its engine wrote:
// the engine's unwind, then the runner's post-engine steps, each phase within
// its own ceiling. A resume that meets the held lease spreads its retries over
// it (natsq.HeldLockRetryDelay) and the orphan sweeper's cutoff counts them
// (natsq.Conn.RedeliveryWindow): the server and the runner both hand it to
// their queue connection.
const LeaseUnwindCeiling = engineUnwindCeiling + postEngineCeiling

// engineUnwindCeiling bounds the engine from the run's cancellation to its
// return: the sandbox's teardown, each of its steps on a budget of its own
// (runtime.SandboxTeardownBudget) — the same steps, on the same budgets, a
// parked engine runs before it returns — and a margin for the rest of its
// unwind.
const engineUnwindCeiling = runtime.SandboxTeardownBudget + unwindMargin

// postEngineCeiling bounds what the runner still does under the lease once
// the engine returned: every post-engine step on its own budget
// (postEngineBudget), and a margin for what those budgets do not count — the
// timeline records the bank writes on bounds of their own, the local
// clean-up.
const postEngineCeiling = postEngineBudget + unwindMargin

// unwindMargin is each phase's allowance for the work its budgets do not
// name.
const unwindMargin = 2 * time.Minute

// postEngineBudget is the sum of the budgets of the steps executeRun takes
// once its engine returned, in their order: the retry circuit's reset, the
// git snapshot, the bank, the artifact upload, the sealed credentials'
// deletion, then its deferred spend records and run log's close. A step added
// there without a budget, or without its budget here, holds the lease past
// the ceiling a resume's retries are spread over.
const postEngineBudget = retryCircuitResetTimeout + gitMetaBudget + bankStepBudget +
	uploadRunFilesBudget + runSecretsDeleteTimeout + orgSpendBudget + runLogCloseBudget

// heartbeat refreshes the NATS KV lease so a long-running run keeps
// holding the lock past the 60s default TTL. Returns when ctx is
// cancelled (the hold ended). On refresh failure it cancels the run with
// runtime.ErrRunInterrupted so the engine unwinds to failed_resumable
// proactively before the lease expires — without that the lease would
// silently lapse and JetStream would redeliver to a sibling pod, two
// writers ending up on the same run state. The interrupted cause makes
// the engine write failed_resumable so the redelivery auto-resumes
// instead of requiring a manual user resume.
func (r *Runner) heartbeat(ctx context.Context, runCancel context.CancelCauseFunc, lock store.RunLock, delivery progressReporter, done chan<- struct{}) {
	defer close(done)
	natsLock, ok := lock.(leaseRefresher)
	if !ok {
		return // no-op lock or non-NATS provider — nothing to refresh
	}
	t := time.NewTicker(r.cfg.HeartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// Hold the JetStream ack deadline open. AckWait (5m default)
			// is far shorter than many real runs; without a periodic
			// InProgress() the broker redelivers the message to a sibling
			// and, after MaxDeliver attempts, drops it from the queue —
			// destroying the crash-recovery safety net while the run is
			// still healthy and head-of-line-blocking one of the consumer's
			// MaxAckPending slots. Best-effort: a transient miss is retried
			// on the next tick, well inside AckWait.
			if err := delivery.InProgress(); err != nil {
				r.cfg.Logger.Warn("runner: heartbeat InProgress failed: %v", err)
			}
			if err := natsLock.Refresh(ctx); err != nil {
				if errors.Is(err, context.Canceled) {
					return // run already exiting
				}
				if r.cfg.Metrics != nil {
					r.cfg.Metrics.RunnerHeartbeatErrors.Inc()
				}
				r.cfg.Logger.Error("runner: heartbeat refresh failed: %v — cancelling run (resumable) to avoid split-brain", err)
				runCancel(runtime.ErrRunInterrupted)
				return
			}
		}
	}
}

// archiveLockFailure retains an exhausted delivery, not a failed execution.
// Without the lock no writer may change the run's outcome or continuation.
func (r *Runner) archiveLockFailure(msg *queue.RunMessage, delivery jsDelivery, logger *iterlog.Logger, lockErr error, held bool) {
	// The operator triaging this DLQ entry decides between replay (which
	// duplicates a live run) and discard (which destroys the last copy), so
	// the reason must never claim more than the lock store proved. Only
	// ErrLockHeld proves an owner; anything else leaves ownership
	// unconfirmed — which is NOT the same as no owner.
	reason := fmt.Sprintf("run lock acquisition failed after %d deliveries: %v; run state unchanged — ownership could not be confirmed, so inspect the run and the lock service before replaying this delivery", delivery.NumDelivered(), lockErr)
	if held {
		reason = fmt.Sprintf("run lock held by another runner after %d deliveries: %v; run state unchanged — inspect the owner before replaying this delivery", delivery.NumDelivered(), lockErr)
	}
	publishTimeout := archiveWriteTimeout
	if r.publishTimeout > 0 {
		publishTimeout = r.publishTimeout
	}
	publishCtx, publishCancel := context.WithTimeout(context.Background(), publishTimeout)
	var err error
	if r.lockFailureDLQ != nil {
		err = r.lockFailureDLQ(publishCtx, delivery, reason)
	} else if d, ok := delivery.(*natsq.Delivery); ok && r.cfg.NATS != nil {
		err = r.cfg.NATS.PublishDLQ(publishCtx, d, reason)
	} else {
		err = errors.New("DLQ publisher unavailable")
	}
	publishCancel()
	data := map[string]any{"reason": reason, "delivered": delivery.NumDelivered(), "parked": err == nil}
	if err != nil {
		data["error"] = err.Error()
		// PublishDLQ waits for the JetStream PubAck, so a deadline or a lost
		// connection leaves the outcome genuinely indeterminate: the server
		// may have persisted the copy and only the ack went missing. Saying
		// the copy is gone would invite a blind replay that duplicates the
		// run when it did land — report it as UNCONFIRMED.
		logger.Error("runner: exhausted lock delivery for %s was not confirmed archived: %v — a DLQ copy may or may not exist; inspect the DLQ before replaying or discarding", msg.RunID, err)
	} else {
		logger.Warn("runner: exhausted lock delivery for %s archived on DLQ; the run is unchanged", msg.RunID)
	}
	// The publish above is bounded by its context ALONE and can burn the
	// whole deadline waiting on a PubAck during a broker outage — which is
	// precisely when this row matters most, the DLQ copy being unconfirmed
	// and this the only trail that is not. Inheriting the spent publish
	// context would fail the append instantly on any store that honours it:
	// Mongo threads ctx into guardNotDeleted/allocSeq/InsertOne, and the
	// cloud runner — the only place this path executes — uses Mongo. The
	// sibling park path (parkOnDLQOnFinalDelivery, loop_nats.go) hands its
	// spent publish context straight to the status flip; the fresh deadline
	// here is a deliberate divergence, not an oversight.
	// On the CONFIRMED-contention branch this row lands mid-timeline of a run
	// another pod is actively executing, so it must not read as that run's
	// own activity. Neither observer sees it, for different reasons worth
	// keeping true: alert.Manager treats ANY event as liveness (it clears
	// stallAlerted and can fire a spurious stall_recovered), but it is fed
	// only by the local events.jsonl tailer and in-process run observers —
	// never by the Mongo store this path writes to; and its cloud twin
	// alert.OpsDispatcher consumes trigger.Events off the bus, filtered to
	// KindRunFailed, which a store event never becomes. Wiring a cloud event
	// source into the Manager would make this row a false liveness signal.
	auditCtx, auditCancel := context.WithTimeout(context.Background(), archiveWriteTimeout)
	defer auditCancel()
	if _, auditErr := r.cfg.Store.AppendEvent(store.WithIdentity(auditCtx, msg.TenantID, msg.OwnerID), msg.RunID,
		store.Event{Type: store.EventRunDeliveryExhausted, Data: data}); auditErr != nil {
		logger.Error("runner: record exhausted lock delivery for %s: %v", msg.RunID, auditErr)
	}
	termTerminal(logger, delivery, "term-lock-exhausted", msg.RunID)
}

// archiveWriteTimeout bounds each of the two INDEPENDENT writes the
// archive path makes — the DLQ publish, then the audit event. They never
// share a deadline: see archiveLockFailure.
const archiveWriteTimeout = 10 * time.Second

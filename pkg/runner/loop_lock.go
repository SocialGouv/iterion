package runner

import (
	"context"
	"errors"
	"fmt"
	"time"

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
			logDeliveryErr(logger, "nak-lock-deferred", msg.RunID, delivery.NakWithDelay(delay))
		}
		return nil, false, status
	}
	return lock, true, ""
}

// heartbeat refreshes the NATS KV lease so a long-running run keeps
// holding the lock past the 60s default TTL. Returns when ctx is
// cancelled (run finished). On refresh failure it cancels the run with
// runtime.ErrRunInterrupted so the engine unwinds to failed_resumable
// proactively before the lease expires — without that the lease would
// silently lapse and JetStream would redeliver to a sibling pod, two
// writers ending up on the same run state. The interrupted cause makes
// the engine write failed_resumable so the redelivery auto-resumes
// instead of requiring a manual user resume.
func (r *Runner) heartbeat(ctx context.Context, runCancel context.CancelCauseFunc, lock store.RunLock, delivery *natsq.Delivery, done chan<- struct{}) {
	defer close(done)
	natsLock, ok := lock.(*natsq.Lock)
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

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
// runners executing the same run. ErrLockHeld means a sibling already
// has it. Retry with a delay while attempts remain; archive the delivery on
// exhaustion without changing the run owned by that sibling. Returns
// (lock, true, "") on success; (nil, false, finalStatus) when the caller
// must abandon the delivery (finalStatus is the metric label).
func (r *Runner) acquireRunLock(runCtx context.Context, msg *queue.RunMessage, delivery jsDelivery, logger *iterlog.Logger) (store.RunLock, bool, string) {
	// Acquire the distributed lock. Two competing runners on the
	// same run is the contention this guards against.
	lock, err := r.cfg.Store.LockRun(runCtx, msg.RunID)
	if err != nil {
		status := "failed"
		if errors.Is(err, natsq.ErrLockHeld) {
			status = "lock_held"
		}
		logger.Warn("runner: lock %s: %v", msg.RunID, err)
		if max := r.maxDeliver(); max > 0 && delivery.NumDelivered() >= max {
			r.archiveLockFailure(msg, delivery, logger, err)
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
func (r *Runner) archiveLockFailure(msg *queue.RunMessage, delivery jsDelivery, logger *iterlog.Logger, lockErr error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	reason := fmt.Sprintf("run lock unavailable after %d deliveries: %v; run state unchanged — inspect the owner before replaying this delivery", delivery.NumDelivered(), lockErr)
	var err error
	if r.lockFailureDLQ != nil {
		err = r.lockFailureDLQ(ctx, delivery, reason)
	} else if d, ok := delivery.(*natsq.Delivery); ok && r.cfg.NATS != nil {
		err = r.cfg.NATS.PublishDLQ(ctx, d, reason)
	} else {
		err = errors.New("DLQ publisher unavailable")
	}
	data := map[string]any{"reason": reason, "delivered": delivery.NumDelivered(), "parked": err == nil}
	if err != nil {
		data["error"] = err.Error()
		logger.Error("runner: exhausted lock delivery for %s could not be archived: %v — no queue copy remains", msg.RunID, err)
	} else {
		logger.Warn("runner: exhausted lock delivery for %s archived on DLQ; the owner's run is unchanged", msg.RunID)
	}
	if _, auditErr := r.cfg.Store.AppendEvent(store.WithIdentity(ctx, msg.TenantID, msg.OwnerID), msg.RunID,
		store.Event{Type: store.EventRunDeliveryExhausted, Data: data}); auditErr != nil {
		logger.Error("runner: record exhausted lock delivery for %s: %v", msg.RunID, auditErr)
	}
	termTerminal(logger, delivery, "term-lock-exhausted", msg.RunID)
}

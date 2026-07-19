package runner

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	natsq "github.com/SocialGouv/iterion/pkg/queue/nats"
	"github.com/SocialGouv/iterion/pkg/store"
)

// acquireRunLock claims the distributed run lock guarding against two
// runners executing the same run. ErrLockHeld means a sibling already
// has it — Nak so the sibling retains exclusive ownership; any other
// lock error is a transient-store shape and also Nak'd. Returns
// (lock, true, "") on success; (nil, false, finalStatus) when the caller
// must abandon the delivery (finalStatus is the metric label).
func (r *Runner) acquireRunLock(runCtx context.Context, msg *queue.RunMessage, delivery *natsq.Delivery, logger *iterlog.Logger) (store.RunLock, bool, string) {
	// Acquire the distributed lock. Two competing runners on the
	// same run is the contention this guards against.
	lock, err := r.cfg.Store.LockRun(runCtx, msg.RunID)
	if err != nil {
		if errors.Is(err, natsq.ErrLockHeld) {
			logger.Warn("runner: lock held for %s — naking for sibling", msg.RunID)
			nakTerminal(logger, delivery, "nak-lock-held", msg.RunID)
			return nil, false, "lock_held"
		}
		logger.Error("runner: lock %s: %v", msg.RunID, err)
		nakTerminal(logger, delivery, "nak-lock-error", msg.RunID)
		return nil, false, "failed"
	}
	return lock, true, ""
}

// heartbeat refreshes the NATS KV lease so a long-running run keeps
// holding the lock past the 60s default TTL. Returns when ctx is
// cancelled (run finished). On refresh failure the heartbeat sets
// hbFailed and triggers runCancel so the engine unwinds proactively
// before the lease expires — without that signal, the lease would
// silently lapse and JetStream would redeliver to a sibling pod, two
// writers ending up on the same run state. processOne reads hbFailed
// to Nak (not Ack) the delivery so the message stays queued for sibling
// redelivery instead of requiring manual user resume.
func (r *Runner) heartbeat(ctx context.Context, runCancel context.CancelFunc, lock store.RunLock, delivery *natsq.Delivery, done chan<- struct{}, hbFailed *atomic.Bool) {
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
				r.cfg.Logger.Error("runner: heartbeat refresh failed: %v — cancelling run to avoid split-brain", err)
				hbFailed.Store(true)
				runCancel()
				return
			}
		}
	}
}

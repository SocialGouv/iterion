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

// decodeOrTerm decodes the delivery payload into a queue.RunMessage. On
// decode failure it Terms the delivery so the malformed message doesn't
// loop in JetStream, surfacing a failed-Term at WARN so the operator
// can purge rather than chase a silent loop. Returns (msg, true) on
// success; (nil, false) when the caller must abandon the delivery.
func (r *Runner) decodeOrTerm(delivery *natsq.Delivery) (*queue.RunMessage, bool) {
	msg, err := delivery.Decode()
	if err != nil {
		r.cfg.Logger.Error("runner: decode delivery: %v", err)
		if termErr := delivery.Term(); termErr != nil {
			// A failed Term leaves the malformed message in the queue
			// where it will be redelivered and fail decode again on
			// every runner — surface it so the operator can purge
			// rather than chase a silent loop.
			r.cfg.Logger.Warn("runner: term after decode failure: %v", termErr)
		}
		return nil, false
	}
	return msg, true
}

// parkOnDLQOnFinalDelivery handles the DLQ branch: a generic engine
// error on the LAST permitted JetStream attempt must park a copy on the
// DLQ and Term instead of Nak — without the bridge, JetStream silently
// drops the message after MaxDeliver and the run is unrecoverable except
// by hand. Handled here (not via classifyExecResult) because it has side
// effects (PublishDLQ + UpdateRunStatusIf) and uses its own context with
// `defer cancel()`. Returns (true, finalStatus) when the caller must
// stop processing the delivery (DLQ dispatch already issued);
// (false, "") otherwise to fall through to classifyExecResult.
func (r *Runner) parkOnDLQOnFinalDelivery(err error, delivery *natsq.Delivery, msg *queue.RunMessage, logger *iterlog.Logger) (bool, string) {
	if err == nil ||
		errors.Is(err, runtime.ErrRunPaused) ||
		errors.Is(err, runtime.ErrRunPausedOperator) ||
		errors.Is(err, runtime.ErrRunCancelled) ||
		errors.Is(err, runtime.ErrRunInterrupted) ||
		r.cfg.NATS == nil || delivery.NumDelivered() < r.cfg.NATS.MaxDeliver() {
		return false, ""
	}
	logger.Error("runner: run %s failed on final delivery %d/%d — parking on DLQ: %v",
		msg.RunID, delivery.NumDelivered(), r.cfg.NATS.MaxDeliver(), err)
	bg, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if perr := r.cfg.NATS.PublishDLQ(bg, delivery, err.Error()); perr != nil {
		// DLQ unavailable: keep the JetStream redelivery as
		// the only remaining safety net.
		logger.Error("runner: DLQ park for %s failed: %v — naking instead", msg.RunID, perr)
		nakTerminal(logger, delivery, "nak-dlq-failed", msg.RunID)
		return true, "dlq"
	}
	sctx := store.WithIdentity(bg, msg.TenantID, msg.OwnerID)
	if _, serr := r.cfg.Store.UpdateRunStatusIf(sctx, msg.RunID, store.RunStatusFailedResumable,
		fmt.Sprintf("max deliveries exhausted: %v (parked on DLQ — replay via /api/admin/dlq)", err),
		[]store.RunStatus{store.RunStatusRunning, store.RunStatusQueued}); serr != nil {
		logger.Warn("runner: DLQ status flip for %s: %v", msg.RunID, serr)
	}
	termTerminal(logger, delivery, "term-dlq-parked", msg.RunID)
	return true, "dlq"
}

// pollPending samples the JetStream consumer info on a fixed cadence
// and republishes the Pending count to nats_pending_messages. Exits
// when ctx is cancelled. Errors are logged at debug level — the
// scaler is the source of truth for autoscaling, so a transient miss
// here is observability noise, not a correctness issue.
func (r *Runner) pollPending(ctx context.Context) {
	t := time.NewTicker(r.cfg.PendingPoll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pending, err := r.consumer.Pending(ctx)
			if err != nil {
				r.cfg.Logger.Debug("runner: pending poll: %v", err)
				continue
			}
			r.cfg.Metrics.NATSPendingMessages.Set(float64(pending))
		}
	}
}

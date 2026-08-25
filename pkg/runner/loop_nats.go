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

// decodeOrTerm decodes the delivery payload into a queue.RunMessage.
//
// Two decode failures, two opposite answers. A MALFORMED payload will never
// decode on any consumer, so it is Termed rather than left to loop in
// JetStream, with a failed Term surfaced at WARN so the operator can purge. A
// payload whose schema version this build does not recognise is handed to
// handleSchemaMismatch instead — see below.
//
// Returns (msg, true) on success; (nil, false) when the caller must abandon
// the delivery.
func (r *Runner) decodeOrTerm(delivery *natsq.Delivery) (*queue.RunMessage, bool) {
	msg, err := delivery.Decode()
	if err != nil {
		if errors.Is(err, queue.ErrSchemaVersion) {
			r.handleSchemaMismatch(delivery, err)
			return nil, false
		}
		// A payload from ANOTHER schema version need not even unmarshal
		// into this build's RunMessage (a bump may change a field's JSON
		// type), so the version is also peeked from the raw envelope —
		// otherwise such a message takes the malformed branch below and
		// is Termed away, which is exactly the loss #481 closes.
		if env, envErr := delivery.Envelope(); envErr == nil && env.V > 0 && env.V != queue.SchemaVersion {
			r.handleSchemaMismatch(delivery, fmt.Errorf("%w: %d unsupported (want %d)", queue.ErrSchemaVersion, env.V, queue.SchemaVersion))
			return nil, false
		}
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

// handleSchemaMismatch answers a delivery whose schema version this build
// does not recognise. That is the ordinary state of a rolling upgrade, in
// EITHER direction: this pod may be behind a server that already publishes
// the new version, or ahead of a queue that still holds messages published
// before the cutover (strict equality rejects both — see issue #481).
//
// Two rules make the overlap safe:
//
//  1. The Nak is DELAYED (SchemaMismatchNakDelay), never immediate. A bare
//     Nak redelivers at once, so a fleet of stale runners can burn the whole
//     MaxDeliver budget in seconds — long before an upgraded runner had any
//     chance to take the message. The delay stretches the budget over
//     minutes of wall clock, which is what a rolling restart needs.
//  2. On the FINAL permitted delivery the message is parked on the DLQ and
//     Termed, and the run document is flipped from `queued` to
//     `failed_resumable` with an actionable message — whether or not the
//     DLQ park itself succeeded (with the budget spent a Nak is inert, so
//     a park failure loses the queue entry; the flip is then the ONLY
//     actionable trail, with a relaunch hint instead of a replay hint).
//     Without this bridge JetStream silently drops the exhausted message:
//     the queue entry is gone while the run document sits `queued` forever,
//     the refusal visible only in one pod's log. Seen in production during
//     a schema bump — two runs vanished that way before the fleet finished
//     rolling. Parked messages replay verbatim once the fleet speaks their
//     version (see docs/cloud-queue-schema-rollout.md).
func (r *Runner) handleSchemaMismatch(delivery *natsq.Delivery, decodeErr error) {
	logger := r.cfg.Logger
	delay := r.cfg.SchemaMismatchDelay
	if delay <= 0 {
		delay = natsq.SchemaMismatchNakDelay
	}
	if r.cfg.NATS == nil || delivery.NumDelivered() < r.cfg.NATS.MaxDeliver() {
		logger.Warn("runner: %v — leaving it for a runner that speaks its version (this pod does not)", decodeErr)
		if nakErr := delivery.NakWithDelay(delay); nakErr != nil {
			logger.Warn("runner: nak after version mismatch: %v", nakErr)
		}
		return
	}

	logger.Error("runner: %v — delivery budget exhausted (%d/%d), parking on DLQ",
		decodeErr, delivery.NumDelivered(), r.cfg.NATS.MaxDeliver())
	parkCtx, parkCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer parkCancel()
	env, envErr := delivery.Envelope()
	if envErr != nil {
		logger.Warn("runner: envelope decode on final delivery: %v — run document cannot be flipped", envErr)
	}
	// Direction-neutral guidance: replay only helps when the fleet speaks
	// the parked message's version; in the other direction (or if nothing
	// was parked) resuming/relaunching re-publishes at the CURRENT version.
	runErr := fmt.Sprintf("schema version mismatch: %v (queue message v%d parked on DLQ — replay via /api/admin/dlq only once the runner fleet speaks schema v%d; otherwise resume this run, which re-publishes at the current schema version — see docs/cloud-queue-schema-rollout.md)", decodeErr, env.V, env.V)
	if perr := r.cfg.NATS.PublishDLQ(parkCtx, delivery, decodeErr.Error()); perr != nil {
		logger.Error("runner: DLQ park after version mismatch failed: %v — queue entry lost with the budget spent", perr)
		runErr = fmt.Sprintf("schema version mismatch: %v (delivery budget exhausted and DLQ park failed: %v — no queue copy remains; relaunch this run)", decodeErr, perr)
	}
	// The flip is scoped by the tenant identity: a payload without
	// tenant_id is logged, never written under an unfiltered (privileged)
	// store context.
	var outcomeMsg *queue.RunMessage
	switch {
	case envErr != nil:
		// Already logged above.
	case env.TenantID == "":
		logger.Warn("runner: mismatched message for run %s has no tenant_id — run document not flipped", env.RunID)
	default:
		// PublishDLQ is bounded by its context alone and can consume the full
		// park deadline during a broker outage. The status flip is the only
		// actionable trail when that happens, so it needs an independent
		// deadline rather than inheriting an already-expired parkCtx.
		flipCtx, flipCancel := context.WithTimeout(context.Background(), 10*time.Second)
		sctx := store.WithIdentity(flipCtx, env.TenantID, env.OwnerID)
		changed, serr := r.cfg.Store.UpdateRunStatusIf(sctx, env.RunID, store.RunStatusFailedResumable, runErr,
			[]store.RunStatus{store.RunStatusQueued})
		flipCancel()
		if serr != nil {
			logger.Warn("runner: schema-mismatch status flip for %s: %v", env.RunID, serr)
		} else if changed {
			// This delivery never acquired the run lease: a running row belongs
			// to another pod (for example after an operator resumed while this
			// stale message was still bouncing). Only the queued owner may emit
			// the terminal-shaped outcome signals below.
			// A schema park is a final disposition just like the generic DLQ
			// path in processOne. Preserve its terminal side effects: release
			// the credential-pool lease, send the completion callback and emit
			// the run-outcome event. Only the successful CAS owner does so.
			outcomeMsg = &queue.RunMessage{RunID: env.RunID, TenantID: env.TenantID, OwnerID: env.OwnerID}
		}
	}
	if termErr := delivery.Term(); termErr != nil {
		logger.Warn("runner: term after schema-mismatch handling: %v", termErr)
	}
	if outcomeMsg != nil {
		// This sealed bundle will never execute: a later explicit resume
		// resolves credentials and acquires afresh. Leaving its lease open
		// would strand the donor's concurrency slot until lease expiry.
		r.cfg.CredPool.Release(context.Background(), outcomeMsg.RunID)
		r.fireCompletionNotifier(outcomeMsg)
		r.fireOutcomeEvent(outcomeMsg, decodeErr)
	}
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

package runner

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/SocialGouv/iterion/pkg/credpool"
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
		if env, envErr := delivery.Envelope(); envErr == nil && env.V > 0 && (env.V < queue.MinSchemaVersion || env.V > queue.SchemaVersion) {
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
	payloadParked := true
	if perr := r.cfg.NATS.PublishDLQ(parkCtx, delivery, decodeErr.Error()); perr != nil {
		payloadParked = false
		logger.Error("runner: DLQ park after version mismatch failed: %v — queue entry lost with the budget spent", perr)
		runErr = fmt.Sprintf("schema version mismatch: %v (delivery budget exhausted and DLQ park failed: %v — no queue copy remains; relaunch this run)", decodeErr, perr)
	}
	// The flip is scoped by the tenant identity: a payload without
	// tenant_id is logged, never written under an unfiltered (privileged)
	// store context.
	var outcomeMsg *queue.RunMessage
	var poolRelease *credpool.ReleaseGuard
	switch {
	case envErr != nil:
		// Already logged above.
	case env.TenantID == "":
		logger.Warn("runner: mismatched message for run %s has no tenant_id — run document not flipped", env.RunID)
	default:
		publishedAt, parseErr := time.Parse(time.RFC3339Nano, env.PublishedAtRFC)
		attempts := store.AsQueuedAttemptStore(r.cfg.Store)
		if parseErr != nil {
			logger.Warn("runner: mismatched message for run %s has invalid published_at %q — run document not flipped: %v", env.RunID, env.PublishedAtRFC, parseErr)
			break
		}
		if attempts == nil {
			// Fail safe for a third-party store: a status-only fallback could
			// clobber a newer resume while it passes through queued.
			logger.Warn("runner: store cannot compare queue attempts for run %s — run document not flipped", env.RunID)
			break
		}
		// PublishDLQ is bounded by its context alone and can consume the full
		// park deadline during a broker outage. The status flip is the only
		// actionable trail when that happens, so it needs an independent
		// deadline rather than inheriting an already-expired parkCtx.
		flipCtx, flipCancel := context.WithTimeout(context.Background(), 10*time.Second)
		sctx := store.WithIdentity(flipCtx, env.TenantID, env.OwnerID)
		if !payloadParked {
			// With no replayable payload, this sealed bundle will never run.
			// Capture the exact lease BEFORE making the run resumable: a new
			// attempt may acquire immediately after the CAS, and a later run-id
			// lookup could otherwise close that successor.
			poolRelease = r.cfg.CredPool.CaptureRelease(flipCtx, env.RunID)
		}
		changed, serr := attempts.FailQueuedRunIfAttempt(sctx, env.RunID, runErr, publishedAt)
		flipCancel()
		if serr != nil {
			logger.Warn("runner: schema-mismatch status flip for %s: %v", env.RunID, serr)
		} else if changed {
			// This delivery never acquired the run lease. Only the owner of this
			// exact queued attempt may emit the terminal-shaped signals below;
			// a later resume has a newer QueuedAt and makes the CAS a no-op even
			// before its claiming pod transitions the row to running.
			// A schema park is a final disposition just like the generic DLQ
			// path in processOne. Preserve its terminal side effects: send the
			// completion callback and emit the run-outcome event. When the DLQ
			// park failed, also release the unusable credential-pool lease.
			// Only the successful CAS owner does so.
			outcomeMsg = &queue.RunMessage{RunID: env.RunID, TenantID: env.TenantID, OwnerID: env.OwnerID}
		}
	}
	if termErr := delivery.Term(); termErr != nil {
		logger.Warn("runner: term after schema-mismatch handling: %v", termErr)
	}
	if outcomeMsg != nil {
		// A parked payload replays byte-for-byte with its original SecretsRef,
		// so its lease must stay open for spend attribution. Release only when
		// the DLQ failed and no replayable copy remains.
		if !payloadParked {
			r.cfg.CredPool.ReleaseCaptured(context.Background(), poolRelease)
		}
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
// when ctx is cancelled. A transient miss stays Debug — the scaler is
// the source of truth for autoscaling — but a PERSISTENT failure means
// the gauge is frozen at its last value and KEDA is scaling on stale
// data, so the episode is surfaced once at Warn (and its recovery once
// at Info) instead of never.
func (r *Runner) pollPending(ctx context.Context) {
	const staleAfter = 5 // consecutive failed samples before the gauge counts as stale
	t := time.NewTicker(r.cfg.PendingPoll)
	defer t.Stop()
	failures := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pending, err := r.consumer.Pending(ctx)
			if err != nil {
				failures++
				if failures == staleAfter {
					r.cfg.Logger.Warn("runner: pending poll failing for %s (%v) — iterion_nats_pending_messages is frozen at its last value, KEDA is scaling on stale data", time.Duration(failures)*r.cfg.PendingPoll, err)
				} else {
					r.cfg.Logger.Debug("runner: pending poll: %v", err)
				}
				continue
			}
			if failures >= staleAfter {
				r.cfg.Logger.Info("runner: pending poll recovered after %d failed samples", failures)
			}
			failures = 0
			r.cfg.Metrics.NATSPendingMessages.Set(float64(pending))
		}
	}
}

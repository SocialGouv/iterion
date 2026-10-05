package runner

import (
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
)

// verifyTenantOrTerm refuses a delivery whose message tenant doesn't
// match the persisted run document. A mismatch implies either a
// corrupted publish (publisher stamped the wrong tenant) or a malicious
// / replayed message; either way the run is unsafe to execute under
// either tenant's scope, so we Term the delivery to keep it from
// redelivering. Kept separate from resolveDeliveryPreconditions so the
// failed-Term log can carry a security-shaped ERROR-level alarm asking
// the operator to purge the JetStream subject manually — the generic
// logDeliveryErr breadcrumb wouldn't surface it. Returns true to proceed,
// false when the caller must abandon the delivery.
func (r *Runner) verifyTenantOrTerm(pre preconditionOutcome, msg *queue.RunMessage, delivery jsDelivery, logger *iterlog.Logger) bool {
	if pre.preRun.TenantID == msg.TenantID {
		return true
	}
	logger.Error("runner: tenant mismatch for run %s (msg=%q stored=%q) — terming", msg.RunID, msg.TenantID, pre.preRun.TenantID)
	if termErr := delivery.Term(); termErr != nil {
		// HIGH-impact: a failed Term on a tenant-mismatched
		// message means a forged / replayed delivery stays in the
		// queue and JetStream will redeliver it, looping forever.
		// Surface loudly so the operator can purge the stream.
		logger.Error("runner: term for %s after tenant mismatch FAILED (%v) — message will redeliver; purge the JetStream subject manually", msg.RunID, termErr)
	}
	return false
}

// verifyPoolOrTerm is the pool admission (plan v2.1 D4'): the pod serves
// ONE pool, and the THREE stamps must agree — the delivery's message, the
// persisted document (frozen at launch), and this pod's own pool identity.
// Any disagreement is a corrupted or replayed publish: the delivery is
// Termed so it cannot redeliver, mirroring verifyTenantOrTerm (a failed
// Term is surfaced loudly for the operator to purge). The doc-flip to a
// failed attempt goes through the caller's attempt-safe path. Red when the
// admission is deleted.
func (r *Runner) verifyPoolOrTerm(pre preconditionOutcome, msg *queue.RunMessage, delivery jsDelivery, logger *iterlog.Logger) bool {
	// An empty pod pool is the shared default: it must never receive a
	// pool-stamped message (the shared consumer's exact subject filter
	// already prevents it structurally; this is the defence-in-depth leg).
	if msg.RunnerPool != r.cfg.RunnerPool {
		logger.Error("runner: pool mismatch for run %s (msg=%q pod=%q) — terming", msg.RunID, msg.RunnerPool, r.cfg.RunnerPool)
		if termErr := delivery.Term(); termErr != nil {
			logger.Error("runner: term for %s after pool mismatch FAILED (%v) — message will redeliver; purge the JetStream subject manually", msg.RunID, termErr)
		}
		return false
	}
	// The document's frozen stamp must agree with the message: a publish
	// whose wire says one pool and whose document says another is corrupted
	// or replayed.
	if pre.preRun.RunnerPool != msg.RunnerPool {
		logger.Error("runner: pool mismatch message vs document for run %s (msg=%q doc=%q) — terming", msg.RunID, msg.RunnerPool, pre.preRun.RunnerPool)
		if termErr := delivery.Term(); termErr != nil {
			logger.Error("runner: term for %s after pool-vs-document mismatch FAILED (%v) — message will redeliver; purge the JetStream subject manually", msg.RunID, termErr)
		}
		return false
	}
	return true
}

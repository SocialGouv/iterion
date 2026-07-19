package runner

import (
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	natsq "github.com/SocialGouv/iterion/pkg/queue/nats"
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
func (r *Runner) verifyTenantOrTerm(pre preconditionOutcome, msg *queue.RunMessage, delivery *natsq.Delivery, logger *iterlog.Logger) bool {
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

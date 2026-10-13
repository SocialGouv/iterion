package runner

import (
	"fmt"
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
// Any disagreement is a corrupted or replayed publish: the delivery goes to
// the admission-mismatch disposition (park on the run's pool's DLQ + the
// attempt-safe document flip with the named reason) — never executed under
// either pool's scope. Red when a leg is dropped or the disposition is
// downgraded to a bare Term.
func (r *Runner) verifyPoolOrTerm(pre preconditionOutcome, msg *queue.RunMessage, delivery *natsq.Delivery, logger *iterlog.Logger) bool {
	if reason := poolAdmissionDecision(r.cfg.RunnerPool, msg.RunnerPool, pre.preRun.RunnerPool); reason != "" {
		logger.Error("runner: pool mismatch for run %s: %s", msg.RunID, reason)
		r.handleAdmissionMismatch(delivery, admissionMismatchPool, fmt.Errorf("%s", reason), 0)
		return false
	}
	return true
}

// poolAdmissionDecision is the pure admission rule: "" when the three
// stamps agree (pod serves msg, doc frozen = msg), otherwise the mismatch
// reason. An empty pod pool is the shared default — it must never receive
// a pool-stamped message (the shared consumer's exact subject filter
// already prevents it structurally; this is the defence-in-depth leg).
func poolAdmissionDecision(pod, msgPool, docPool string) string {
	if msgPool != pod {
		return fmt.Sprintf("message stamped %q, pod serves %q", msgPool, pod)
	}
	if docPool != msgPool {
		return fmt.Sprintf("message stamped %q, document frozen %q", msgPool, docPool)
	}
	return ""
}

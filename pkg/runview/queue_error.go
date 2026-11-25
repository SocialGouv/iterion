package runview

import "fmt"

// QueueUnavailableErrorCode is the stable API code returned when the cloud
// queue cannot accept a launch or resume after bounded retries.
const QueueUnavailableErrorCode = "QUEUE_UNAVAILABLE"

// QueueUnavailableError marks a transient queue-backend outage. Callers may
// retry the same operation; semantic resume refusals use their existing error
// types and are deliberately not wrapped in this type.
type QueueUnavailableError struct {
	Cause error
}

func (e *QueueUnavailableError) Error() string {
	if e == nil || e.Cause == nil {
		return "queue temporarily unavailable"
	}
	return fmt.Sprintf("queue temporarily unavailable: %v", e.Cause)
}

func (e *QueueUnavailableError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// Code and Retryable let transport layers expose stable machine-readable
// semantics without matching the human-readable NATS cause.
func (e *QueueUnavailableError) Code() string    { return QueueUnavailableErrorCode }
func (e *QueueUnavailableError) Retryable() bool { return true }

// LeaseUnreachable declares a queue outage a failure to REACH the queue, for
// pkg/lease's classifier (lease.TransportFailure, which types no queue of its
// own: a NATS timeout or "no responders" is a plain sentinel to it). This
// error wraps every transient publish failure that outlived the bounded
// retries, so the declaration covers more than a node that never got there:
// it also covers the JetStream degradation answers the broker itself sends
// back (API error codes 10008, 10023 and 10040), which a peer on another
// node would get just the same. That over-declaration is deliberate, in the
// bounded direction: a holder that cannot publish hands its net over rather
// than keeping it, and a queue degraded for the whole fleet is capped by the
// sick-terms circuit (lease.Health).
func (e *QueueUnavailableError) LeaseUnreachable() bool { return e != nil }

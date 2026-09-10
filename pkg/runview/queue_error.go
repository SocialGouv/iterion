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

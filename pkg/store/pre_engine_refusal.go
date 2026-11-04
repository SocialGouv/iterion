package store

import (
	"context"
	"errors"
	"strings"
	"time"
)

// PreEngineRefusal describes one delivery refused before the engine runs.
// A queued diagnostic is not a failed outcome: the pending checkpoint and
// answers still belong to that delivery's retry. Only the recorder writes it.
type PreEngineRefusal struct {
	Phase       string      `json:"phase" bson:"phase"`
	Code        FailureCode `json:"code" bson:"code"`
	Message     string      `json:"message" bson:"message"`
	Hint        string      `json:"hint,omitempty" bson:"hint,omitempty"`
	PublishedAt time.Time   `json:"published_at,omitempty" bson:"published_at,omitempty"`
	QueuedAt    *time.Time  `json:"queued_at,omitempty" bson:"queued_at,omitempty"`
	ObservedAt  time.Time   `json:"observed_at" bson:"observed_at"`
}

// PreEngineDisposition names the existing delivery decisions. It does not
// classify errors or decide whether the queue acknowledges a delivery.
type PreEngineDisposition string

const (
	PreEngineRetryPending PreEngineDisposition = "retry_pending"
	PreEngineFinalVerdict PreEngineDisposition = "final_verdict"
	PreEngineUsagePark    PreEngineDisposition = "usage_park"
)

var ErrInvalidPreEngineRefusal = errors.New("invalid pre-engine refusal")

// PreEngineRefusalRequest contains no arbitrary document patch. PublishedAt
// zero is legacy-only: the stored run must also have no queue marker. The
// recorder captures QueuedAt and ObservedAt itself under the same write.
type PreEngineRefusalRequest struct {
	Phase       string
	Code        FailureCode
	Message     string
	Hint        string
	PublishedAt time.Time
	Disposition PreEngineDisposition
	FinalStatus RunStatus
	// LeaveQueued lets the existing refused-resume releaser restore the pause.
	LeaveQueued bool
}

func (q PreEngineRefusalRequest) Validate() error {
	if strings.TrimSpace(q.Phase) == "" || q.Code == "" || q.Message == "" {
		return ErrInvalidPreEngineRefusal
	}
	switch q.Disposition {
	case PreEngineRetryPending:
		if q.LeaveQueued || q.FinalStatus != "" {
			return ErrInvalidPreEngineRefusal
		}
	case PreEngineUsagePark:
		if q.LeaveQueued || q.FinalStatus != "" || q.Code != FailureUsageLimitBlocked {
			return ErrInvalidPreEngineRefusal
		}
	case PreEngineFinalVerdict:
		if !q.FinalStatus.IsPreEngineFailureVerdict() {
			return ErrInvalidPreEngineRefusal
		}
	default:
		return ErrInvalidPreEngineRefusal
	}
	return nil
}

func (q PreEngineRefusalRequest) TargetStatus() RunStatus {
	if q.Disposition == PreEngineFinalVerdict {
		return q.FinalStatus
	}
	return RunStatusFailedResumable
}
func (q PreEngineRefusalRequest) Outcome() RunOutcomeMeta {
	m := RunOutcomeMeta{Code: q.Code}
	if q.Disposition == PreEngineFinalVerdict {
		m.Continuation = ContinuationFinal
	}
	return m
}

// PreEngineRefusalResult reports the state actually written, not a target
// the caller hoped to reach. A refused CAS produces no event-worthy result.
type PreEngineRefusalResult struct {
	Changed bool
	Status  RunStatus
}

// PreEngineRefusalRecorder fences every modifiable status by the delivery's
// publication, atomically with the write. A queued retry changes only its
// diagnostic/message; usage parking still creates the armable failed state.
// Capability absence has no unsafe status-only fallback.
type PreEngineRefusalRecorder interface {
	RecordPreEngineRefusal(context.Context, string, PreEngineRefusalRequest) (PreEngineRefusalResult, error)
}

func preEngineRefusalForSave(incoming, persisted *Run) *PreEngineRefusal {
	if persisted == nil || incoming.Status != persisted.Status || !samePreEngineMarker(incoming.QueuedAt, persisted.QueuedAt) || persisted.PreEngineRefusal == nil {
		return nil
	}
	copy := *persisted.PreEngineRefusal
	copy.QueuedAt = cloneTimePtr(copy.QueuedAt)
	return &copy
}
func samePreEngineMarker(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

package store

import (
	"context"
	"crypto/rand"
	"fmt"
	"time"
)

// RunExecutionAuthority is the grant returned by one successful acquisition.
// It must not be replaced by an epoch obtained from a later LoadRun.
type RunExecutionAuthority struct {
	RunID string `json:"run_id" bson:"run_id"`
	Epoch string `json:"epoch" bson:"epoch"`
}

// RunExecutionExpectation pins the pre-image of an acquisition. A caller may
// refresh Version after benign metadata writes, but must keep the entry Epoch.
type RunExecutionExpectation struct {
	Version     int64
	Epoch       string
	Status      RunStatus
	QueuedAt    *time.Time
	PublishedAt time.Time
}

type RunExecutionIntent string

const (
	ExecutionStart  RunExecutionIntent = "start"
	ExecutionResume RunExecutionIntent = "resume"
)

// RunExecutionStore does not replace run locks or authorize a caller to resume.
// It gives an already-authorized start/resume an atomic, durable identity.
type RunExecutionStore interface {
	AcquireRunExecutionIf(context.Context, string, RunExecutionExpectation, RunExecutionIntent) (RunExecutionAuthority, bool, error)
}

func AsRunExecutionStore(s RunStore) RunExecutionStore { return capability[RunExecutionStore](s) }

// NewExecutionEpoch returns an opaque cryptographic token. Random identities
// also distinguish a runID recreated after its deletion marker was pruned.
func NewExecutionEpoch() string { return rand.Text() }

func (e RunExecutionExpectation) Validate(intent RunExecutionIntent) error {
	valid := intent == ExecutionStart && e.Status.CanStartExecution() ||
		intent == ExecutionResume && (e.Status == RunStatusQueued || e.Status.CanOperatorResume())
	if !valid {
		return fmt.Errorf("store: invalid execution acquisition %q from %q", intent, e.Status)
	}
	if e.Version < 0 {
		return fmt.Errorf("store: negative acquisition version")
	}
	return nil
}

func (e RunExecutionExpectation) Matches(r *Run) bool {
	return r != nil && r.CASVersion == e.Version && r.ExecutionEpoch == e.Epoch && r.Status == e.Status &&
		sameExecutionTime(r.QueuedAt, e.QueuedAt) && (e.PublishedAt.IsZero() || r.Status != RunStatusQueued || r.QueuedAt == nil || !r.QueuedAt.After(e.PublishedAt))
}

func sameExecutionTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

func (a RunExecutionAuthority) Matches(r *Run) bool {
	return r != nil && a.RunID == r.ID && a.Epoch != "" && a.Epoch == r.ExecutionEpoch && r.Status == RunStatusRunning
}

func (s *FilesystemRunStore) AcquireRunExecutionIf(ctx context.Context, id string, expected RunExecutionExpectation, intent RunExecutionIntent) (RunExecutionAuthority, bool, error) {
	if err := expected.Validate(intent); err != nil {
		return RunExecutionAuthority{}, false, err
	}
	r, unlock, err := s.lockExistingRunMetadata(ctx, id)
	if err != nil {
		return RunExecutionAuthority{}, false, err
	}
	defer unlock()
	if !expected.Matches(r) {
		return RunExecutionAuthority{}, false, nil
	}
	transitionRunStatus(r, RunStatusRunning, "", RunOutcomeMeta{})
	if err := s.writeRunLockedContext(ctx, r, true, &expected.Epoch); err != nil {
		return RunExecutionAuthority{}, false, err
	}
	return RunExecutionAuthority{RunID: id, Epoch: r.ExecutionEpoch}, true, nil
}

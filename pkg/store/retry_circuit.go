package store

import (
	"context"
	"time"
)

// RetryCircuitState is the durable shared state for a retry circuit. The key
// is normally a workflow revision (or a tenant-scoped workflow name when no
// revision is available), so failures from concurrent runs participate in one
// breaker instead of each pod creating its own retry storm.
type RetryCircuitState struct {
	Key                 string     `json:"key" bson:"key"`
	ConsecutiveFailures int        `json:"consecutive_failures" bson:"consecutive_failures"`
	OpenUntil           *time.Time `json:"open_until,omitempty" bson:"open_until,omitempty"`
	LastFailureAt       *time.Time `json:"last_failure_at,omitempty" bson:"last_failure_at,omitempty"`
	LastFailureRunID    string     `json:"last_failure_run_id,omitempty" bson:"last_failure_run_id,omitempty"`
	UpdatedAt           time.Time  `json:"updated_at" bson:"updated_at"`
}

// RetryCircuitStore is an optional durable capability used by cloud runners
// to coordinate retry waves across pods. Implementations must scope keys to
// the current tenant and make updates atomic at the storage boundary.
type RetryCircuitStore interface {
	// RecordRetryFailure increments the shared failure counter and opens the
	// circuit once threshold is reached. The returned OpenUntil is the earliest
	// instant at which another retry wave may be admitted.
	RecordRetryFailure(ctx context.Context, key, runID string, now time.Time, threshold int, cooldown time.Duration) (*RetryCircuitState, error)
	// RetryCircuitOpen reports whether the breaker is open AT NOW: the state
	// when it is, nil when it is not. Nil is the contract — a caller reads it
	// as `if state != nil { blocked }` — so a circuit that never opened, one
	// still below the threshold, and one whose cooldown has already expired
	// all report nil, even though the last two still have a durable document.
	// It is the independent read of what RecordRetryFailure wrote: use it to
	// observe another pod's breaker, not to re-read your own return value.
	RetryCircuitOpen(ctx context.Context, key string, now time.Time) (*RetryCircuitState, error)
	// RecordRetrySuccess clears the failure streak after a run completes, so a
	// recovered provider does not keep a stale breaker open forever.
	RecordRetrySuccess(ctx context.Context, key string, now time.Time) error
}

// AsRetryCircuitStore returns the optional retry-circuit capability.
func AsRetryCircuitStore(s RunStore) RetryCircuitStore {
	if s == nil {
		return nil
	}
	c, _ := s.(RetryCircuitStore)
	return c
}

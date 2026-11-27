package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

var _ CapacityTransitStore = (*FilesystemRunStore)(nil)

// BeginCapacityTransit implements CapacityTransitStore under the run's
// metadata lock: the read and the write are one decision, as the Mongo twin's
// conditional update is.
func (s *FilesystemRunStore) BeginCapacityTransit(ctx context.Context, runID string, t CapacityTransit) (CapacityTransitStart, error) {
	if runID == "" || t.Until.IsZero() {
		return CapacityTransitStart{}, fmt.Errorf("store: begin the capacity transit of run %q: run id and the hold's end are required (until=%v)", runID, t.Until)
	}
	r, unlock, err := s.lockExistingRunMetadata(ctx, runID)
	if errors.Is(err, ErrRunNotFound) || errors.Is(err, ErrRunDeleted) {
		// The Mongo twin's filter matches no document either.
		return CapacityTransitStart{}, nil
	}
	if err != nil {
		return CapacityTransitStart{}, fmt.Errorf("store: begin the capacity transit of run %s: %w", runID, err)
	}
	defer unlock()
	if !capacityTransitBeginMatches(r, t) {
		return CapacityTransitStart{}, nil
	}
	prior := r.ContinuationState
	until := TransitInstant(t.Until)
	r.ContinuationState = ContinuationRedeliveryPending
	r.ContinuationUntil = &until
	r.UpdatedAt = time.Now().UTC()
	if err := s.writeRunLockedContext(ctx, r, false, nil); err != nil {
		return CapacityTransitStart{}, fmt.Errorf("store: begin the capacity transit of run %s: %w", runID, err)
	}
	return CapacityTransitStart{Started: true, Prior: prior}, nil
}

// capacityTransitBeginMatches is BeginCapacityTransit's filter: a
// failed_resumable run whose retry is not armed, in the expected transit and
// attempt when the caller names them.
func capacityTransitBeginMatches(r *Run, t CapacityTransit) bool {
	if r.Status != RunStatusFailedResumable || r.ContinuationState == ContinuationRetryArmed {
		return false
	}
	if !MatchesExactQueuedAt(r.QueuedAt, t.ExactQueuedAt) {
		return false
	}
	if !t.ExpectUntil.IsZero() {
		return r.ContinuationState == ContinuationRedeliveryPending && r.ContinuationUntil != nil &&
			r.ContinuationUntil.Equal(TransitInstant(t.ExpectUntil))
	}
	return true
}

// EndCapacityTransit implements CapacityTransitStore under the run's metadata
// lock.
func (s *FilesystemRunStore) EndCapacityTransit(ctx context.Context, runID string, until time.Time, restore ContinuationState) (bool, error) {
	if runID == "" || until.IsZero() {
		return false, fmt.Errorf("store: end the capacity transit of run %q: run id and the transit's end are required (until=%v)", runID, until)
	}
	r, unlock, err := s.lockExistingRunMetadata(ctx, runID)
	if errors.Is(err, ErrRunNotFound) || errors.Is(err, ErrRunDeleted) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: end the capacity transit of run %s: %w", runID, err)
	}
	defer unlock()
	if r.Status != RunStatusFailedResumable || r.ContinuationState != ContinuationRedeliveryPending ||
		r.ContinuationUntil == nil || !r.ContinuationUntil.Equal(TransitInstant(until)) {
		return false, nil
	}
	r.ContinuationState = restore
	r.ContinuationUntil = nil
	r.UpdatedAt = time.Now().UTC()
	if err := s.writeRunLockedContext(ctx, r, false, nil); err != nil {
		return false, fmt.Errorf("store: end the capacity transit of run %s: %w", runID, err)
	}
	return true, nil
}

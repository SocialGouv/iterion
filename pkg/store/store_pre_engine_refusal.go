package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
)

var _ PreEngineRefusalRecorder = (*FilesystemRunStore)(nil)

func (s *FilesystemRunStore) RecordPreEngineRefusal(ctx context.Context, id string, q PreEngineRefusalRequest) (PreEngineRefusalResult, error) {
	if err := q.Validate(); err != nil {
		return PreEngineRefusalResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return PreEngineRefusalResult{}, err
	}
	run, unlock, err := s.lockExistingRunMetadata(ctx, id)
	if err != nil {
		// Snapshot admission now detects a corrupt identity before the
		// recorder sees the row. Preserve its public typed refusal as well.
		if errors.Is(err, ErrRunDataIdentityChanged) {
			return PreEngineRefusalResult{}, fmt.Errorf("store: pre-engine refusal binding: %w: %w", ErrInvalidPreEngineRefusal, err)
		}
		return PreEngineRefusalResult{}, err
	}
	defer unlock()
	if run.ID != id {
		return PreEngineRefusalResult{}, fmt.Errorf("store: pre-engine refusal binding: %w", ErrInvalidPreEngineRefusal)
	}
	// Local filesystem runs historically have no tenant. A persisted tenant
	// is authoritative when one exists; never write another tenant's run.
	if tenant, ok := TenantFromContext(ctx); ok && run.TenantID != "" && tenant != run.TenantID {
		return PreEngineRefusalResult{}, fmt.Errorf("store: pre-engine refusal: %w", ErrRunNotFound)
	}
	if !slices.Contains(PreEngineRefusalFromStatuses(q), run.Status) || !MatchesExactQueuedAt(run.QueuedAt, q.ExactQueuedAt) || (q.ExactQueuedAt.IsZero() && run.QueuedAt != nil && (q.PublishedAt.IsZero() || run.QueuedAt.After(q.PublishedAt))) {
		return PreEngineRefusalResult{}, nil
	}
	pending := q.Disposition == PreEngineRetryPending && run.Status.IsQueued()
	observed := time.Now().UTC()
	if !pending {
		transitionRunStatus(run, q.TargetStatus(), q.Message, q.Outcome())
		if q.Disposition != PreEngineFinalVerdict {
			run.ContinuationState = ""
		}
	}
	run.Error = q.Message
	run.UpdatedAt = observed
	run.PreEngineRefusal = &PreEngineRefusal{Phase: q.Phase, Code: q.Code, Message: q.Message, Hint: q.Hint, PublishedAt: q.PublishedAt, QueuedAt: cloneTimePtr(run.QueuedAt), ObservedAt: observed}
	if err := ctx.Err(); err != nil {
		return PreEngineRefusalResult{}, err
	}
	// The metadata lock owns this read-modify-write; retain the invocation's
	// original format binding through the no-relock persistence tail.
	if err := s.writeRunLockedContext(ctx, run, false, nil); err != nil {
		return PreEngineRefusalResult{}, err
	}
	return PreEngineRefusalResult{Changed: true, Status: run.Status}, nil
}

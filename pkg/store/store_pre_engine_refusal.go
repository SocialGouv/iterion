package store

import (
	"context"
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
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.guardNotDeleted(id); err != nil {
		return PreEngineRefusalResult{}, err
	}
	run, err := s.loadRunRaw(id)
	if err != nil {
		return PreEngineRefusalResult{}, err
	}
	if run.ID != id {
		return PreEngineRefusalResult{}, fmt.Errorf("store: pre-engine refusal binding: %w", ErrInvalidPreEngineRefusal)
	}
	// Local filesystem runs historically have no tenant. A persisted tenant
	// is authoritative when one exists; never write another tenant's run.
	if tenant, ok := TenantFromContext(ctx); ok && run.TenantID != "" && tenant != run.TenantID {
		return PreEngineRefusalResult{}, fmt.Errorf("store: pre-engine refusal: %w", ErrRunNotFound)
	}
	if !slices.Contains(PreEngineRefusalFromStatuses(q), run.Status) || (run.QueuedAt != nil && (q.PublishedAt.IsZero() || run.QueuedAt.After(q.PublishedAt))) {
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
	// writeRun's version CAS, under its cross-instance file lock, rejects a
	// new publication/claim that won after loadRunRaw. No second fallback.
	if err := s.writeRun(run); err != nil {
		return PreEngineRefusalResult{}, err
	}
	return PreEngineRefusalResult{Changed: true, Status: run.Status}, nil
}

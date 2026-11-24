package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
)

// ToolApprovalClaim is owned by the run's resume transition, not its metadata writers.
type ToolApprovalClaim struct {
	InteractionID string `json:"interaction_id" bson:"interaction_id"`
	Token         string `json:"token" bson:"token"`
}

// ToolApprovalSpender records one dispatch against an immutable resume claim.
// The caller may dispatch only on (true, nil). A rejected or uncertain write
// is not retried against a newer revision.
type ToolApprovalSpender interface {
	ClaimToolApprovalResume(ctx context.Context, runID string, claim ToolApprovalClaim, expectedStatus RunStatus, expectedVersion int64) (bool, error)
	SpendToolApproval(ctx context.Context, runID string, claim ToolApprovalClaim) (bool, error)
	CommitToolApprovalContinuation(ctx context.Context, runID string, claim ToolApprovalClaim, cp *Checkpoint) (bool, error)
	GrantOwnedToolPermission(ctx context.Context, runID string, claim ToolApprovalClaim, grants map[string][]string) (bool, error)
	FailOwnedToolApproval(ctx context.Context, runID string, claim ToolApprovalClaim, cp *Checkpoint, reason string, code FailureCode) (FailureCode, error)
}

// AsToolApprovalSpender finds the approval capability through store decorators.
func AsToolApprovalSpender(s RunStore) ToolApprovalSpender { return capability[ToolApprovalSpender](s) }

func (s *FilesystemRunStore) FailOwnedToolApproval(ctx context.Context, id string, claim ToolApprovalClaim, cp *Checkpoint, reason string, code FailureCode) (FailureCode, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	r, unlock, err := s.lockExistingRunMetadata(ctx, id)
	if err != nil {
		return "", err
	}
	defer unlock()
	if r.Status != RunStatusRunning || r.ToolApprovalClaim == nil || *r.ToolApprovalClaim != claim {
		return "", nil
	}
	if r.UnresolvedToolApproval != "" {
		code = FailureAmbiguousEffect
	}
	r.Checkpoint = cp
	err = s.applyStatusTransitionOutcome(ctx, r, RunStatusFailedResumable, reason, RunOutcomeMeta{Code: code, Continuation: ContinuationFinal})
	if errors.Is(err, ErrRunConflict) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return code, nil
}

func (s *FilesystemRunStore) GrantOwnedToolPermission(ctx context.Context, id string, claim ToolApprovalClaim, grants map[string][]string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	r, unlock, err := s.lockExistingRunMetadata(ctx, id)
	if err != nil {
		return false, err
	}
	defer unlock()
	if r.Status != RunStatusRunning || r.ToolApprovalClaim == nil || *r.ToolApprovalClaim != claim {
		return false, nil
	}
	r.PermissionGrants = grants
	if err := s.writeRunLockedContext(ctx, r, false, nil); err != nil {
		return false, err
	}
	return true, nil
}

func (s *FilesystemRunStore) CommitToolApprovalContinuation(ctx context.Context, id string, claim ToolApprovalClaim, cp *Checkpoint) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if cp == nil || cp.NodeID == "" || claim.InteractionID == "" || claim.Token == "" {
		return false, fmt.Errorf("store: approval continuation requires a claim and checkpoint")
	}
	r, unlock, err := s.lockExistingRunMetadata(ctx, id)
	if err != nil {
		return false, err
	}
	defer unlock()
	if r.Status != RunStatusRunning || r.ToolApprovalClaim == nil || *r.ToolApprovalClaim != claim {
		return false, nil
	}
	if cp.InteractionID != "" {
		transitionRunStatus(r, RunStatusPausedWaitingHuman, "", RunOutcomeMeta{})
	}
	r.Checkpoint = cp
	r.UnresolvedToolApproval = ""
	r.ToolApprovalClaim = nil
	r.UpdatedAt = time.Now().UTC()
	if err := s.writeRunLockedContext(ctx, r, false, nil); err != nil {
		return false, err
	}
	return true, nil
}

func (s *FilesystemRunStore) ClaimToolApprovalResume(ctx context.Context, runID string, claim ToolApprovalClaim, expectedStatus RunStatus, expectedVersion int64) (bool, error) {
	if claim.InteractionID == "" || claim.Token == "" || expectedVersion <= 0 || (expectedStatus != RunStatusPausedWaitingHuman && !expectedStatus.IsQueued()) {
		return false, fmt.Errorf("store: tool approval resume requires a paused or queued run revision and an interaction claim")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	r, unlock, err := s.lockExistingRunMetadata(ctx, runID)
	if err != nil {
		return false, err
	}
	defer unlock()
	if r.Status != expectedStatus || r.CASVersion != expectedVersion || r.UnresolvedToolApproval != "" || r.Checkpoint == nil || r.Checkpoint.InteractionID != claim.InteractionID || slices.Contains(r.SpentToolApprovals, claim.InteractionID) {
		return false, nil
	}
	transitionRunStatus(r, RunStatusRunning, "", RunOutcomeMeta{})
	r.ToolApprovalClaim = &claim
	if err := s.writeRunLockedContext(ctx, r, false, nil); err != nil {
		if errors.Is(err, ErrRunConflict) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (s *FilesystemRunStore) SpendToolApproval(ctx context.Context, runID string, claim ToolApprovalClaim) (bool, error) {
	if claim.InteractionID == "" || claim.Token == "" {
		return false, fmt.Errorf("store: tool approval spend requires an interaction claim")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	r, unlock, err := s.lockExistingRunMetadata(ctx, runID)
	if err != nil {
		return false, err
	}
	defer unlock()
	if r.Status != RunStatusRunning || r.ToolApprovalClaim == nil || *r.ToolApprovalClaim != claim || slices.Contains(r.SpentToolApprovals, claim.InteractionID) {
		return false, nil
	}
	r.SpentToolApprovals = append(r.SpentToolApprovals, claim.InteractionID)
	r.UnresolvedToolApproval = claim.InteractionID
	if err := s.writeRunLockedContext(ctx, r, false, nil); err != nil {
		if errors.Is(err, ErrRunConflict) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

var _ ToolApprovalSpender = (*FilesystemRunStore)(nil)

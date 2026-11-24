package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ResumePauseHandoff identifies the pause a quota claim is handing to the
// execution path. The live checkpoint no longer carries a pause pointer once
// claimed. The handoff keeps that identity until its continuation is durable.
// Answers live in the interaction store, not in this marker.
type ResumePauseHandoff struct {
	ID                   string         `json:"id" bson:"id"`
	NodeID               string         `json:"node_id" bson:"node_id"`
	InteractionID        string         `json:"interaction_id" bson:"interaction_id"`
	InteractionQuestions map[string]any `json:"interaction_questions,omitempty" bson:"interaction_questions,omitempty"`
	RecoveryPause        bool           `json:"recovery_pause,omitempty" bson:"recovery_pause,omitempty"`
	RecoveryCode         string         `json:"recovery_code,omitempty" bson:"recovery_code,omitempty"`
}

// ResumePauseBudget carries only accounting across a failed continuation write.
// It cannot replace the original pause envelope, outputs or session references.
// ResumePauseCostReading is an already booked provider session total.
type ResumePauseCostReading struct {
	Backend string  `json:"backend" bson:"backend"`
	Session string  `json:"session" bson:"session"`
	USD     float64 `json:"usd" bson:"usd"`
}

type ResumePauseBudget struct {
	ResumePauseCostReadings []ResumePauseCostReading `bson:"resume_pause_cost_readings"`

	BudgetTokensUsed      int     `bson:"budget_tokens_used"`
	BudgetCostUSD         float64 `bson:"budget_cost_usd"`
	BudgetIterationsUsed  int     `bson:"budget_iterations_used"`
	BudgetElapsedNS       int64   `bson:"budget_elapsed_ns"`
	BudgetUnpricedTokens  int     `bson:"budget_unpriced_tokens"`
	BudgetUnpricedNodes   int     `bson:"budget_unpriced_nodes"`
	BudgetUnreportedCalls int     `bson:"budget_unreported_calls"`
	CostUSDTotal          float64 `bson:"cost_usd_total"`
}

func (b ResumePauseBudget) apply(cp *Checkpoint) {
	cp.ResumePauseCostReadings = b.ResumePauseCostReadings
	cp.BudgetTokensUsed, cp.BudgetCostUSD = b.BudgetTokensUsed, b.BudgetCostUSD
	cp.BudgetIterationsUsed, cp.BudgetElapsedNS = b.BudgetIterationsUsed, b.BudgetElapsedNS
	cp.BudgetUnpricedTokens, cp.BudgetUnpricedNodes = b.BudgetUnpricedTokens, b.BudgetUnpricedNodes
	cp.BudgetUnreportedCalls, cp.CostUSDTotal = b.BudgetUnreportedCalls, b.CostUSDTotal
}

// ResumePauseHandoffStore owns the durable continuation of a claimed pause.
// A false result means the fence no longer matches; no write was made.
type ResumePauseHandoffStore interface {
	// FinishResumePauseHandoff retires a running handoff. A non-nil checkpoint
	// replaces the live checkpoint in the same write, before any effects resume.
	FinishResumePauseHandoff(ctx context.Context, runID, handoffID string, cp *Checkpoint) (bool, error)
	// ReplaceResumePauseHandoff publishes a new pause for the current owner.
	ReplaceResumePauseHandoff(ctx context.Context, runID, handoffID string, cp *Checkpoint) (bool, error)
	// ParkResumePauseHandoff moves a running handoff to failed_resumable while
	// retaining its identity, checkpoint, and claimed quota. reason is safe to persist.
	ParkResumePauseHandoff(ctx context.Context, runID, handoffID, reason string, budget *ResumePauseBudget) (bool, error)
	// ReclaimResumePauseHandoff moves an exact parked or queued handoff to
	// running, without restating its quota, and rotates its fence to newID.
	ReclaimResumePauseHandoff(ctx context.Context, runID, oldID, newID string, expectedStatus RunStatus, expectedVersion int64, queuedAt time.Time) (bool, error)
}

// AsResumePauseHandoffStore finds the handoff capability through decorators.
func AsResumePauseHandoffStore(s RunStore) ResumePauseHandoffStore {
	return capability[ResumePauseHandoffStore](s)
}

// ResumeExecutionClaim binds a specialized resume CAS to the inspected epoch
// and the exact new epoch its caller will use as the execution grant.
type ResumeExecutionClaim struct {
	ExpectedEpoch string
	NextEpoch     string
}

type resumeExecutionClaimKey struct{}

func WithResumeExecutionClaim(ctx context.Context, claim ResumeExecutionClaim) context.Context {
	return context.WithValue(ctx, resumeExecutionClaimKey{}, claim)
}

func ResumeExecutionClaimFromContext(ctx context.Context) (ResumeExecutionClaim, bool) {
	claim, ok := ctx.Value(resumeExecutionClaimKey{}).(ResumeExecutionClaim)
	return claim, ok
}

func (c ResumeExecutionClaim) Validate() error {
	if c.NextEpoch == "" || c.NextEpoch == c.ExpectedEpoch {
		return errors.New("resume execution claim requires a new nonempty epoch")
	}
	return nil
}

// ValidateResumePauseReclaim validates the fence operands shared by the stores.
func ValidateResumePauseReclaim(oldID, newID string, status RunStatus, version int64, queuedAt time.Time) error {
	if oldID == "" || newID == "" || oldID == newID {
		return errors.New("resume pause reclaim requires distinct nonempty handoff IDs")
	}
	if version < 1 {
		return errors.New("resume pause reclaim requires the inspected run version")
	}
	if !status.CanReclaimResumePauseHandoff() {
		return fmt.Errorf("resume pause reclaim cannot claim status %q", status)
	}
	if status.IsQueued() {
		if queuedAt.IsZero() || !queuedAt.Equal(QueuedFlipAt(queuedAt)) {
			return errors.New("resume pause reclaim requires the exact queued attempt timestamp at millisecond precision")
		}
	} else if !queuedAt.IsZero() {
		return errors.New("resume pause reclaim of a parked run does not take a queued attempt timestamp")
	}
	return nil
}

var _ ResumePauseHandoffStore = (*FilesystemRunStore)(nil)

func (s *FilesystemRunStore) FinishResumePauseHandoff(ctx context.Context, runID, handoffID string, cp *Checkpoint) (bool, error) {
	if cp != nil && (cp.InteractionID != "" || len(cp.InteractionQuestions) != 0) {
		return false, errors.New("finish resume pause handoff: running checkpoint cannot carry a pause pointer")
	}
	return s.updateResumePauseHandoff(ctx, runID, handoffID, RunStatusRunning, func(r *Run) bool {
		r.ResumePauseHandoff = nil
		if cp != nil {
			r.Checkpoint = cp
		}
		r.UpdatedAt = time.Now().UTC()
		return true
	})
}

func (s *FilesystemRunStore) ReplaceResumePauseHandoff(ctx context.Context, runID, handoffID string, cp *Checkpoint) (bool, error) {
	if cp == nil || cp.InteractionID == "" {
		return false, errors.New("replace resume pause handoff: pending interaction is required")
	}
	return s.updateResumePauseHandoff(ctx, runID, handoffID, RunStatusRunning, func(r *Run) bool {
		if r.ResumePauseHandoff.InteractionID == cp.InteractionID {
			return false
		}
		transitionRunStatus(r, RunStatusPausedWaitingHuman, "", RunOutcomeMeta{})
		r.Checkpoint = cp
		return true
	})
}

func (s *FilesystemRunStore) ParkResumePauseHandoff(ctx context.Context, runID, handoffID, reason string, budget *ResumePauseBudget) (bool, error) {
	return s.updateResumePauseHandoff(ctx, runID, handoffID, RunStatusRunning, func(r *Run) bool {
		if budget != nil && r.Checkpoint != nil {
			budget.apply(r.Checkpoint)
		}
		transitionRunStatus(r, RunStatusFailedResumable, reason, RunOutcomeMeta{Code: FailureInterrupted})
		return true
	})
}

func (s *FilesystemRunStore) ReclaimResumePauseHandoff(ctx context.Context, runID, oldID, newID string, expectedStatus RunStatus, expectedVersion int64, queuedAt time.Time) (bool, error) {
	if err := ValidateResumePauseReclaim(oldID, newID, expectedStatus, expectedVersion, queuedAt); err != nil {
		return false, err
	}
	claim, hasClaim := ResumeExecutionClaimFromContext(ctx)
	if hasClaim {
		if err := claim.Validate(); err != nil {
			return false, err
		}
	}
	return s.updateResumePauseHandoff(ctx, runID, oldID, expectedStatus, func(r *Run) bool {
		if r.CASVersion != expectedVersion || (expectedStatus == RunStatusQueued && (r.QueuedAt == nil || !r.QueuedAt.Equal(queuedAt))) {
			return false
		}
		transitionRunStatus(r, RunStatusRunning, "", RunOutcomeMeta{})
		r.ResumePauseHandoff.ID = newID
		return true
	})
}

func (s *FilesystemRunStore) updateResumePauseHandoff(ctx context.Context, runID, handoffID string, expectedStatus RunStatus, apply func(*Run) bool) (bool, error) {
	if handoffID == "" {
		return false, errors.New("update resume pause handoff: handoff ID is required")
	}
	r, unlock, err := s.lockExistingRunMetadata(ctx, runID)
	if errors.Is(err, ErrRunNotFound) || errors.Is(err, ErrRunDeleted) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer unlock()
	if r.Status != expectedStatus || r.ResumePauseHandoff == nil || r.ResumePauseHandoff.ID != handoffID {
		return false, nil
	}
	var expectedEpoch *string
	var nextEpoch string
	if claim, ok := ResumeExecutionClaimFromContext(ctx); ok && expectedStatus.CanReclaimResumePauseHandoff() {
		if r.ExecutionEpoch != claim.ExpectedEpoch {
			return false, nil
		}
		expectedEpoch, nextEpoch = &claim.ExpectedEpoch, claim.NextEpoch
	}
	if !apply(r) {
		return false, nil
	}
	err = s.writeRunLockedWithNextEpoch(ctx, r, nextEpoch != "", expectedEpoch, nextEpoch)
	return err == nil, err
}

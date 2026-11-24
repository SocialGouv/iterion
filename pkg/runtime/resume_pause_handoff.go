package runtime

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/google/uuid"
)

func (e *Engine) pauseHandoffForClaim(r *store.Run, cp *store.Checkpoint) *store.ResumePauseHandoff {
	if cp == nil || cp.InteractionID == "" || (r.ResumePauseHandoff == nil && e.resumeCodemodeQuota == (store.CodemodeScratchLimits{})) {
		return nil
	}
	if node, ok := e.workflow.Nodes[cp.NodeID].(*ir.HumanNode); ok && node.Interaction == ir.InteractionReview {
		return nil
	}
	return &store.ResumePauseHandoff{ID: uuid.NewString(), NodeID: cp.NodeID,
		InteractionID: cp.InteractionID, InteractionQuestions: cp.InteractionQuestions,
		RecoveryPause: cp.RecoveryPause, RecoveryCode: cp.RecoveryCode}
}

func (e *Engine) finishResumePauseHandoff(ctx context.Context, r *store.Run, cp *store.Checkpoint) error {
	if r.ResumePauseHandoff == nil {
		return nil
	}
	s := store.AsResumePauseHandoffStore(e.store)
	if s == nil {
		return errors.New("runtime: store cannot finish a resume pause handoff")
	}
	changed, err := s.FinishResumePauseHandoff(ctx, r.ID, r.ResumePauseHandoff.ID, cp)
	if err != nil {
		return fmt.Errorf("runtime: finish resume pause handoff: %w", err)
	}
	if !changed {
		return store.ErrRunConflict
	}
	r.ResumePauseHandoff = nil
	return nil
}

func (e *Engine) parkResumePauseHandoff(ctx context.Context, r *store.Run, cause error) error {
	s := store.AsResumePauseHandoffStore(e.store)
	if s == nil {
		return errors.Join(cause, errors.New("runtime: store cannot park a resume pause handoff"))
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), resumeParkWriteBudget)
	defer cancel()
	const reason = "resume pause handoff did not complete"
	changed, err := s.ParkResumePauseHandoff(writeCtx, r.ID, r.ResumePauseHandoff.ID, reason, e.resumePauseBudget)
	if changed {
		e.emitSetupFailure(writeCtx, r.ID, "resume pause handoff", store.RunStatusFailedResumable, reason, store.FailureInterrupted, Remedy{})
	}
	return errors.Join(cause, err)
}

func (e *Engine) reclaimResumePauseHandoff(ctx context.Context, r *store.Run, next *store.ResumePauseHandoff) (bool, error) {
	if next == nil {
		return false, errors.New("runtime: cannot resume a handoff without its original pause")
	}
	s := store.AsResumePauseHandoffStore(e.store)
	if s == nil {
		return false, errors.New("runtime: store cannot reclaim a resume pause handoff")
	}
	var queuedAt time.Time
	if r.Status == store.RunStatusQueued && r.QueuedAt != nil {
		queuedAt = *r.QueuedAt
	}
	changed, err := s.ReclaimResumePauseHandoff(ctx, r.ID, r.ResumePauseHandoff.ID, next.ID, r.Status, r.CASVersion, queuedAt)
	if changed && err == nil {
		r.ResumePauseHandoff = next
	}
	return changed, err
}

func restoreResumePauseHandoff(r *store.Run) error {
	marker := r.ResumePauseHandoff
	if r.Checkpoint == nil || marker.NodeID != r.Checkpoint.NodeID || marker.InteractionID == "" {
		return fmt.Errorf("runtime: run %q has a pause handoff without its original checkpoint", r.ID)
	}
	cp := *r.Checkpoint
	cp.InteractionID, cp.InteractionQuestions = marker.InteractionID, marker.InteractionQuestions
	cp.RecoveryPause, cp.RecoveryCode = marker.RecoveryPause, marker.RecoveryCode
	r.Checkpoint = &cp
	return nil
}

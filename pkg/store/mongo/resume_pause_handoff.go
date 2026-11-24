package mongo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/bson"
)

const resumePauseHandoffField = "resume_pause_handoff"

var _ store.ResumePauseHandoffStore = (*Store)(nil)

func (s *Store) FinishResumePauseHandoff(ctx context.Context, runID, handoffID string, cp *store.Checkpoint) (bool, error) {
	if cp != nil && (cp.InteractionID != "" || len(cp.InteractionQuestions) != 0) {
		return false, errors.New("finish resume pause handoff: running checkpoint cannot carry a pause pointer")
	}
	set := bson.M{resumePauseHandoffField: "$$REMOVE", "updated_at": time.Now().UTC()}
	return s.updateResumePauseHandoff(ctx, "finish", resumePauseHandoffFilter(runID, handoffID, store.RunStatusRunning), set, cp)
}

func (s *Store) ReplaceResumePauseHandoff(ctx context.Context, runID, handoffID string, cp *store.Checkpoint) (bool, error) {
	if cp == nil || cp.InteractionID == "" {
		return false, errors.New("replace resume pause handoff: pending interaction is required")
	}
	filter := resumePauseHandoffFilter(runID, handoffID, store.RunStatusRunning)
	filter[resumePauseHandoffField+".interaction_id"] = bson.M{"$ne": cp.InteractionID}
	set := statusTransitionSet(store.RunStatusPausedWaitingHuman, "", store.RunOutcomeMeta{}, time.Now().UTC())
	return s.updateResumePauseHandoff(ctx, "replace", filter, set, cp)
}

func (s *Store) ParkResumePauseHandoff(ctx context.Context, runID, handoffID, reason string, budget *store.ResumePauseBudget) (bool, error) {
	set := statusTransitionSet(store.RunStatusFailedResumable, reason, store.RunOutcomeMeta{Code: store.FailureInterrupted}, time.Now().UTC())
	if budget != nil {
		cp := set["checkpoint"]
		set["checkpoint"] = bson.M{"$cond": bson.A{
			bson.M{"$eq": bson.A{bson.M{"$type": "$checkpoint"}, "object"}},
			bson.M{"$mergeObjects": bson.A{cp, bson.M{"$literal": budget}}},
			cp,
		}}
	}
	return s.updateResumePauseHandoff(ctx, "park", resumePauseHandoffFilter(runID, handoffID, store.RunStatusRunning), set, nil)
}

func (s *Store) ReclaimResumePauseHandoff(ctx context.Context, runID, oldID, newID string, expectedStatus store.RunStatus, expectedVersion int64, queuedAt time.Time) (bool, error) {
	if err := store.ValidateResumePauseReclaim(oldID, newID, expectedStatus, expectedVersion, queuedAt); err != nil {
		return false, err
	}
	filter := resumePauseHandoffFilter(runID, oldID, expectedStatus)
	filter["version"] = expectedVersion
	if expectedStatus == store.RunStatusQueued {
		filter["queued_at"] = queuedAt
	}
	set := statusTransitionSet(store.RunStatusRunning, "", store.RunOutcomeMeta{}, time.Now().UTC())
	set[resumePauseHandoffField+".id"] = bson.M{"$literal": newID}
	if claim, ok := store.ResumeExecutionClaimFromContext(ctx); ok {
		if err := claim.Validate(); err != nil {
			return false, err
		}
		filter["$and"] = bson.A{correctionEpochFilter(claim.ExpectedEpoch)}
		set["execution_epoch"] = bson.M{"$literal": claim.NextEpoch}
	}
	return s.updateResumePauseHandoff(ctx, "reclaim", filter, set, nil)
}

func resumePauseHandoffFilter(runID, handoffID string, status store.RunStatus) bson.M {
	return bson.M{"_id": runID, resumePauseHandoffField + ".id": handoffID, "status": status}
}

func (s *Store) updateResumePauseHandoff(ctx context.Context, op string, filter, set bson.M, cp *store.Checkpoint) (bool, error) {
	if filter[resumePauseHandoffField+".id"] == "" {
		return false, errors.New("update resume pause handoff: handoff ID is required")
	}
	id := filter["_id"].(string)
	guard, err := s.prepareRunMetadataFormat(ctx, id)
	if errors.Is(err, store.ErrRunDeleted) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if cp != nil {
		whole, _, err := checkpointValueDocuments(ctx, guard.original.Format, cp)
		if err != nil {
			return false, err
		}
		set["checkpoint"] = bson.M{"$literal": whole}
	}
	if s.afterRunMetadataPrepare != nil {
		s.afterRunMetadataPrepare(id)
	}
	res, err := s.runs.UpdateOne(ctx, guard.filter(ctx, filter), versionRunUpdate(statusTransitionPipeline(set)))
	if err != nil {
		return false, fmt.Errorf("store/mongo: %s resume pause handoff for run %s: %w", op, filter["_id"], err)
	}
	if res.MatchedCount == 0 {
		return false, guard.checkMiss(ctx, s)
	}
	return true, nil
}

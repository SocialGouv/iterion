package mongo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/writeconcern"
)

func (s *Store) ClaimToolApprovalResume(ctx context.Context, runID string, claim store.ToolApprovalClaim, expectedStatus store.RunStatus, expectedVersion int64) (bool, error) {
	if claim.InteractionID == "" || claim.Token == "" || expectedVersion <= 0 || (expectedStatus != store.RunStatusPausedWaitingHuman && !expectedStatus.IsQueued()) {
		return false, fmt.Errorf("store/mongo: tool approval resume requires a paused or queued run revision and an interaction claim")
	}
	filter := notDeleted(withTenantFilter(ctx, bson.M{"_id": runID}))
	filter["status"] = expectedStatus
	filter["version"] = expectedVersion
	filter["unresolved_tool_approval"] = bson.M{"$exists": false}
	filter["checkpoint.interaction_id"] = claim.InteractionID
	filter["spent_tool_approvals"] = bson.M{"$ne": claim.InteractionID}
	set := statusTransitionSet(store.RunStatusRunning, "", store.RunOutcomeMeta{}, time.Now().UTC())
	set["tool_approval_claim"] = bson.M{"$literal": claim}
	result, err := s.runs.UpdateOne(ctx, filter, versionRunUpdate(statusTransitionPipeline(set)))
	if err != nil {
		return false, fmt.Errorf("store/mongo: claim tool approval resume %s: %w", runID, err)
	}
	return result.MatchedCount == 1, nil
}

func (s *Store) SpendToolApproval(ctx context.Context, runID string, claim store.ToolApprovalClaim) (bool, error) {
	if claim.InteractionID == "" || claim.Token == "" {
		return false, fmt.Errorf("store/mongo: tool approval spend requires an interaction claim")
	}
	filter := notDeleted(withTenantFilter(ctx, bson.M{"_id": runID}))
	filter["status"] = store.RunStatusRunning
	filter["tool_approval_claim.interaction_id"] = claim.InteractionID
	filter["tool_approval_claim.token"] = claim.Token
	filter["spent_tool_approvals"] = bson.M{"$ne": claim.InteractionID}
	// A primary failover must not roll back the spent record after dispatch.
	runs := s.runs.Clone(options.Collection().SetWriteConcern(writeconcern.Majority()))
	result, err := runs.UpdateOne(ctx, filter, bson.M{
		"$addToSet": bson.M{"spent_tool_approvals": claim.InteractionID},
		"$set":      bson.M{"unresolved_tool_approval": claim.InteractionID},
		"$inc":      bson.M{"version": 1},
	})
	if err != nil {
		return false, fmt.Errorf("store/mongo: spend tool approval %s/%s: %w", runID, claim.InteractionID, err)
	}
	return result.MatchedCount == 1, nil
}

func (s *Store) CommitToolApprovalContinuation(ctx context.Context, id string, claim store.ToolApprovalClaim, cp *store.Checkpoint) (bool, error) {
	if cp == nil || cp.NodeID == "" || claim.InteractionID == "" || claim.Token == "" {
		return false, fmt.Errorf("store/mongo: approval continuation requires a claim and checkpoint")
	}
	filter := notDeleted(withTenantFilter(ctx, bson.M{"_id": id, "status": store.RunStatusRunning}))
	filter["tool_approval_claim.interaction_id"] = claim.InteractionID
	filter["tool_approval_claim.token"] = claim.Token
	runs := s.runs.Clone(options.Collection().SetWriteConcern(writeconcern.Majority()))
	format, err := s.valueWriteFormat(ctx, id)
	if err != nil {
		return false, err
	}
	whole, _, err := checkpointValueDocuments(ctx, format, cp)
	if err != nil {
		return false, err
	}
	filter["$and"] = bson.A{valueFormatPredicate(format)}
	set := bson.M{"checkpoint": whole, "updated_at": time.Now().UTC()}
	unset := bson.M{"unresolved_tool_approval": "", "tool_approval_claim": ""}
	if cp.InteractionID != "" {
		set["status"] = store.RunStatusPausedWaitingHuman
		set["execution_epoch"] = store.NewExecutionEpoch()
		for _, key := range []string{"finished_at", "failure_code", "continuation_state", "continuation_until", "await_answers_waits", "pre_engine_refusal"} {
			unset[key] = ""
		}
	}
	result, err := runs.UpdateOne(ctx, filter, bson.M{"$set": set, "$unset": unset, "$inc": bson.M{"version": 1}})
	if err != nil {
		return false, fmt.Errorf("store/mongo: commit approval continuation %s: %w", id, err)
	}
	return result.MatchedCount == 1, nil
}

func (s *Store) GrantOwnedToolPermission(ctx context.Context, id string, claim store.ToolApprovalClaim, grants map[string][]string) (bool, error) {
	filter := notDeleted(withTenantFilter(ctx, bson.M{"_id": id, "status": store.RunStatusRunning,
		"tool_approval_claim.interaction_id": claim.InteractionID, "tool_approval_claim.token": claim.Token}))
	result, err := s.runs.UpdateOne(ctx, filter, bson.M{
		"$set": bson.M{"permission_grants": grants}, "$inc": bson.M{"version": 1},
	})
	if err != nil {
		return false, fmt.Errorf("store/mongo: grant owned tool permission %s: %w", id, err)
	}
	return result.MatchedCount == 1, nil
}

func (s *Store) FailOwnedToolApproval(ctx context.Context, id string, claim store.ToolApprovalClaim, cp *store.Checkpoint, reason string, code store.FailureCode) (store.FailureCode, error) {
	filter := notDeleted(withTenantFilter(ctx, bson.M{"_id": id, "status": store.RunStatusRunning,
		"tool_approval_claim.interaction_id": claim.InteractionID, "tool_approval_claim.token": claim.Token}))
	set := statusTransitionSet(store.RunStatusFailedResumable, reason, store.RunOutcomeMeta{Code: code, Continuation: store.ContinuationFinal}, time.Now().UTC())
	format, err := s.valueWriteFormat(ctx, id)
	if err != nil {
		return "", err
	}
	_, consumed, err := checkpointValueDocuments(ctx, format, cp)
	if err != nil {
		return "", err
	}
	filter["$and"] = bson.A{valueFormatPredicate(format)}
	set["checkpoint"] = bson.M{"$literal": consumed}
	set["failure_code"] = bson.M{"$cond": bson.A{
		bson.M{"$ne": bson.A{bson.M{"$ifNull": bson.A{"$unresolved_tool_approval", ""}}, ""}},
		store.FailureAmbiguousEffect, bson.M{"$literal": code},
	}}
	var after struct {
		Code store.FailureCode `bson:"failure_code"`
	}
	result, err := s.findOneAndUpdateRunMetadata(ctx, id, filter, versionRunUpdate(statusTransitionPipeline(set)), options.FindOneAndUpdate().SetReturnDocument(options.After).SetProjection(bson.M{"failure_code": 1}))
	if err == nil {
		err = result.Decode(&after)
	}
	if errors.Is(err, mongo.ErrNoDocuments) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store/mongo: fail owned approval %s: %w", id, err)
	}
	return after.Code, nil
}

var _ store.ToolApprovalSpender = (*Store)(nil)

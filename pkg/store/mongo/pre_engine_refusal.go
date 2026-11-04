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
)

var _ store.PreEngineRefusalRecorder = (*Store)(nil)

func (s *Store) RecordPreEngineRefusal(ctx context.Context, id string, q store.PreEngineRefusalRequest) (store.PreEngineRefusalResult, error) {
	if err := q.Validate(); err != nil {
		return store.PreEngineRefusalResult{}, err
	}
	if id == "" {
		return store.PreEngineRefusalResult{}, store.ErrInvalidPreEngineRefusal
	}
	attempts := bson.A{bson.M{"queued_at": bson.M{"$exists": false}}, bson.M{"queued_at": nil}}
	if !q.PublishedAt.IsZero() {
		attempts = append(attempts, bson.M{"queued_at": bson.M{"$lte": q.PublishedAt}})
	}
	filter := notDeleted(withTenantFilter(ctx, bson.M{"_id": id, "status": bson.M{"$in": store.PreEngineRefusalFromStatuses(q)}, "$or": attempts}))
	now := time.Now().UTC()
	set := statusTransitionSet(q.TargetStatus(), q.Message, q.Outcome(), now)
	if q.Disposition != store.PreEngineFinalVerdict {
		set["continuation_state"] = "$$REMOVE"
	}
	if q.Disposition == store.PreEngineRetryPending {
		pending := bson.M{"$eq": bson.A{"$status", string(store.RunStatusQueued)}}
		for field, value := range set {
			// Only the diagnostic, error, update time and CAS version change on
			// pending retries. In particular the checkpoint never takes the
			// failed-resumable transition's consumed-question projection.
			if field == "error" || field == "updated_at" || field == "version" {
				continue
			}
			set[field] = bson.M{"$cond": bson.A{pending, bson.M{"$ifNull": bson.A{"$" + field, "$$REMOVE"}}, value}}
		}
	}
	diagnostic := bson.M{"phase": q.Phase, "code": string(q.Code), "message": q.Message, "hint": q.Hint, "observed_at": now}
	if !q.PublishedAt.IsZero() {
		diagnostic["published_at"] = q.PublishedAt
	}
	set["pre_engine_refusal"] = bson.M{"$mergeObjects": bson.A{bson.M{"$literal": diagnostic}, bson.M{"queued_at": bson.M{"$ifNull": bson.A{"$queued_at", "$$REMOVE"}}}}}
	var after struct {
		Status store.RunStatus `bson:"status"`
	}
	err := s.runs.FindOneAndUpdate(ctx, filter, statusTransitionPipeline(set), options.FindOneAndUpdate().SetReturnDocument(options.After).SetProjection(bson.M{"status": 1})).Decode(&after)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return store.PreEngineRefusalResult{}, nil
	}
	if err != nil {
		return store.PreEngineRefusalResult{}, fmt.Errorf("store/mongo: pre-engine refusal %s: %w", id, err)
	}
	return store.PreEngineRefusalResult{Changed: true, Status: after.Status}, nil
}

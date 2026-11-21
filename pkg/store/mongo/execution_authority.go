package mongo

import (
	"context"
	"errors"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func executionEpochExpr(changed any) bson.M {
	return bson.M{"$cond": bson.A{changed, bson.M{"$literal": store.NewExecutionEpoch()}, bson.M{"$ifNull": bson.A{"$execution_epoch", ""}}}}
}

func correctionVersionFilter(version int64) bson.M {
	if version == 0 {
		return bson.M{"$or": bson.A{bson.M{"version": 0}, bson.M{"version": bson.M{"$exists": false}}}}
	}
	return bson.M{"version": version}
}

func correctionEpochFilter(epoch string) bson.M {
	if epoch == "" {
		return bson.M{"$or": bson.A{bson.M{"execution_epoch": ""}, bson.M{"execution_epoch": bson.M{"$exists": false}}}}
	}
	return bson.M{"execution_epoch": epoch}
}

func (s *Store) AcquireRunExecutionIf(ctx context.Context, id string, expected store.RunExecutionExpectation, intent store.RunExecutionIntent) (store.RunExecutionAuthority, bool, error) {
	if err := expected.Validate(intent); err != nil {
		return store.RunExecutionAuthority{}, false, err
	}
	// Exact queued_at plus the publication upper bound preserve the existing
	// queued-attempt identity, including legacy absent markers.
	conditions := bson.A{correctionVersionFilter(expected.Version), correctionEpochFilter(expected.Epoch)}
	if !expected.PublishedAt.IsZero() && expected.Status == store.RunStatusQueued {
		conditions = append(conditions, bson.M{"$or": bson.A{bson.M{"queued_at": nil}, bson.M{"queued_at": bson.M{"$lte": expected.PublishedAt}}}})
	}
	filter := notDeleted(withTenantFilter(ctx, bson.M{"_id": id, "status": expected.Status, "queued_at": expected.QueuedAt, "$and": conditions}))
	set := statusTransitionSet(store.RunStatusRunning, "", store.RunOutcomeMeta{}, time.Now().UTC())
	// Explicit acquisition renews even a precreated running run.
	set["execution_epoch"] = bson.M{"$literal": store.NewExecutionEpoch()}
	var after struct {
		Epoch string `bson:"execution_epoch"`
	}
	metadataResult, err := s.findOneAndUpdateRunMetadata(ctx, id, filter, statusTransitionPipeline(set), options.FindOneAndUpdate().SetReturnDocument(options.After).SetProjection(bson.M{"execution_epoch": 1}))
	if err == nil {
		err = metadataResult.Decode(&after)
	}
	if errors.Is(err, mongo.ErrNoDocuments) {
		return store.RunExecutionAuthority{}, false, nil
	}
	if err != nil {
		return store.RunExecutionAuthority{}, false, err
	}
	return store.RunExecutionAuthority{RunID: id, Epoch: after.Epoch}, true, nil
}

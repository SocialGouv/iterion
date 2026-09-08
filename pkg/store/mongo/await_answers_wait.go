package mongo

import (
	"context"
	"fmt"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func (s *Store) SetAwaitAnswersWait(ctx context.Context, runID, token string, wait *store.AwaitAnswersWait) error {
	if err := store.ValidateAwaitAnswersWait(token, wait); err != nil {
		return err
	}
	filter := notDeleted(withTenantFilter(ctx, bson.M{"_id": runID}))
	set := bson.M{"updated_at": time.Now().UTC()}
	update := bson.M{"$set": set}
	key := "await_answers_waits." + token
	if wait != nil {
		filter["status"] = store.RunStatusRunning
		set[key] = *wait
	} else {
		update["$unset"] = bson.M{key: ""}
	}
	res, err := s.runs.UpdateOne(ctx, filter, versionRunUpdate(update))
	if err != nil {
		return fmt.Errorf("store/mongo: update await_answers wait: %w", err)
	}
	if res.MatchedCount == 0 {
		return fmt.Errorf("store/mongo: run %s missing or no longer running", runID)
	}
	return nil
}

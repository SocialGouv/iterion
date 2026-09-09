package mongo

import (
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/SocialGouv/iterion/pkg/store"
)

// SetWatcherCursor updates one supervisor cursor with a targeted Mongo
// update, preserving concurrent run lifecycle writes.
func (s *Store) SetWatcherCursor(ctx context.Context, id, watcherID string, cursor store.WatcherCursor) error {
	if watcherID == "" {
		return fmt.Errorf("store/mongo: watcher cursor id is empty")
	}
	res, err := s.runs.UpdateOne(ctx, notDeleted(withTenantFilter(ctx, bson.M{"_id": id})), versionRunUpdate(bson.M{
		"$set": bson.M{
			"watcher_cursors." + watcherID: cursor,
			"updated_at":                   time.Now().UTC(),
		},
	}))
	if err != nil {
		return fmt.Errorf("store/mongo: set watcher cursor: %w", err)
	}
	if res.MatchedCount == 0 {
		return store.ErrRunNotFound
	}
	return nil
}

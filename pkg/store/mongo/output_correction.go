package mongo

import (
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/SocialGouv/iterion/pkg/store"
)

// SetRunOutputCorrection updates one node's episode with a targeted Mongo
// $set. The run status, checkpoint, steering and cancel fields are untouched
// even when another authority writes them concurrently.
func (s *Store) SetRunOutputCorrection(ctx context.Context, id, ledgerKey string, episode store.OutputCorrectionEpisode) error {
	if ledgerKey == "" {
		return fmt.Errorf("store/mongo: output correction ledger key is empty")
	}
	update := bson.M{"$set": bson.M{
		"output_corrections." + ledgerKey: episode,
		"updated_at":                      time.Now().UTC(),
	}}
	res, err := s.runs.UpdateOne(ctx, notDeleted(withTenantFilter(ctx, bson.M{"_id": id})), versionRunUpdate(update))
	if err != nil {
		return fmt.Errorf("store/mongo: set output correction: %w", err)
	}
	if res.MatchedCount == 0 {
		return store.ErrRunNotFound
	}
	return nil
}

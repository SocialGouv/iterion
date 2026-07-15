package mongo

import (
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/store"
)

// The cloud (Mongo) store satisfies the run-tag seam so the studio run
// header shows + edits tags identically in cloud and local mode.
var _ store.RunTagStore = (*Store)(nil)

// runTagsDoc is the persisted tag set for a run, one document per run
// (unique run_id). The cloud twin of the filesystem store's
// runs/<id>/tags.json: a whole-list overwrite upserted on every PUT.
// Tenant-stamped like run_gitmeta so a tenant only ever reads its own tags.
type runTagsDoc struct {
	TenantID  string    `bson:"tenant_id,omitempty"`
	RunID     string    `bson:"run_id"`
	Tags      []string  `bson:"tags"`
	UpdatedAt time.Time `bson:"updated_at"`
}

// SetRunTags implements store.RunTagStore: upsert the whole tag list keyed
// on run_id. Re-saving replaces the prior set (never merges). tags is
// assumed already normalized by the caller (see store.NormalizeTags).
func (s *Store) SetRunTags(ctx context.Context, runID string, tags []string) error {
	if tags == nil {
		tags = []string{}
	}
	doc := runTagsDoc{
		RunID:     runID,
		Tags:      tags,
		UpdatedAt: time.Now().UTC(),
	}
	if id, ok := store.TenantFromContext(ctx); ok {
		doc.TenantID = id
	}
	filter := withTenantFilter(ctx, bson.M{"run_id": runID})
	if _, err := s.runTags.ReplaceOne(ctx, filter, doc, options.Replace().SetUpsert(true)); err != nil {
		return fmt.Errorf("store/mongo: set run tags %s: %w", runID, err)
	}
	return nil
}

// GetRunTags implements store.RunTagStore: the tag set for the run, or an
// empty slice when none was ever recorded.
func (s *Store) GetRunTags(ctx context.Context, runID string) ([]string, error) {
	filter := withTenantFilter(ctx, bson.M{"run_id": runID})
	var doc runTagsDoc
	if err := s.runTags.FindOne(ctx, filter).Decode(&doc); err != nil {
		if err == mongo.ErrNoDocuments {
			return []string{}, nil
		}
		return nil, fmt.Errorf("store/mongo: get run tags %s: %w", runID, err)
	}
	if doc.Tags == nil {
		return []string{}, nil
	}
	return doc.Tags, nil
}

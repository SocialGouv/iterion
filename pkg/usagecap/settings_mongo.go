package usagecap

import (
	"errors"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"context"
)

// colPlatformSettings holds the platform-scoped runtime-settings records —
// one document per settings family, keyed by a fixed _id, so a future
// family (say retry ceilings) is a new document, not a schema change.
const colPlatformSettings = "platform_settings"

// settingsDocID is the usage-cap family's document.
const settingsDocID = "usage_caps"

// MongoSettingsStore is the cloud SettingsStore: the single document every
// replica of both deployments (server + runner) reads, which is what ends
// the "one deployment rolled, the other not" divergence the env-only path
// allowed.
type MongoSettingsStore struct{ col *mongo.Collection }

// NewMongoSettingsStore binds the store to a database.
func NewMongoSettingsStore(db *mongo.Database) *MongoSettingsStore {
	return &MongoSettingsStore{col: db.Collection(colPlatformSettings)}
}

type settingsDoc struct {
	ID          string    `bson:"_id"`
	FiveHourPct *int      `bson:"five_hour_pct,omitempty"`
	WeekPct     *int      `bson:"week_pct,omitempty"`
	UpdatedAt   time.Time `bson:"updated_at"`
	UpdatedBy   string    `bson:"updated_by,omitempty"`
}

// GetSettings returns the record, or (nil, nil) when none exists yet.
func (s *MongoSettingsStore) GetSettings(ctx context.Context) (*Settings, error) {
	var doc settingsDoc
	err := s.col.FindOne(ctx, bson.M{"_id": settingsDocID}).Decode(&doc)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, nil
		}
		return nil, fmt.Errorf("usagecap: get settings: %w", err)
	}
	return &Settings{
		FiveHourPct: doc.FiveHourPct,
		WeekPct:     doc.WeekPct,
		UpdatedAt:   doc.UpdatedAt,
		UpdatedBy:   doc.UpdatedBy,
	}, nil
}

// PutSettings replaces the record. ReplaceOne (not $set) so a cleared
// override really disappears from the document instead of lingering.
func (s *MongoSettingsStore) PutSettings(ctx context.Context, rec Settings) error {
	if rec.UpdatedAt.IsZero() {
		rec.UpdatedAt = time.Now().UTC()
	}
	doc := settingsDoc{
		ID:          settingsDocID,
		FiveHourPct: rec.FiveHourPct,
		WeekPct:     rec.WeekPct,
		UpdatedAt:   rec.UpdatedAt.UTC(),
		UpdatedBy:   rec.UpdatedBy,
	}
	_, err := s.col.ReplaceOne(ctx, bson.M{"_id": settingsDocID}, doc,
		options.Replace().SetUpsert(true))
	if err != nil {
		return fmt.Errorf("usagecap: put settings: %w", err)
	}
	return nil
}

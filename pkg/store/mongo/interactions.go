package mongo

import (
	"context"
	"errors"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/store"
)

// Composite _id for the interactions collection. Plan §D.4.
type interactionID struct {
	RunID         string `bson:"run_id"`
	InteractionID string `bson:"interaction_id"`
}

type interactionDoc struct {
	ID                interactionID `bson:"_id"`
	store.Interaction `bson:",inline"`
}

// WriteInteraction upserts the interaction document. Two paths:
// the initial pause writes the questions, and the resume path writes
// the answers; both go through this single method.
func (s *Store) WriteInteraction(ctx context.Context, i *store.Interaction) error {
	doc := interactionDoc{
		ID: interactionID{
			RunID:         i.RunID,
			InteractionID: i.ID,
		},
		Interaction: *i,
	}
	_, err := s.interactions.ReplaceOne(
		ctx,
		bson.M{"_id": doc.ID},
		doc,
		options.Replace().SetUpsert(true),
	)
	if err != nil {
		return fmt.Errorf("store/mongo: write interaction %s/%s: %w", i.RunID, i.ID, err)
	}
	return nil
}

// LoadInteraction looks up the composite key directly.
func (s *Store) LoadInteraction(ctx context.Context, runID, interactionID2 string) (*store.Interaction, error) {
	var doc interactionDoc
	err := s.interactions.FindOne(ctx, bson.M{"_id": interactionID{RunID: runID, InteractionID: interactionID2}}).Decode(&doc)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, fmt.Errorf("store/mongo: interaction %s/%s not found", runID, interactionID2)
		}
		return nil, fmt.Errorf("store/mongo: load interaction %s/%s: %w", runID, interactionID2, err)
	}
	out := doc.Interaction
	out.RunID = runID
	out.ID = interactionID2
	return &out, nil
}

// ListInteractions returns the interaction ids for a run, in
// requested-at order. Mirrors the filesystem store's directory
// enumeration.
func (s *Store) ListInteractions(ctx context.Context, runID string) ([]string, error) {
	cur, err := s.interactions.Find(
		ctx,
		bson.M{"run_id": runID},
		options.Find().
			SetProjection(bson.M{"_id": 1}).
			SetSort(bson.D{{Key: "requested_at", Value: 1}}),
	)
	if err != nil {
		return nil, fmt.Errorf("store/mongo: list interactions %s: %w", runID, err)
	}
	defer cur.Close(ctx)

	ids := []string{}
	for cur.Next(ctx) {
		var doc struct {
			ID interactionID `bson:"_id"`
		}
		if err := cur.Decode(&doc); err != nil {
			return nil, fmt.Errorf("store/mongo: decode interaction id: %w", err)
		}
		ids = append(ids, doc.ID.InteractionID)
	}
	return ids, cur.Err()
}

package mongo

import (
	"context"
	"errors"
	"fmt"
	"time"

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

// WriteInteraction upserts the interaction document. Two paths:
// the initial pause writes the questions, and the resume path writes
// the answers; both go through this single method.
func (s *Store) WriteInteraction(ctx context.Context, i *store.Interaction) error {
	format, err := s.valueWriteFormat(ctx, i.RunID)
	if err != nil {
		return err
	}
	copy := *i
	stampTenantOnInteraction(ctx, &copy)
	doc, err := valueRecordDocument(ctx, format, store.ExactInteraction, &copy)
	if err != nil {
		return err
	}
	doc["_id"] = interactionID{RunID: i.RunID, InteractionID: i.ID}
	return s.withValueRecordWrite(ctx, i.RunID, func(sc context.Context, _ store.ValueFormat, _ *store.Run) error {
		_, e := s.interactions.ReplaceOne(sc, withTenantFilter(sc, bson.M{"_id": doc["_id"]}), doc, options.Replace().SetUpsert(true))
		return e
	})
}

func (s *Store) LoadInteraction(ctx context.Context, runID, interactionID2 string) (*store.Interaction, error) {
	format, err := s.valueReadFormat(ctx, runID)
	if err != nil {
		return nil, err
	}
	res := s.interactions.FindOne(ctx, withTenantFilter(ctx, bson.M{"_id": interactionID2Key(runID, interactionID2)}))
	raw, err := res.Raw()
	if err != nil {
		return nil, fmt.Errorf("store/mongo: load interaction %s/%s: %w", runID, interactionID2, err)
	}
	var out store.Interaction
	if err = decodeValueBSON(ctx, format, store.ExactInteraction, raw, &out); err != nil {
		return nil, err
	}
	out.RunID = runID
	out.ID = interactionID2
	return &out, nil
}

func (s *Store) AnswerInteractionCAS(ctx context.Context, runID, interactionID string, answers map[string]any) (*store.Interaction, error) {
	format, err := s.valueWriteFormat(ctx, runID)
	if err != nil {
		return nil, err
	}
	encoded, err := valueRecordDocument(ctx, format, store.ExactInteraction, &store.Interaction{Answers: answers})
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	var result *store.Interaction
	err = s.withValueRecordWrite(ctx, runID, func(sc context.Context, _ store.ValueFormat, _ *store.Run) error {
		res := s.interactions.FindOneAndUpdate(sc, withTenantFilter(sc, bson.M{"_id": interactionID2Key(runID, interactionID), "answered_at": nil}), bson.M{"$set": bson.M{"answers": encoded["answers"], "answered_at": now}}, options.FindOneAndUpdate().SetReturnDocument(options.After))
		raw, e := res.Raw()
		if errors.Is(e, mongo.ErrNoDocuments) {
			if exists := s.interactions.FindOne(sc, withTenantFilter(sc, bson.M{"_id": interactionID2Key(runID, interactionID)})).Err(); exists != nil {
				return exists
			}
			return store.ErrInteractionAlreadyAnswered
		}
		if e != nil {
			return e
		}
		var out store.Interaction
		if e = decodeValueBSON(sc, format, store.ExactInteraction, raw, &out); e != nil {
			return e
		}
		result = &out
		return nil
	})
	return result, err
}

func interactionID2Key(runID, iid string) interactionID {
	return interactionID{RunID: runID, InteractionID: iid}
}

// ListInteractions returns the interaction ids for a run, in
// requested-at order. Mirrors the filesystem store's directory
// enumeration.
func (s *Store) ListInteractions(ctx context.Context, runID string) ([]string, error) {
	if _, err := s.valueReadFormat(ctx, runID); err != nil {
		return nil, err
	}
	cur, err := s.interactions.Find(
		ctx,
		withTenantFilter(ctx, bson.M{"run_id": runID}),
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

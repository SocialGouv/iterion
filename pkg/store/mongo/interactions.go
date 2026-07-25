package mongo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/internal/mongoutil"
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
	if err := s.guardNotDeleted(ctx, i.RunID); err != nil {
		return err
	}
	stampTenantOnInteraction(ctx, i)
	doc := interactionDoc{
		ID: interactionID{
			RunID:         i.RunID,
			InteractionID: i.ID,
		},
		Interaction: *i,
	}
	_, err := s.interactions.ReplaceOne(
		ctx,
		withTenantFilter(ctx, bson.M{"_id": doc.ID}),
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
	doc, err := mongoutil.FindOne[interactionDoc](ctx, s.interactions,
		withTenantFilter(ctx, bson.M{"_id": interactionID{RunID: runID, InteractionID: interactionID2}}),
		fmt.Errorf("store/mongo: interaction %s/%s not found", runID, interactionID2),
		fmt.Sprintf("store/mongo: load interaction %s/%s", runID, interactionID2))
	if err != nil {
		return nil, err
	}
	out := doc.Interaction
	out.RunID = runID
	out.ID = interactionID2
	return &out, nil
}

// AnswerInteractionCAS implements store.InteractionAnswerCAS with a
// single filtered update: the answered_at-is-unset condition rides the
// filter, so of two concurrent answerers exactly one document update
// applies and the loser maps to ErrInteractionAlreadyAnswered — no
// load-then-write window. ({"answered_at": nil} matches both a missing
// field — the omitempty write shape — and an explicit null.)
func (s *Store) AnswerInteractionCAS(ctx context.Context, runID, interactionID string, answers map[string]any) (*store.Interaction, error) {
	now := time.Now().UTC()
	res := s.interactions.FindOneAndUpdate(
		ctx,
		withTenantFilter(ctx, bson.M{
			"_id":         interactionID2Key(runID, interactionID),
			"answered_at": nil,
		}),
		bson.M{"$set": bson.M{"answers": answers, "answered_at": now}},
		options.FindOneAndUpdate().SetReturnDocument(options.After),
	)
	var doc interactionDoc
	err := res.Decode(&doc)
	if err == nil {
		out := doc.Interaction
		out.RunID = runID
		out.ID = interactionID
		return &out, nil
	}
	if !errors.Is(err, mongo.ErrNoDocuments) {
		return nil, fmt.Errorf("store/mongo: answer interaction %s/%s: %w", runID, interactionID, err)
	}
	// No unanswered document matched: distinguish already-answered from
	// genuinely missing.
	if _, lerr := s.LoadInteraction(ctx, runID, interactionID); lerr != nil {
		return nil, lerr
	}
	return nil, fmt.Errorf("interaction %s/%s: %w", runID, interactionID, store.ErrInteractionAlreadyAnswered)
}

func interactionID2Key(runID, iid string) interactionID {
	return interactionID{RunID: runID, InteractionID: iid}
}

// ListInteractions returns the interaction ids for a run, in
// requested-at order. Mirrors the filesystem store's directory
// enumeration.
func (s *Store) ListInteractions(ctx context.Context, runID string) ([]string, error) {
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

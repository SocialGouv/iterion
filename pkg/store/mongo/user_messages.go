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

// userMessageID is the composite _id for the user_messages collection.
// Mirrors the (run_id, interaction_id) shape used by interactions.
type userMessageID struct {
	RunID     string `bson:"run_id"`
	MessageID string `bson:"message_id"`
}

type userMessageDoc struct {
	ID                      userMessageID `bson:"_id"`
	store.QueuedUserMessage `bson:",inline"`
}

// AppendQueuedMessage upserts a new queued message in "queued" status.
// Unlike the filesystem store's append-JSONL log, Mongo carries a
// single row per message (the latest status overrides). The runtime
// emits a companion event to events.jsonl so observers can replay
// transitions.
func (s *Store) AppendQueuedMessage(ctx context.Context, runID string, msg store.QueuedUserMessage) error {
	if err := store.NormalizeQueuedForAppend(&msg, runID); err != nil {
		return fmt.Errorf("store/mongo: %w", err)
	}
	stampTenantOnQueuedMessage(ctx, &msg)
	doc := userMessageDoc{
		ID:                userMessageID{RunID: runID, MessageID: msg.ID},
		QueuedUserMessage: msg,
	}
	_, err := s.userMessages.ReplaceOne(
		ctx,
		withTenantFilter(ctx, bson.M{"_id": doc.ID}),
		doc,
		options.Replace().SetUpsert(true),
	)
	if err != nil {
		return fmt.Errorf("store/mongo: write user_message %s/%s: %w", runID, msg.ID, err)
	}
	return nil
}

// UpdateQueuedMessageStatus performs a compare-and-set on the message
// row when expectedFrom is non-empty; otherwise it unconditionally
// stamps the new status + the matching transition timestamp.
func (s *Store) UpdateQueuedMessageStatus(ctx context.Context, runID, msgID string, status store.QueuedMessageStatus, expectedFrom ...store.QueuedMessageStatus) error {
	if msgID == "" {
		return fmt.Errorf("store/mongo: queued message ID required")
	}
	filter := withTenantFilter(ctx, bson.M{"_id": userMessageID{RunID: runID, MessageID: msgID}})
	if len(expectedFrom) > 0 {
		want := make(bson.A, 0, len(expectedFrom))
		for _, s := range expectedFrom {
			want = append(want, string(s))
		}
		filter["status"] = bson.M{"$in": want}
	}
	now := time.Now().UTC()
	set := bson.M{"status": string(status)}
	switch status {
	case store.QueuedMessageStatusDelivered:
		set["delivered_at"] = now
	case store.QueuedMessageStatusConsumed:
		set["consumed_at"] = now
	case store.QueuedMessageStatusCancelled:
		set["cancelled_at"] = now
	}
	res, err := s.userMessages.UpdateOne(ctx, filter, bson.M{"$set": set})
	if err != nil {
		return fmt.Errorf("store/mongo: update user_message %s/%s: %w", runID, msgID, err)
	}
	if res.MatchedCount == 0 {
		exists, exErr := s.userMessageExists(ctx, runID, msgID)
		if exErr != nil {
			return exErr
		}
		if !exists {
			return store.ErrQueuedMessageNotFound
		}
		return fmt.Errorf("%w", store.ErrQueuedMessageStatusConflict)
	}
	return nil
}

func (s *Store) userMessageExists(ctx context.Context, runID, msgID string) (bool, error) {
	err := s.userMessages.FindOne(ctx, withTenantFilter(ctx, bson.M{"_id": userMessageID{RunID: runID, MessageID: msgID}})).Err()
	if err == nil {
		return true, nil
	}
	if errors.Is(err, mongo.ErrNoDocuments) {
		return false, nil
	}
	return false, fmt.Errorf("store/mongo: check user_message %s/%s: %w", runID, msgID, err)
}

// LoadPendingQueuedMessages returns rows with status="queued" in FIFO
// order by queued_at.
func (s *Store) LoadPendingQueuedMessages(ctx context.Context, runID string) ([]store.QueuedUserMessage, error) {
	return s.findUserMessages(ctx, withTenantFilter(ctx, bson.M{
		"run_id": runID,
		"status": string(store.QueuedMessageStatusQueued),
	}))
}

// ListQueuedMessages returns every row for the run in FIFO order.
func (s *Store) ListQueuedMessages(ctx context.Context, runID string) ([]store.QueuedUserMessage, error) {
	return s.findUserMessages(ctx, withTenantFilter(ctx, bson.M{"run_id": runID}))
}

func (s *Store) findUserMessages(ctx context.Context, filter bson.M) ([]store.QueuedUserMessage, error) {
	cur, err := s.userMessages.Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "queued_at", Value: 1}}))
	if err != nil {
		return nil, fmt.Errorf("store/mongo: find user_messages: %w", err)
	}
	defer cur.Close(ctx)
	out := []store.QueuedUserMessage{}
	for cur.Next(ctx) {
		var doc userMessageDoc
		if err := cur.Decode(&doc); err != nil {
			return nil, fmt.Errorf("store/mongo: decode user_message: %w", err)
		}
		out = append(out, doc.QueuedUserMessage)
	}
	return out, cur.Err()
}

// stampTenantOnQueuedMessage mirrors stampTenantOnInteraction.
func stampTenantOnQueuedMessage(ctx context.Context, msg *store.QueuedUserMessage) {
	if msg == nil || msg.TenantID != "" {
		return
	}
	if id, ok := store.TenantFromContext(ctx); ok {
		msg.TenantID = id
	}
}

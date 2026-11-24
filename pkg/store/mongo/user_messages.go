package mongo

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readconcern"

	"github.com/SocialGouv/iterion/pkg/store"
)

// userMessageID is the composite _id for the user_messages collection.
// Mirrors the (run_id, interaction_id) shape used by interactions.
type userMessageID struct {
	RunID     string `bson:"run_id"`
	MessageID string `bson:"message_id"`
	Namespace string `bson:"namespace,omitempty"`
}

type userMessageDoc struct {
	ID                      userMessageID `bson:"_id"`
	Namespace               string        `bson:"data_namespace,omitempty"`
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
	msg.SkillRefs = slices.Clone(msg.SkillRefs)
	stampTenantOnQueuedMessage(ctx, &msg)
	doc := userMessageDoc{
		ID:                userMessageID{RunID: runID, MessageID: msg.ID},
		QueuedUserMessage: msg,
	}
	return s.withRunMetadataWrite(ctx, runID, func(sc context.Context, parent *store.Run) error {
		ns, err := s.inboxNamespace(sc, runID, parent)
		if err != nil {
			return err
		}
		doc.ID, doc.Namespace = ns.messageID(msg.ID), ns.namespace
		_, err = ns.collection.ReplaceOne(sc, ns.filter(sc, bson.M{"_id": doc.ID}), doc, options.Replace().SetUpsert(true))
		if err != nil {
			return fmt.Errorf("store/mongo: write user_message %s/%s: %w", runID, msg.ID, err)
		}
		return nil
	})
}

// AppendQueuedMessageOnce inserts a stable message ID exactly once. Unlike
// ReplaceOne, a replay cannot move an already-delivered/consumed row back to
// queued; $setOnInsert makes the existing row immutable on the duplicate.
func (s *Store) AppendQueuedMessageOnce(ctx context.Context, runID string, msg store.QueuedUserMessage) (bool, error) {
	if err := store.NormalizeQueuedForAppend(&msg, runID); err != nil {
		return false, fmt.Errorf("store/mongo: %w", err)
	}
	msg.SkillRefs = slices.Clone(msg.SkillRefs)
	stampTenantOnQueuedMessage(ctx, &msg)
	doc := userMessageDoc{
		ID:                userMessageID{RunID: runID, MessageID: msg.ID},
		QueuedUserMessage: msg,
	}
	inserted := false
	err := s.withRunMetadataWrite(ctx, runID, func(sc context.Context, parent *store.Run) error {
		inserted = false
		ns, err := s.inboxNamespace(sc, runID, parent)
		if err != nil {
			return err
		}
		doc.ID, doc.Namespace = ns.messageID(msg.ID), ns.namespace
		res, err := ns.collection.UpdateOne(sc, ns.filter(sc, bson.M{"_id": doc.ID}), bson.M{"$setOnInsert": doc}, options.UpdateOne().SetUpsert(true))
		if err != nil {
			return fmt.Errorf("store/mongo: insert-once user_message %s/%s: %w", runID, msg.ID, err)
		}
		if res.UpsertedCount == 0 {
			return errValuePublicationNoop
		}
		inserted = true
		return nil
	})
	return inserted && err == nil, err
}

// UpdateQueuedMessageStatus performs a compare-and-set on the message
// row when expectedFrom is non-empty; otherwise it unconditionally
// stamps the new status + the matching transition timestamp.
func (s *Store) UpdateQueuedMessageStatus(ctx context.Context, runID, msgID string, status store.QueuedMessageStatus, expectedFrom ...store.QueuedMessageStatus) error {
	if msgID == "" {
		return fmt.Errorf("store/mongo: queued message ID required")
	}
	filter := bson.M{}
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
	return s.withRunMetadataWrite(ctx, runID, func(sc context.Context, parent *store.Run) error {
		ns, err := s.inboxNamespace(sc, runID, parent)
		if err != nil {
			return err
		}
		selected := ns.filter(sc, filter)
		selected["_id"] = ns.messageID(msgID)
		res, err := ns.collection.UpdateOne(sc, selected, bson.M{"$set": set})
		if err != nil {
			return fmt.Errorf("store/mongo: update user_message %s/%s: %w", runID, msgID, err)
		}
		if res.MatchedCount == 0 {
			exists, exErr := s.userMessageExists(sc, ns, msgID)
			if exErr != nil {
				return exErr
			}
			if !exists {
				return store.ErrQueuedMessageNotFound
			}
			return fmt.Errorf("%w", store.ErrQueuedMessageStatusConflict)
		}
		return nil
	})
}

func (s *Store) userMessageExists(ctx context.Context, ns inboxDataNamespace, msgID string) (bool, error) {
	runID := ns.runID
	err := ns.collection.FindOne(ctx, ns.filter(ctx, bson.M{"_id": ns.messageID(msgID)})).Err()
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
	return s.findObservedUserMessages(ctx, runID, withTenantFilter(ctx, bson.M{
		"run_id": runID,
		"status": string(store.QueuedMessageStatusQueued),
	}))
}

// ListQueuedMessages returns every row for the run in FIFO order.
func (s *Store) ListQueuedMessages(ctx context.Context, runID string) ([]store.QueuedUserMessage, error) {
	return s.findObservedUserMessages(ctx, runID, withTenantFilter(ctx, bson.M{"run_id": runID}))
}

func (s *Store) findUserMessages(ctx context.Context, filter bson.M) ([]store.QueuedUserMessage, error) {
	return s.findUserMessagesIn(ctx, s.userMessages, filter)
}

func (s *Store) findUserMessagesIn(ctx context.Context, collection *mongo.Collection, filter bson.M) ([]store.QueuedUserMessage, error) {
	cur, err := collection.Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "queued_at", Value: 1}}))
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

// findObservedUserMessages reads the original header and its rows from one
// database snapshot. A preflight outside that snapshot can adopt a replacement
// created between the header check and the payload query.
func (s *Store) findObservedUserMessages(ctx context.Context, runID string, filter bson.M) ([]store.QueuedUserMessage, error) {
	if ctx == nil {
		return nil, store.ErrInvalidValueFormat
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	session, err := s.client.StartSession()
	if err != nil {
		return nil, err
	}
	defer session.EndSession(ctx)
	var result []store.QueuedUserMessage
	_, err = session.WithTransaction(ctx, func(sc context.Context) (any, error) {
		result = nil
		parent, err := s.valueParent(sc, runID)
		if err != nil {
			return nil, err
		}
		ns, err := s.inboxNamespace(sc, runID, parent)
		if err != nil {
			return nil, err
		}
		rows, err := s.findUserMessagesIn(sc, ns.collection, ns.filter(sc, filter))
		if err != nil {
			return nil, err
		}
		result = rows
		return nil, nil
	}, options.Transaction().SetReadConcern(readconcern.Snapshot()))
	if err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

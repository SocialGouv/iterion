package boardmongo

import (
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
)

// This file holds the trigger-spine primitives of the Mongo board: the
// atomic one-shot label consume (the multi-replica counterpart of
// trigger.NativeBoardEffect.ConsumeMatchLabels) and the per-tenant event
// cursor the cloud board source poll-tails events with.

// ConsumeLabels atomically strips the given labels from an issue, reporting
// whether they were present. ONE UpdateOne with a labels-present filter —
// two replicas' evaluators racing on the same event cannot both observe
// consumed=true (same CAS shape as Claim). Matching is exact-string: the
// trigger labels are machine-stamped constants (native.LabelTriageAuto),
// never operator free-text.
func (s *Store) ConsumeLabels(id string, labels []string) (bool, error) {
	if id == "" || len(labels) == 0 {
		return false, nil
	}
	ctx, cancel := ctxWithTimeout()
	defer cancel()
	res, err := s.issues.UpdateOne(ctx,
		bson.M{"_id": id, "tenant_id": s.tenant, "issue.labels": bson.M{"$all": labels}},
		bson.M{
			"$pull": bson.M{"issue.labels": bson.M{"$in": labels}},
			"$set":  bson.M{"issue.updatedat": time.Now().UTC()},
		},
	)
	if err != nil {
		return false, fmt.Errorf("boardmongo: consume labels: %w", err)
	}
	if res.ModifiedCount == 0 {
		return false, nil // already consumed (or issue/labels absent)
	}
	// Parity with the local effect (whose store.Update emits): the label
	// strip is a real card mutation and must show in the event log. The
	// trigger label is gone from the card, so this update event cannot
	// re-fire the consuming subscription.
	return true, s.emit(native.Event{Type: native.EvtIssueUpdated, IssueID: id, Payload: map[string]any{"consumed_labels": labels}})
}

// EventsAfter returns up to limit events with seq strictly greater than
// afterSeq, oldest first — the poll-tail read the cloud board source uses.
func (s *Store) EventsAfter(afterSeq int64, limit int) ([]native.Event, error) {
	ctx, cancel := ctxWithTimeout()
	defer cancel()
	opt := options.Find().SetSort(bson.D{{Key: "event.seq", Value: 1}})
	if limit > 0 {
		opt.SetLimit(int64(limit))
	}
	cur, err := s.events.Find(ctx, bson.M{"tenant_id": s.tenant, "event.seq": bson.M{"$gt": afterSeq}}, opt)
	if err != nil {
		return nil, fmt.Errorf("boardmongo: events after %d: %w", afterSeq, err)
	}
	defer cur.Close(ctx)
	var out []native.Event
	for cur.Next(ctx) {
		var doc eventDoc
		if err := cur.Decode(&doc); err != nil {
			continue
		}
		out = append(out, doc.Event)
	}
	return out, cur.Err()
}

// triggerCursorID is the config-collection doc holding the trigger tail's
// high-water mark for one tenant.
func (s *Store) triggerCursorID() string { return "trigger-cursor:" + s.tenant }

// TriggerCursor returns the trigger tail's current high-water seq. A missing
// cursor initialises AT THE CURRENT TIP (the event-seq counter's value), so
// a fresh deployment never replays board history as a trigger storm.
func (s *Store) TriggerCursor() (int64, error) {
	ctx, cancel := ctxWithTimeout()
	defer cancel()
	var doc struct {
		Seq int64 `bson:"seq"`
	}
	err := s.config.FindOne(ctx, bson.M{"_id": s.triggerCursorID()}).Decode(&doc)
	if err == nil {
		return doc.Seq, nil
	}
	if err != mongo.ErrNoDocuments {
		return 0, fmt.Errorf("boardmongo: trigger cursor: %w", err)
	}
	// First read: seed at the tip. Best-effort insert — a racing replica's
	// seed wins harmlessly (both read the same tip modulo a few events that
	// the next tick picks up).
	tip := int64(0)
	if err := s.config.FindOne(ctx, bson.M{"_id": "seq:" + s.tenant}).Decode(&doc); err == nil {
		tip = doc.Seq
	}
	_, _ = s.config.UpdateOne(ctx,
		bson.M{"_id": s.triggerCursorID()},
		bson.M{"$setOnInsert": bson.M{"seq": tip}},
		options.UpdateOne().SetUpsert(true),
	)
	return tip, nil
}

// AdvanceTriggerCursor CAS-advances the cursor from `from` to `to`,
// reporting whether THIS caller won. In a multi-replica deployment every
// replica tails the same tenant; the winner of the advance publishes that
// batch of events, the losers drop it — so each board event enters the bus
// exactly once.
func (s *Store) AdvanceTriggerCursor(from, to int64) (bool, error) {
	if to <= from {
		return false, nil
	}
	ctx, cancel := ctxWithTimeout()
	defer cancel()
	res, err := s.config.UpdateOne(ctx,
		bson.M{"_id": s.triggerCursorID(), "seq": from},
		bson.M{"$set": bson.M{"seq": to}},
	)
	if err != nil {
		return false, fmt.Errorf("boardmongo: advance trigger cursor: %w", err)
	}
	return res.ModifiedCount == 1, nil
}

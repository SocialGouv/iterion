package boardmongo

import (
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	"github.com/SocialGouv/iterion/pkg/trigger"
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

// --- trigger-effect outbox (ADR-094) ---
//
// One durable row per matched (board event, subscription) pair, written by
// the cloud board source BEFORE the cursor advances and drained by the
// trigger.EffectWorker under an atomic leased claim. Implements
// trigger.EffectOutbox.

// UpsertPending inserts rows that do not exist yet; an existing row (a racing
// replica's materialization, a re-scan after an aborted batch) is untouched
// whatever state it reached — $setOnInsert is the idempotency.
func (s *Store) UpsertPending(_ context.Context, rows []trigger.EffectRow) error {
	if len(rows) == 0 {
		return nil
	}
	ctx, cancel := ctxWithTimeout()
	defer cancel()
	models := make([]mongo.WriteModel, 0, len(rows))
	for _, r := range rows {
		r.TenantID = s.tenant
		if r.State == "" {
			r.State = trigger.EffectPending
		}
		models = append(models, mongo.NewUpdateOneModel().
			SetFilter(bson.M{"_id": r.ID}).
			SetUpdate(bson.M{"$setOnInsert": r}).
			SetUpsert(true))
	}
	if _, err := s.effects.BulkWrite(ctx, models, options.BulkWrite().SetOrdered(false)); err != nil {
		return fmt.Errorf("boardmongo: upsert trigger effects: %w", err)
	}
	return nil
}

// ClaimDue flips up to limit eligible rows to claimed with a fresh lease.
// One FindOneAndUpdate per row keeps the flip atomic per document — two
// replicas' workers cannot claim the same row.
func (s *Store) ClaimDue(_ context.Context, now time.Time, limit int) ([]trigger.EffectRow, error) {
	ctx, cancel := ctxWithTimeout()
	defer cancel()
	if limit <= 0 {
		limit = 20
	}
	var out []trigger.EffectRow
	for len(out) < limit {
		var row trigger.EffectRow
		err := s.effects.FindOneAndUpdate(ctx,
			bson.M{
				"tenant_id":  s.tenant,
				"state":      bson.M{"$in": bson.A{trigger.EffectPending, trigger.EffectClaimed}},
				"not_before": bson.M{"$lte": now},
			},
			bson.M{"$set": bson.M{
				"state":      trigger.EffectClaimed,
				"not_before": now.Add(trigger.EffectLease),
				"updated_at": now,
			}},
			options.FindOneAndUpdate().
				SetSort(bson.D{{Key: "created_at", Value: 1}}).
				SetReturnDocument(options.After),
		).Decode(&row)
		if err == mongo.ErrNoDocuments {
			break
		}
		if err != nil {
			return out, fmt.Errorf("boardmongo: claim trigger effect: %w", err)
		}
		out = append(out, row)
	}
	return out, nil
}

func (s *Store) setEffect(id string, fields bson.M) error {
	ctx, cancel := ctxWithTimeout()
	defer cancel()
	fields["updated_at"] = time.Now().UTC()
	if _, err := s.effects.UpdateOne(ctx, bson.M{"_id": id, "tenant_id": s.tenant}, bson.M{"$set": fields}); err != nil {
		return fmt.Errorf("boardmongo: update trigger effect %s: %w", id, err)
	}
	return nil
}

func (s *Store) MarkConsumed(_ context.Context, id string) error {
	return s.setEffect(id, bson.M{"consume_marked": true})
}

func (s *Store) MarkDone(_ context.Context, id string) error {
	return s.setEffect(id, bson.M{"state": trigger.EffectDone})
}

func (s *Store) MarkRetry(_ context.Context, id string, attempts int, notBefore time.Time, lastErr string) error {
	return s.setEffect(id, bson.M{
		"state": trigger.EffectPending, "attempts": attempts,
		"not_before": notBefore, "last_error": lastErr,
	})
}

func (s *Store) MarkFailed(_ context.Context, id string, lastErr string) error {
	return s.setEffect(id, bson.M{"state": trigger.EffectFailed, "last_error": lastErr})
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

package mongo

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/store"
)

// AppendEvent publishes the parent fence, sequence and event in one Mongo
// transaction. The driver retries transient transaction conflicts; the event
// tail seeds a missing or old counter inside that same transaction. User
// encoding and runtime callbacks complete before any retryable operation.
//
// Plan §D.3.
func (s *Store) AppendEvent(ctx context.Context, runID string, evt store.Event) (*store.Event, error) {
	format, original, err := s.prepareEventObservation(ctx, runID)
	if err != nil {
		return nil, err
	}
	if evt.Timestamp.IsZero() {
		evt.Timestamp = time.Now().UTC()
	}
	evt.RunID = runID
	stampTenantOnEvent(ctx, &evt)
	// Stamp the run's current log byte position when the runner wired a
	// callback (SetLogPositionFn) — the cloud twin of the filesystem
	// store's stamping, powering the studio's per-node log slicing.
	// Only when the caller didn't pre-fill LogOffset.
	projected, err := store.ApplyEventProjection(ctx, original, &evt)
	if err != nil {
		return nil, err
	}
	if !projected && evt.LogOffset == 0 {
		evt.LogOffset = s.logPositionFor(runID)
	}
	// Stamp the run's monotonic active duration (engine SharedBudget
	// elapsed) — the cloud twin of the filesystem store's ActiveMs
	// stamping, powering the studio's suspend-excluded active timer.
	if !projected && evt.ActiveMs == 0 {
		evt.ActiveMs = s.activeDurationFor(runID)
	}

	doc, err := valueRecordDocument(ctx, format, store.ExactEvent, &evt)
	if err != nil {
		return nil, err
	}
	err = s.withValueRecordWrite(ctx, runID, func(sc context.Context, selectedFormat store.ValueFormat, parent *store.Run) error {
		selected, e := store.ObserveEventParent(sc, runID, selectedFormat, parent)
		if e != nil {
			return e
		}
		if selected != original {
			return store.ErrRunDataIdentityChanged
		}
		seq, e := s.publishValueEvent(sc, runID, doc)
		if e == nil {
			evt.Seq = seq
			evt, e = store.WithEventObservation(evt, selected)
		}
		return e
	})
	if err != nil {
		return nil, err
	}
	return &evt, nil
}

// planSeqField advances plan snapshots independently of the events next_seq field.
const planSeqField = "next_plan_seq"

// Legacy counters used a bson.M embedded _id: both field orders exist as
// distinct Mongo keys. New writers use one ordered key. Before incrementing,
// the selected field converges monotonically to the maximum of both historic
// keys; rows are retained, and the other sequence field is independent.
// Older binaries must be drained before these writers are activated.
func (s *Store) seedStreamSeqField(ctx context.Context, n streamDataNamespace, field string, floor int64) error {
	runID := n.runID
	tenantID, _ := store.TenantFromContext(ctx)
	canonical := bson.D{{Key: "tenant_id", Value: tenantID}, {Key: "run_id", Value: runID}}
	reversed := bson.D{{Key: "run_id", Value: runID}, {Key: "tenant_id", Value: tenantID}}
	// Match both exact historic keys through the _id index, not a collection scan.
	if n.namespace == "" {
		legacy := s.runSeq.FindOne(ctx, bson.M{"_id": bson.M{"$in": bson.A{canonical, reversed}}}, options.FindOne().SetSort(bson.D{{Key: field, Value: -1}}).SetProjection(bson.M{field: 1}))
		raw, readErr := legacy.Raw()
		if readErr != nil && !errors.Is(readErr, mongo.ErrNoDocuments) {
			return readErr
		}
		if readErr == nil {
			if n, ok := raw.Lookup(field).AsInt64OK(); ok && n > floor {
				floor = n
			}
		}
	}
	if floor == math.MaxInt64 {
		return fmt.Errorf("store/mongo: sequence %s exhausted", field)
	}

	_, err := n.seq.UpdateOne(
		ctx,
		bson.M{"_id": n.counterKey(ctx)},
		bson.M{"$max": bson.M{field: floor}},
		options.UpdateOne().SetUpsert(true),
	)
	if err != nil {
		return fmt.Errorf("store/mongo: seed seq %s for run %s: %w", field, runID, err)
	}
	return nil
}

func (s *Store) allocStreamSeqField(ctx context.Context, n streamDataNamespace, field string) (int64, error) {
	runID := n.runID
	if err := s.seedStreamSeqField(ctx, n, field, 0); err != nil {
		return 0, err
	}

	res := n.seq.FindOneAndUpdate(
		ctx,
		bson.M{"_id": n.counterKey(ctx), field: bson.M{"$lt": int64(math.MaxInt64)}},
		bson.M{"$inc": bson.M{field: 1}},
		options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.Before),
	)
	if err := res.Err(); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			// Pre-image absent: the upsert just created the document
			// with field=1 (the $inc applied to the implicit zero).
			// Our seq is therefore 0 (the value before the increment).
			return 0, nil
		}
		return 0, fmt.Errorf("store/mongo: alloc seq %s: %w", runID, err)
	}
	raw, err := res.Raw()
	if err != nil {
		return 0, fmt.Errorf("store/mongo: decode seq %s: %w", runID, err)
	}
	// Field may be absent when the counter document was created by a
	// sibling sequence (e.g. events' next_seq exists but this is the
	// first plan snapshot for the run): absent → 0.
	if v, ok := raw.Lookup(field).AsInt64OK(); ok {
		return v, nil
	}
	return 0, nil
}

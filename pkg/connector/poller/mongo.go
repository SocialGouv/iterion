package poller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/internal/mongoutil"
)

// Collection names.
const (
	colPollers = "connector_pollers"
	colSeen    = "connector_poller_seen"
)

// seenBatch bounds the keys one round trip carries: a feed of ten thousand
// items is twenty requests, not one document past Mongo's bound.
const seenBatch = 500

// MongoStore is the cloud twin. Every fenced write is one conditional update
// on the poller's document, matching its id and generation together — and,
// for a timestamp checkpoint, the stored instant — so a write under a lost
// generation or a checkpoint behind changes nothing.
type MongoStore struct {
	pollers *mongo.Collection
	seen    *mongo.Collection
}

// NewMongoStore returns the store over db; EnsureSchema creates its indexes.
func NewMongoStore(db *mongo.Database) *MongoStore {
	return &MongoStore{pollers: db.Collection(colPollers), seen: db.Collection(colSeen)}
}

// EnsureSchema creates the indexes idempotently: the due query reads
// (next_tick_at, _id) across tenants, a tenant's list reads its own, and a
// prune reads a poller's keys by their last observation.
func EnsureSchema(ctx context.Context, db *mongo.Database) error {
	if _, err := db.Collection(colPollers).Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "next_tick_at", Value: 1}, {Key: "_id", Value: 1}}, Options: options.Index().SetName("poller_due")},
		{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "_id", Value: 1}}, Options: options.Index().SetName("poller_tenant")},
	}); err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("poller: ensure poller indexes: %w", err)
	}
	if _, err := db.Collection(colSeen).Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "poller_id", Value: 1}, {Key: "last_observed_at", Value: 1}}, Options: options.Index().SetName("seen_poller_observed")},
		// An org purge deletes a team's rows by tenant.
		{Keys: bson.D{{Key: "tenant_id", Value: 1}}, Options: options.Index().SetName("seen_tenant")},
	}); err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("poller: ensure seen indexes: %w", err)
	}
	return nil
}

func (s *MongoStore) Ensure(ctx context.Context, p Poller) (Poller, bool, error) {
	p, err := preparePoller(p)
	if err != nil {
		return Poller{}, false, err
	}
	res, err := s.pollers.UpdateOne(ctx, bson.M{"_id": p.ID}, bson.M{"$setOnInsert": p}, options.UpdateOne().SetUpsert(true))
	if err != nil && !mongo.IsDuplicateKeyError(err) {
		return Poller{}, false, fmt.Errorf("poller: ensure %s: %w", p.ID, err)
	}
	// A duplicate key is a concurrent Ensure of the same poller: the record
	// exists, which is the answer.
	created := err == nil && res.UpsertedCount == 1
	var stored Poller
	if err := s.pollers.FindOne(ctx, bson.M{"_id": p.ID}).Decode(&stored); err != nil {
		return Poller{}, created, fmt.Errorf("poller: read ensured %s: %w", p.ID, err)
	}
	return stored, created, nil
}

func (s *MongoStore) Get(ctx context.Context, tenantID, id string) (Poller, error) {
	var p Poller
	err := s.pollers.FindOne(ctx, bson.M{"_id": id, "tenant_id": tenantID}).Decode(&p)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return Poller{}, fmt.Errorf("%w: poller %s", ErrNotFound, id)
	}
	if err != nil {
		return Poller{}, fmt.Errorf("poller: get %s: %w", id, err)
	}
	return p, nil
}

func (s *MongoStore) List(ctx context.Context, tenantID string) ([]Poller, error) {
	return s.find(ctx, bson.M{"tenant_id": tenantID}, options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}))
}

func (s *MongoStore) ListDue(ctx context.Context, now time.Time, limit int) ([]Poller, error) {
	opts := options.Find().SetSort(bson.D{{Key: "next_tick_at", Value: 1}, {Key: "_id", Value: 1}})
	if limit > 0 {
		opts.SetLimit(int64(limit))
	}
	return s.find(ctx, bson.M{"next_tick_at": bson.M{"$lte": stamp(now)}}, opts)
}

func (s *MongoStore) find(ctx context.Context, filter bson.M, opts *options.FindOptionsBuilder) ([]Poller, error) {
	cur, err := s.pollers.Find(ctx, filter, opts)
	if err != nil {
		return nil, fmt.Errorf("poller: list: %w", err)
	}
	var out []Poller
	if err := cur.All(ctx, &out); err != nil {
		return nil, fmt.Errorf("poller: decode pollers: %w", err)
	}
	return out, nil
}

// refusal says why a fenced write matched nothing: the poller is gone, or
// its generation moved on.
func (s *MongoStore) refusal(ctx context.Context, id string, generation int64) error {
	var p Poller
	err := s.pollers.FindOne(ctx, bson.M{"_id": id}).Decode(&p)
	switch {
	case errors.Is(err, mongo.ErrNoDocuments):
		return fmt.Errorf("%w: poller %s", ErrNotFound, id)
	case err != nil:
		return fmt.Errorf("poller: read %s after a refused write: %w", id, err)
	case p.Generation != generation:
		return fmt.Errorf("%w: poller %s is at generation %d, the write names %d", ErrFenced, id, p.Generation, generation)
	}
	return nil
}

func (s *MongoStore) Acquire(ctx context.Context, id string, generation int64, owner string, now time.Time) (Poller, error) {
	if err := checkOwner(owner); err != nil {
		return Poller{}, err
	}
	var p Poller
	err := s.pollers.FindOneAndUpdate(ctx,
		bson.M{"_id": id, "generation": generation},
		bson.M{"$inc": bson.M{"generation": 1}, "$set": bson.M{"holder": owner, "updated_at": stamp(now)}},
		options.FindOneAndUpdate().SetReturnDocument(options.After)).Decode(&p)
	if errors.Is(err, mongo.ErrNoDocuments) {
		if rerr := s.refusal(ctx, id, generation); rerr != nil {
			return Poller{}, rerr
		}
		return Poller{}, fmt.Errorf("%w: poller %s moved while it was taken", ErrFenced, id)
	}
	if err != nil {
		return Poller{}, fmt.Errorf("poller: acquire %s: %w", id, err)
	}
	return p, nil
}

func (s *MongoStore) Wake(ctx context.Context, tenantID, id string, at time.Time) error {
	at = stamp(at)
	res, err := s.pollers.UpdateOne(ctx, bson.M{"_id": id, "tenant_id": tenantID}, bson.M{"$min": bson.M{"next_tick_at": at}})
	if err != nil {
		return fmt.Errorf("poller: wake %s: %w", id, err)
	}
	if res.MatchedCount == 0 {
		return fmt.Errorf("%w: poller %s", ErrNotFound, id)
	}
	return nil
}

func (s *MongoStore) Reserve(ctx context.Context, id string, generation int64, n int) (int64, error) {
	if n <= 0 {
		return 0, fmt.Errorf("%w: reserve %d sequence numbers", ErrInvalid, n)
	}
	var p Poller
	err := s.pollers.FindOneAndUpdate(ctx,
		bson.M{"_id": id, "generation": generation},
		bson.M{"$inc": bson.M{"sequence": int64(n)}},
		options.FindOneAndUpdate().SetReturnDocument(options.After)).Decode(&p)
	if errors.Is(err, mongo.ErrNoDocuments) {
		if rerr := s.refusal(ctx, id, generation); rerr != nil {
			return 0, rerr
		}
		return 0, fmt.Errorf("%w: poller %s moved while it reserved", ErrFenced, id)
	}
	if err != nil {
		return 0, fmt.Errorf("poller: reserve %s: %w", id, err)
	}
	return p.Sequence - int64(n) + 1, nil
}

func (s *MongoStore) Complete(ctx context.Context, id string, generation int64, t Tick) error {
	t, err := checkTick(t)
	if err != nil {
		return err
	}
	filter := bson.M{"_id": id, "generation": generation}
	if c := t.Checkpoint; c != nil && c.Kind == KindTimestamp {
		// The same rule as behind(): only a stored timestamp is ordered
		// against the new one.
		filter["$or"] = bson.A{
			bson.M{"checkpoint": nil},
			bson.M{"checkpoint.kind": bson.M{"$ne": KindTimestamp}},
			bson.M{"checkpoint.at": nil},
			bson.M{"checkpoint.at": bson.M{"$lte": *c.At}},
		}
	}
	// A pipeline update reads a string starting with "$" as a field path:
	// every value a vendor or an operator wrote goes in as a literal.
	set := bson.M{"last_tick_at": t.At, "next_tick_at": t.NextTickAt, "last_error": bson.M{"$literal": t.Error}, "updated_at": t.At}
	if t.Checkpoint != nil {
		set["checkpoint"] = bson.M{"$literal": t.Checkpoint}
	}
	switch {
	case t.Idle:
		set["primed"] = false
	case t.Primed:
		// Never back: a later priming reaches a later sequence.
		set["primed_through"] = bson.M{"$max": bson.A{bson.M{"$ifNull": bson.A{"$primed_through", 0}}, t.PrimedThrough}}
		set["primed"] = true
	}
	res, err := s.pollers.UpdateOne(ctx, filter, mongo.Pipeline{{{Key: "$set", Value: set}}})
	if err != nil {
		return fmt.Errorf("poller: complete %s: %w", id, err)
	}
	if res.MatchedCount == 1 {
		return nil
	}
	if rerr := s.refusal(ctx, id, generation); rerr != nil {
		return rerr
	}
	return fmt.Errorf("%w: poller %s holds a later timestamp than the tick reached (%s)", ErrCheckpointBehind, id, t.Checkpoint.At.Format(time.RFC3339Nano))
}

func (s *MongoStore) Seen(ctx context.Context, pollerID string, keys []ItemKey) (map[ItemKey]bool, error) {
	out := map[ItemKey]bool{}
	for start := 0; start < len(keys); start += seenBatch {
		batch := keys[start:min(start+seenBatch, len(keys))]
		ids := make(bson.A, 0, len(batch))
		for _, k := range batch {
			ids = append(ids, seenID(pollerID, k))
		}
		cur, err := s.seen.Find(ctx, bson.M{"_id": bson.M{"$in": ids}})
		if err != nil {
			return nil, fmt.Errorf("poller: read the seen set of %s: %w", pollerID, err)
		}
		var rows []seenRow
		if err := cur.All(ctx, &rows); err != nil {
			return nil, fmt.Errorf("poller: decode the seen set of %s: %w", pollerID, err)
		}
		for _, r := range rows {
			if r.PollerID == pollerID {
				out[r.Key] = true
			}
		}
	}
	return out, nil
}

func (s *MongoStore) MarkSeen(ctx context.Context, tenantID, pollerID string, keys []ItemKey, at time.Time) error {
	if tenantID == "" || pollerID == "" || at.IsZero() {
		return fmt.Errorf("%w: mark seen without a tenant, a poller or an instant", ErrInvalid)
	}
	at = stamp(at)
	for start := 0; start < len(keys); start += seenBatch {
		batch := keys[start:min(start+seenBatch, len(keys))]
		models := make([]mongo.WriteModel, 0, len(batch))
		for _, k := range batch {
			models = append(models, mongo.NewUpdateOneModel().
				SetFilter(bson.M{"_id": seenID(pollerID, k)}).
				SetUpdate(bson.M{
					"$setOnInsert": bson.M{"tenant_id": tenantID, "poller_id": pollerID, "item_id": k.ID, "item_version": k.Version, "first_seen_at": at},
					"$max":         bson.M{"last_observed_at": at},
				}).
				SetUpsert(true))
		}
		if _, err := s.seen.BulkWrite(ctx, models, options.BulkWrite().SetOrdered(false)); err != nil && !mongo.IsDuplicateKeyError(err) {
			return fmt.Errorf("poller: mark %d keys seen for %s: %w", len(batch), pollerID, err)
		}
	}
	return nil
}

func (s *MongoStore) PruneSeen(ctx context.Context, pollerID string, before time.Time) (int, error) {
	res, err := s.seen.DeleteMany(ctx, bson.M{"poller_id": pollerID, "last_observed_at": bson.M{"$lt": stamp(before)}})
	if err != nil {
		return 0, fmt.Errorf("poller: prune the seen set of %s: %w", pollerID, err)
	}
	return int(res.DeletedCount), nil
}

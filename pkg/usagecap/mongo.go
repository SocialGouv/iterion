package usagecap

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/internal/mongoutil"
)

const colUsageWindows = "usage_windows"

// usageRetentionDays evicts readings long after they could still matter. A
// weekly window is seven days; a fortnight of retention keeps the collection
// from growing without bound while never dropping a live reading.
const usageRetentionDays = 14

// MongoStore is the cloud Store: one document per (credential key, window),
// readable by every replica.
//
// The cloud twin ships with the feature rather than after it — a guard that
// only shares its readings inside one process is exactly the hole a runner
// fleet falls through, since no two runs land on the same pod.
type MongoStore struct{ col *mongo.Collection }

// NewMongoStore binds the store to a database.
func NewMongoStore(db *mongo.Database) *MongoStore {
	return &MongoStore{col: db.Collection(colUsageWindows)}
}

// EnsureSchema creates the indexes idempotently.
func EnsureSchema(ctx context.Context, db *mongo.Database) error {
	col := db.Collection(colUsageWindows)
	if _, err := col.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "key", Value: 1}},
			Options: options.Index().SetName("usage_windows_key")},
		{Keys: bson.D{{Key: "observed_at", Value: 1}},
			Options: options.Index().SetName("usage_windows_ttl").
				SetExpireAfterSeconds(int32(usageRetentionDays * 24 * 60 * 60))},
	}); err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("usagecap: ensure indexes: %w", err)
	}
	return nil
}

type readingDoc struct {
	Key         string    `bson:"key"`
	Window      string    `bson:"window"`
	Utilization float64   `bson:"utilization"`
	Status      string    `bson:"status"`
	ResetsAt    time.Time `bson:"resets_at,omitempty"`
	ObservedAt  time.Time `bson:"observed_at"`
	Refusals    int       `bson:"refusals,omitempty"`
}

func docID(key string, w Window) string { return key + "#" + string(w) }

// Record upserts the reading, refusing to regress a window to an older
// observation. The freshness condition lives in the FILTER, not in a
// read-then-write: several pods observe the same credential concurrently and
// the loser of that race must be dropped, not applied.
func (s *MongoStore) Record(ctx context.Context, key string, r Reading) error {
	if r.ObservedAt.IsZero() {
		r.ObservedAt = time.Now().UTC()
	}
	filter := bson.M{
		"_id": docID(key, r.Window),
		// An absent observed_at is the first write for this window: Mongo
		// comparison operators are type-bracketed and would not match a
		// missing field, so the "never seen" case needs its own branch or
		// the very first reading never lands.
		"$or": []bson.M{
			{"observed_at": bson.M{"$exists": false}},
			{"observed_at": bson.M{"$lte": r.ObservedAt.UTC()}},
		},
	}
	// An aggregation-pipeline update, not a plain $set: the refusal streak
	// is a function of the document being replaced, and several pods write
	// the same one. Computing it server-side keeps it a single atomic
	// operation instead of a read-then-write two of them could interleave.
	// Every literal is wrapped in $literal — inside a pipeline a bare
	// string is a field path, and the key carries operator-supplied text.
	update := mongo.Pipeline{{{Key: "$set", Value: bson.M{
		"key":         bson.M{"$literal": key},
		"window":      bson.M{"$literal": string(r.Window)},
		"utilization": bson.M{"$literal": r.Utilization},
		"status":      bson.M{"$literal": r.Status},
		"resets_at":   bson.M{"$literal": r.ResetsAt.UTC()},
		"observed_at": bson.M{"$literal": r.ObservedAt.UTC()},
		"refusals":    refusalStreakExpr(r),
	}}}}
	_, err := s.col.UpdateOne(ctx, filter, update, options.UpdateOne().SetUpsert(true))
	if err != nil {
		// A concurrent upsert that lost the race raises a duplicate key on
		// _id — the newer reading is already stored, which is the outcome
		// this call wanted.
		if mongo.IsDuplicateKeyError(err) {
			return nil
		}
		return fmt.Errorf("usagecap: record %s: %w", docID(key, r.Window), err)
	}
	return nil
}

// refusalStreakExpr is nextRefusalCount as an aggregation expression: the
// incoming reading decides whether a streak can exist at all (a refusal
// with no reset instant), and the PRE-UPDATE document decides whether this
// one continues an existing streak or starts it at 1.
func refusalStreakExpr(r Reading) any {
	if r.Status != StatusRejected || !r.ResetsAt.IsZero() {
		return bson.M{"$literal": 0}
	}
	return bson.M{"$cond": bson.A{
		bson.M{"$and": bson.A{
			bson.M{"$eq": bson.A{"$status", StatusRejected}},
			// A missing resets_at and the zero time both mean "no reset
			// instant": the field is written unconditionally, so stored
			// docs carry the zero time, while a doc being inserted has
			// no field at all.
			bson.M{"$in": bson.A{bson.M{"$ifNull": bson.A{"$resets_at", time.Time{}.UTC()}}, bson.A{time.Time{}.UTC()}}},
		}},
		bson.M{"$add": bson.A{bson.M{"$ifNull": bson.A{"$refusals", 0}}, 1}},
		1,
	}}
}

// Latest returns the newest reading per window for a credential key.
func (s *MongoStore) Latest(ctx context.Context, key string) ([]Reading, error) {
	cur, err := s.col.Find(ctx, bson.M{"key": key})
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, nil
		}
		return nil, fmt.Errorf("usagecap: latest %s: %w", key, err)
	}
	defer func() { _ = cur.Close(ctx) }()
	var docs []readingDoc
	if err := cur.All(ctx, &docs); err != nil {
		return nil, fmt.Errorf("usagecap: latest %s: decode: %w", key, err)
	}
	out := make([]Reading, 0, len(docs))
	for _, d := range docs {
		out = append(out, Reading{
			Window:      Window(d.Window),
			Utilization: d.Utilization,
			Status:      d.Status,
			ResetsAt:    d.ResetsAt,
			ObservedAt:  d.ObservedAt,
			Refusals:    d.Refusals,
		})
	}
	return out, nil
}

// DeleteByFingerprint drops every document whose key ends in the
// credential's fp segment. A suffix match cannot use the key index, so it
// scans the collection — bounded by design: one document per (credential,
// window) with a 14-day TTL, and this runs once per operator decision.
func (s *MongoStore) DeleteByFingerprint(ctx context.Context, fingerprint string) (int, error) {
	if strings.TrimSpace(fingerprint) == "" {
		return 0, nil
	}
	res, err := s.col.DeleteMany(ctx, bson.M{
		"key": bson.M{"$regex": regexp.QuoteMeta(keyFingerprintSuffix(fingerprint)) + "$"},
	})
	if err != nil {
		return 0, fmt.Errorf("usagecap: delete readings for fingerprint %s: %w", fingerprint, err)
	}
	return int(res.DeletedCount), nil
}

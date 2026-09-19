package cloudsched

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/internal/mongoutil"
	"github.com/SocialGouv/iterion/pkg/internal/storekit"
)

// Collection is the Mongo collection name for scheduled bots.
const Collection = "scheduled_bots"

// MongoStore is the Mongo-backed Store.
type MongoStore struct {
	kit *storekit.Mongo[ScheduledBot]
}

// NewMongoStore builds a Mongo-backed scheduled-bot store.
func NewMongoStore(db *mongo.Database) *MongoStore {
	return &MongoStore{kit: storekit.NewMongo[ScheduledBot](db.Collection(Collection), ErrNotFound, "cloudsched")}
}

var _ Store = (*MongoStore)(nil)

// EnsureSchema creates the indexes. Idempotent.
func EnsureSchema(ctx context.Context, db *mongo.Database) error {
	coll := db.Collection(Collection)
	_, err := coll.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "repo_integration_id", Value: 1}}, Options: options.Index().SetName("tenant_integration")},
		{Keys: bson.D{{Key: "disabled", Value: 1}, {Key: "next_fire_at", Value: 1}}, Options: options.Index().SetName("due")},
	})
	if err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("cloudsched: ensure schema: %w", err)
	}
	return nil
}

func (s *MongoStore) Create(ctx context.Context, sb ScheduledBot) error {
	return s.kit.Insert(ctx, sb, fmt.Errorf("cloudsched: schedule %q already exists", sb.ID), "create")
}

func (s *MongoStore) Get(ctx context.Context, id string) (ScheduledBot, error) {
	return s.kit.GetByID(ctx, id, "get")
}

func (s *MongoStore) ListByIntegration(ctx context.Context, tenantID, integrationID string) ([]ScheduledBot, error) {
	return s.kit.List(ctx, bson.M{"tenant_id": tenantID, "repo_integration_id": integrationID},
		"list by integration", "decode")
}

func (s *MongoStore) ListByTenant(ctx context.Context, tenantID string) ([]ScheduledBot, error) {
	return s.kit.List(ctx, bson.M{"tenant_id": tenantID}, "list by tenant", "decode",
		options.Find().SetSort(bson.D{{Key: "created_at", Value: 1}}))
}

func (s *MongoStore) ListDue(ctx context.Context, now time.Time, limit int) ([]ScheduledBot, error) {
	opt := options.Find().SetSort(bson.D{{Key: "next_fire_at", Value: 1}})
	if limit > 0 {
		opt.SetLimit(int64(limit))
	}
	return s.kit.List(ctx, bson.M{
		"disabled":     bson.M{"$ne": true},
		"next_fire_at": bson.M{"$lte": now},
	}, "list due", "decode", opt)
}

// ClaimTick is the CAS: the update matches only while next_fire_at still equals
// expectedNext, so the first replica to advance it wins and the rest get
// (false, nil). exactly-once per slot, no leader.
func (s *MongoStore) ClaimTick(ctx context.Context, id string, expectedNext, newNext, firedAt time.Time) (bool, error) {
	res, err := s.kit.Coll().UpdateOne(ctx,
		bson.M{"_id": id, "next_fire_at": expectedNext},
		bson.M{"$set": bson.M{"next_fire_at": newNext, "last_fire_at": firedAt, "updated_at": firedAt}},
	)
	if err != nil {
		return false, fmt.Errorf("cloudsched: claim tick: %w", err)
	}
	return res.MatchedCount > 0, nil
}

// MarkLaunchError sets (or clears) the schedule's launch health with a
// targeted field write — never the full-replace Update below uses, because the
// ticker's copy is the one ListDue returned and an operator may have retuned
// the row since.
func (s *MongoStore) MarkLaunchError(ctx context.Context, id, lastError string, at time.Time) error {
	update := bson.M{"$set": bson.M{"last_error": lastError, "last_error_at": at.UTC()}}
	if lastError == "" {
		update = bson.M{"$unset": bson.M{"last_error": "", "last_error_at": ""}}
	}
	res, err := s.kit.Coll().UpdateOne(ctx, bson.M{"_id": id}, update)
	if err != nil {
		return fmt.Errorf("cloudsched: mark launch error: %w", err)
	}
	if res.MatchedCount == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkRunOutcome stamps the last run's terminal outcome on the schedule —
// same targeted-field discipline as MarkLaunchError, because the writer
// (an eventbus subscriber) holds no copy of the row.
//
// The write carries a MONOTONICITY GUARD: the update matches only while the
// row's last_run_at is absent or not newer than `at`, so an out-of-order
// delivery (overlapping runs of one schedule, a NATS redelivery arriving
// after a newer outcome was already stamped) cannot move the health field
// backwards. A zero-match is ambiguous — "row gone" vs "stale outcome" —
// and is disambiguated with one Get: the former is ErrNotFound, the latter
// a silent no-op.
func (s *MongoStore) MarkRunOutcome(ctx context.Context, id, runID, status, errMsg, errCode string, at time.Time) error {
	set := bson.M{
		"last_run_id":     runID,
		"last_run_status": status,
		"last_run_at":     at.UTC(),
	}
	unset := bson.M{}
	if errMsg != "" {
		set["last_run_error"] = errMsg
	} else {
		unset["last_run_error"] = ""
	}
	if errCode != "" {
		set["last_run_error_code"] = errCode
	} else {
		unset["last_run_error_code"] = ""
	}
	update := bson.M{"$set": set}
	if len(unset) > 0 {
		update["$unset"] = unset
	}
	filter := bson.M{
		"_id": id,
		"$or": bson.A{
			bson.M{"last_run_at": bson.M{"$exists": false}},
			bson.M{"last_run_at": bson.M{"$lte": at.UTC()}},
		},
	}
	res, err := s.kit.Coll().UpdateOne(ctx, filter, update)
	if err != nil {
		return fmt.Errorf("cloudsched: mark run outcome: %w", err)
	}
	if res.MatchedCount == 0 {
		if _, getErr := s.Get(ctx, id); getErr != nil {
			if errors.Is(getErr, ErrNotFound) {
				return ErrNotFound
			}
			// A transient store failure must not be laundered into
			// ErrNotFound — the caller warns on real errors and stays
			// silent only on a purged row.
			return fmt.Errorf("cloudsched: mark run outcome: disambiguate zero-match: %w", getErr)
		}
		// The row exists but holds a NEWER outcome: a stale delivery (an
		// overlapping run finishing late, or a redelivery that raced the
		// next tick's stamp). Keeping the newer outcome is the point of
		// the guard — not an error.
		return nil
	}
	return nil
}

// Update applies a partial mutation. Reads the current row, mutates it via
// applySchedulePatch, and writes back via ReplaceOne — the atomicity that
// matters here is exactly-once fire (ClaimTick's CAS), not multi-writer
// serialisation on the mutable payload (Cron/Vars/…), so a full replace is
// safe and matches the semantics operators expect from a REST PATCH.
func (s *MongoStore) Update(ctx context.Context, id string, patch SchedulePatch) (ScheduledBot, error) {
	sb, err := s.Get(ctx, id)
	if err != nil {
		return ScheduledBot{}, err
	}
	applySchedulePatch(&sb, patch)
	if _, err := s.kit.Coll().ReplaceOne(ctx, bson.M{"_id": id}, sb); err != nil {
		return ScheduledBot{}, fmt.Errorf("cloudsched: update: %w", err)
	}
	return sb, nil
}

func (s *MongoStore) Delete(ctx context.Context, id string) error {
	return s.kit.Delete(ctx, id, "delete")
}

func (s *MongoStore) DeleteByIntegration(ctx context.Context, tenantID, integrationID string) error {
	return s.kit.DeleteWhere(ctx, bson.M{"tenant_id": tenantID, "repo_integration_id": integrationID}, "delete by integration")
}

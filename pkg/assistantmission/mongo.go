package assistantmission

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/SocialGouv/iterion/pkg/internal/mongoutil"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const missionsCollection = "assistant_missions"

type MongoStore struct{ missions *mongo.Collection }

func NewMongoStore(db *mongo.Database) *MongoStore {
	return &MongoStore{missions: db.Collection(missionsCollection)}
}

func (s *MongoStore) EnsureSchema(ctx context.Context) error {
	_, err := s.missions.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "operator_id", Value: 1}, {Key: "invocation_key", Value: 1}}, Options: options.Index().SetName("mission_invocation").SetUnique(true)},
		{Keys: bson.D{{Key: "active_target_key", Value: 1}}, Options: options.Index().SetName("one_active_target").SetUnique(true).SetPartialFilterExpression(bson.M{"active_target_key": bson.M{"$type": "string"}})},
		{Keys: bson.D{{Key: "state", Value: 1}, {Key: "lease_until", Value: 1}, {Key: "updated_at", Value: 1}}, Options: options.Index().SetName("mission_reconcile")},
	})
	if err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("assistantmission: ensure indexes: %w", err)
	}
	return nil
}

func mongoScope(scope Scope) bson.M {
	filter := bson.M{"tenant_id": scope.TenantID, "operator_id": scope.OperatorID}
	if scope.TargetRunID != "" {
		filter["target_run_id"] = scope.TargetRunID
	}
	return filter
}

func (s *MongoStore) CreateOrGet(ctx context.Context, requested Mission) (Mission, bool, error) {
	scope := Scope{TenantID: requested.TenantID, OperatorID: requested.OperatorID}
	if existing, err := s.GetByInvocation(ctx, scope, requested.InvocationKey); err == nil {
		if !existing.SameRequest(requested) {
			return Mission{}, false, ErrConflict
		}
		return existing, false, nil
	} else if !errors.Is(err, ErrNotFound) {
		return Mission{}, false, err
	}
	requested.Policy = requested.Policy.Canonical()
	requested.ActiveTargetKey = requested.TenantID + "\x00" + requested.TargetRunID
	_, err := s.missions.InsertOne(ctx, requested)
	if err == nil {
		return requested, true, nil
	}
	if !mongo.IsDuplicateKeyError(err) {
		return Mission{}, false, fmt.Errorf("assistantmission: create: %w", err)
	}
	existing, getErr := s.GetByInvocation(ctx, scope, requested.InvocationKey)
	if getErr == nil && existing.SameRequest(requested) {
		return existing, false, nil
	}
	return Mission{}, false, ErrConflict
}

func (s *MongoStore) Get(ctx context.Context, scope Scope, id string) (Mission, error) {
	filter := mongoScope(scope)
	filter["_id"] = id
	return mongoutil.FindOne[Mission](ctx, s.missions, filter, ErrNotFound, "assistantmission: get")
}

func (s *MongoStore) GetByInvocation(ctx context.Context, scope Scope, key string) (Mission, error) {
	filter := mongoScope(scope)
	filter["invocation_key"] = key
	return mongoutil.FindOne[Mission](ctx, s.missions, filter, ErrNotFound, "assistantmission: get invocation")
}

func (s *MongoStore) find(ctx context.Context, filter bson.M, limit int) ([]Mission, error) {
	opts := options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}})
	if limit > 0 {
		opts.SetLimit(int64(limit))
	}
	cur, err := s.missions.Find(ctx, filter, opts)
	if err != nil {
		return nil, fmt.Errorf("assistantmission: find: %w", err)
	}
	defer cur.Close(ctx)
	var out []Mission
	if err := cur.All(ctx, &out); err != nil {
		return nil, fmt.Errorf("assistantmission: decode: %w", err)
	}
	return out, nil
}

func (s *MongoStore) List(ctx context.Context, scope Scope, limit int) ([]Mission, error) {
	return s.find(ctx, mongoScope(scope), limit)
}

func (s *MongoStore) ListReconcileCandidates(ctx context.Context, _ time.Time, limit int) ([]Mission, error) {
	return s.find(ctx, bson.M{"state": bson.M{"$nin": bson.A{StateCompleted, StateExpired, StateStopped, StateExhausted}}}, limit)
}

func (s *MongoStore) Claim(ctx context.Context, id, owner string, now time.Time, lease time.Duration) (Mission, bool, error) {
	filter := bson.M{"_id": id, "state": bson.M{"$nin": bson.A{StateCompleted, StateExpired, StateStopped, StateExhausted}}, "$or": bson.A{
		bson.M{"lease_until": bson.M{"$lte": now}}, bson.M{"lease_until": nil}, bson.M{"lease_until": bson.M{"$exists": false}}, bson.M{"lease_owner": owner},
	}}
	until := now.Add(lease)
	update := bson.M{"$set": bson.M{"lease_owner": owner, "lease_until": until, "updated_at": now}, "$inc": bson.M{"lease_epoch": 1, "revision": 1}}
	var out Mission
	err := s.missions.FindOneAndUpdate(ctx, filter, update, options.FindOneAndUpdate().SetReturnDocument(options.After)).Decode(&out)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return Mission{}, false, nil
	}
	if err != nil {
		return Mission{}, false, fmt.Errorf("assistantmission: claim: %w", err)
	}
	return out, true, nil
}

func (s *MongoStore) UpdateClaimed(ctx context.Context, next Mission, owner string) (Mission, error) {
	current, err := mongoutil.FindOne[Mission](ctx, s.missions, bson.M{"_id": next.ID}, ErrNotFound, "assistantmission: load claimed")
	if err != nil {
		return Mission{}, err
	}
	if current.LeaseOwner != owner || current.LeaseEpoch != next.LeaseEpoch || current.Revision != next.Revision {
		return Mission{}, ErrClaimLost
	}
	if !current.SameRequest(next) || current.CreatedAt != next.CreatedAt || current.InvocationKey != next.InvocationKey {
		return Mission{}, ErrConflict
	}
	next.Revision++
	if next.State.Terminal() {
		next.ActiveTargetKey, next.LeaseOwner, next.LeaseUntil = "", "", nil
	}
	res, err := s.missions.ReplaceOne(ctx, bson.M{"_id": next.ID, "revision": current.Revision, "lease_owner": owner, "lease_epoch": current.LeaseEpoch}, next)
	if err != nil {
		return Mission{}, fmt.Errorf("assistantmission: update claimed: %w", err)
	}
	if res.MatchedCount == 0 {
		return Mission{}, ErrClaimLost
	}
	return next, nil
}

func (s *MongoStore) RequestStop(ctx context.Context, scope Scope, id, reason string, now time.Time) (Mission, error) {
	filter := mongoScope(scope)
	filter["_id"] = id
	filter["state"] = bson.M{"$nin": bson.A{StateCompleted, StateExpired, StateStopped, StateExhausted}}
	update := bson.M{"$set": bson.M{"state": StateStopped, "reason": reason, "updated_at": now, "terminal_at": now}, "$unset": bson.M{"active_target_key": "", "lease_owner": "", "lease_until": ""}, "$inc": bson.M{"revision": 1}}
	var out Mission
	err := s.missions.FindOneAndUpdate(ctx, filter, update, options.FindOneAndUpdate().SetReturnDocument(options.After)).Decode(&out)
	if err == nil {
		return out, nil
	}
	if !errors.Is(err, mongo.ErrNoDocuments) {
		return Mission{}, fmt.Errorf("assistantmission: stop: %w", err)
	}
	return s.Get(ctx, scope, id)
}

var _ Store = (*MongoStore)(nil)

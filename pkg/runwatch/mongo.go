package runwatch

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

const (
	watchesCollection  = "assistant_run_watches"
	episodesCollection = "assistant_run_watch_episodes"
)

type MongoStore struct {
	watches  *mongo.Collection
	episodes *mongo.Collection
}

func NewMongoStore(db *mongo.Database) *MongoStore {
	return &MongoStore{watches: db.Collection(watchesCollection), episodes: db.Collection(episodesCollection)}
}

func (s *MongoStore) EnsureSchema(ctx context.Context) error {
	_, err := s.watches.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "target_run_id", Value: 1}, {Key: "state", Value: 1}}, Options: options.Index().SetName("target_active")},
		{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "assistant_run_id", Value: 1}, {Key: "state", Value: 1}}, Options: options.Index().SetName("assistant_active")},
		{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "owner_id", Value: 1}, {Key: "target_run_id", Value: 1}}, Options: options.Index().SetName("one_active_owner_target").SetUnique(true).SetPartialFilterExpression(bson.M{"state": WatchActive})},
	})
	if err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("runwatch: ensure watch indexes: %w", err)
	}
	_, err = s.episodes.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "watch_id", Value: 1}, {Key: "outcome_event_id", Value: 1}}, Options: options.Index().SetName("watch_outcome").SetUnique(true)},
		{Keys: bson.D{{Key: "state", Value: 1}, {Key: "next_attempt_at", Value: 1}, {Key: "lease_until", Value: 1}}, Options: options.Index().SetName("due_claim")},
	})
	if err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("runwatch: ensure episode indexes: %w", err)
	}
	return nil
}

func (s *MongoStore) CreateWatch(ctx context.Context, w Watch) error {
	_, err := s.watches.InsertOne(ctx, w)
	if mongo.IsDuplicateKeyError(err) {
		return ErrAlreadyExists
	}
	if err != nil {
		return fmt.Errorf("runwatch: create watch: %w", err)
	}
	return nil
}

// ReconfigureActiveWatch updates only request-owned configuration on the
// matching active link. The filter deliberately includes assistant_run_id: a
// different assistant may not take over an owner's existing target watch.
func (s *MongoStore) ReconfigureActiveWatch(ctx context.Context, requested Watch) (Watch, bool, error) {
	filter := bson.M{
		"tenant_id": requested.TenantID, "owner_id": requested.OwnerID,
		"target_run_id": requested.TargetRunID, "assistant_run_id": requested.AssistantRunID,
		"state": WatchActive,
	}
	update := bson.M{"$set": bson.M{
		"mode": requested.Mode, "kinds": requested.Kinds,
		"max_episodes": 0, "cooldown_seconds": requested.CooldownSeconds,
		"updated_at": requested.UpdatedAt,
	}}
	var out Watch
	err := s.watches.FindOneAndUpdate(ctx, filter, update, options.FindOneAndUpdate().SetReturnDocument(options.After)).Decode(&out)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return Watch{}, false, nil
	}
	if err != nil {
		return Watch{}, false, fmt.Errorf("runwatch: reconfigure active watch: %w", err)
	}
	return out, true, nil
}

// TransferActiveWatch is deliberately separate from ReconfigureActiveWatch:
// the latter must stay assistant-scoped for ordinary policy changes. Here the
// outgoing assistant id is an explicit compare-and-swap guard for a requested
// handoff, while the durable row and its delivery/tree state remain intact.
func (s *MongoStore) TransferActiveWatch(ctx context.Context, fromAssistantID string, requested Watch) (Watch, bool, error) {
	filter := bson.M{
		"tenant_id": requested.TenantID, "owner_id": requested.OwnerID,
		"target_run_id": requested.TargetRunID, "assistant_run_id": fromAssistantID,
		"state": WatchActive,
	}
	update := bson.M{"$set": bson.M{
		"assistant_run_id": requested.AssistantRunID,
		"mode":             requested.Mode, "kinds": requested.Kinds,
		"max_episodes": 0, "cooldown_seconds": requested.CooldownSeconds,
		"updated_at": requested.UpdatedAt,
	}}
	var out Watch
	err := s.watches.FindOneAndUpdate(ctx, filter, update, options.FindOneAndUpdate().SetReturnDocument(options.After)).Decode(&out)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return Watch{}, false, nil
	}
	if err != nil {
		return Watch{}, false, fmt.Errorf("runwatch: transfer active watch: %w", err)
	}
	return out, true, nil
}

func (s *MongoStore) GetWatch(ctx context.Context, id string) (Watch, error) {
	return mongoutil.FindOne[Watch](ctx, s.watches, bson.M{"_id": id}, ErrNotFound, "runwatch: get watch")
}

func (s *MongoStore) findWatches(ctx context.Context, filter bson.M, limit int64) ([]Watch, error) {
	opts := options.Find().SetSort(bson.D{{Key: "created_at", Value: 1}})
	if limit > 0 {
		opts.SetLimit(limit)
	}
	cur, err := s.watches.Find(ctx, filter, opts)
	if err != nil {
		return nil, fmt.Errorf("runwatch: find watches: %w", err)
	}
	defer cur.Close(ctx)
	var out []Watch
	if err := cur.All(ctx, &out); err != nil {
		return nil, fmt.Errorf("runwatch: decode watches: %w", err)
	}
	return out, nil
}

func (s *MongoStore) ListActiveByTarget(ctx context.Context, tenant, id string) ([]Watch, error) {
	return s.findWatches(ctx, bson.M{"tenant_id": tenant, "target_run_id": id, "state": WatchActive}, 0)
}
func (s *MongoStore) ListActiveByAssistant(ctx context.Context, tenant, id string) ([]Watch, error) {
	return s.findWatches(ctx, bson.M{"tenant_id": tenant, "assistant_run_id": id, "state": WatchActive}, 0)
}
func (s *MongoStore) ListActive(ctx context.Context, limit int) ([]Watch, error) {
	return s.findWatches(ctx, bson.M{"state": WatchActive}, int64(limit))
}

func (s *MongoStore) StopWatch(ctx context.Context, id, tenant string, state WatchState, reason string, now time.Time) error {
	res, err := s.watches.UpdateOne(ctx, bson.M{"_id": id, "tenant_id": tenant}, bson.M{"$set": bson.M{"state": state, "stop_reason": reason, "updated_at": now}})
	if err != nil {
		return fmt.Errorf("runwatch: stop watch: %w", err)
	}
	if res.MatchedCount == 0 {
		return ErrNotFound
	}
	return nil
}

// AdvanceObservedEventSeq is a compare-and-set maximum. The missing-field
// arm preserves compatibility with watches stored before the cursor existed.
func (s *MongoStore) AdvanceObservedEventSeq(ctx context.Context, id, tenant string, seq int64, now time.Time) error {
	filter := bson.M{
		"_id": id, "tenant_id": tenant,
		"$or": bson.A{
			bson.M{"last_observed_event_seq": bson.M{"$lt": seq}},
			bson.M{"last_observed_event_seq": bson.M{"$exists": false}},
		},
	}
	_, err := s.watches.UpdateOne(ctx, filter, bson.M{"$set": bson.M{"last_observed_event_seq": seq, "updated_at": now}})
	if err != nil {
		return fmt.Errorf("runwatch: advance observed event: %w", err)
	}
	return nil
}

// InitializeTreeTracking is a one-shot CAS. During a rolling deploy only the
// first coordinator chooses the legacy replay floor; every later caller reads
// that same timestamp.
func (s *MongoStore) InitializeTreeTracking(ctx context.Context, id, tenant string, started, now time.Time) (Watch, error) {
	filter := bson.M{
		"_id": id, "tenant_id": tenant,
		"tree_tracking_started_at": bson.M{"$exists": false},
	}
	update := bson.M{"$set": bson.M{"tree_tracking_started_at": started, "updated_at": now}}
	var out Watch
	err := s.watches.FindOneAndUpdate(ctx, filter, update, options.FindOneAndUpdate().SetReturnDocument(options.After)).Decode(&out)
	if err == nil {
		return out, nil
	}
	if !errors.Is(err, mongo.ErrNoDocuments) {
		return Watch{}, fmt.Errorf("runwatch: initialize tree tracking: %w", err)
	}
	return s.GetWatch(ctx, id)
}

// EnsureRunObservation appends one array element atomically. The $ne filter
// makes concurrent first observations converge without dynamic BSON keys.
func (s *MongoStore) EnsureRunObservation(ctx context.Context, id, tenant string, observation RunObservation, now time.Time) (RunObservation, bool, error) {
	filter := bson.M{
		"_id": id, "tenant_id": tenant,
		"observations.run_id": bson.M{"$ne": observation.RunID},
	}
	res, err := s.watches.UpdateOne(ctx, filter, bson.M{
		"$push": bson.M{"observations": observation},
		"$set":  bson.M{"updated_at": now},
	})
	if err != nil {
		return RunObservation{}, false, fmt.Errorf("runwatch: ensure run observation: %w", err)
	}
	if res.ModifiedCount == 1 {
		return observation, true, nil
	}
	w, err := s.GetWatch(ctx, id)
	if err != nil {
		return RunObservation{}, false, err
	}
	if w.TenantID != tenant {
		return RunObservation{}, false, ErrNotFound
	}
	for _, existing := range w.Observations {
		if existing.RunID == observation.RunID {
			return existing, false, nil
		}
	}
	return RunObservation{}, false, ErrNotFound
}

// AdvanceObservedRunEventSeq is a per-array-element compare-and-set maximum.
// $max keeps the compatibility root cursor monotonic as well.
func (s *MongoStore) AdvanceObservedRunEventSeq(ctx context.Context, id, tenant, runID string, seq int64, now time.Time) error {
	filter := bson.M{
		"_id": id, "tenant_id": tenant,
		"observations": bson.M{"$elemMatch": bson.M{"run_id": runID, "event_seq": bson.M{"$lt": seq}}},
	}
	update := bson.M{"$set": bson.M{"observations.$.event_seq": seq, "updated_at": now}}
	if watch, err := s.GetWatch(ctx, id); err == nil && watch.TargetRunID == runID {
		update["$max"] = bson.M{"last_observed_event_seq": seq}
	}
	res, err := s.watches.UpdateOne(ctx, filter, update)
	if err != nil {
		return fmt.Errorf("runwatch: advance observed run event: %w", err)
	}
	if res.MatchedCount > 0 {
		return nil
	}
	w, err := s.GetWatch(ctx, id)
	if err != nil {
		return err
	}
	for _, observation := range w.Observations {
		if observation.RunID == runID {
			return nil
		}
	}
	return ErrNotFound
}

func (s *MongoStore) CreateEpisode(ctx context.Context, ep Episode) (bool, error) {
	_, err := s.episodes.InsertOne(ctx, ep)
	if mongo.IsDuplicateKeyError(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("runwatch: create episode: %w", err)
	}
	return true, nil
}

func (s *MongoStore) ListEpisodesByWatch(ctx context.Context, watchID, tenant string, limit int) ([]Episode, error) {
	if limit <= 0 || limit > maxEpisodeReadLimit {
		limit = maxEpisodeReadLimit
	}
	opts := options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}, {Key: "_id", Value: -1}}).SetLimit(int64(limit))
	cur, err := s.episodes.Find(ctx, bson.M{"watch_id": watchID, "tenant_id": tenant}, opts)
	if err != nil {
		return nil, fmt.Errorf("runwatch: list episodes by watch: %w", err)
	}
	defer cur.Close(ctx)
	out := make([]Episode, 0)
	if err := cur.All(ctx, &out); err != nil {
		return nil, fmt.Errorf("runwatch: decode episodes by watch: %w", err)
	}
	return out, nil
}

func (s *MongoStore) ListDueEpisodes(ctx context.Context, now time.Time, limit int) ([]Episode, error) {
	filter := bson.M{"next_attempt_at": bson.M{"$lte": now}, "$or": bson.A{
		bson.M{"state": EpisodePending},
		bson.M{"state": EpisodeProcessing, "$or": bson.A{bson.M{"lease_until": bson.M{"$lte": now}}, bson.M{"lease_until": nil}}},
	}}
	opts := options.Find().SetSort(bson.D{{Key: "next_attempt_at", Value: 1}}).SetLimit(int64(limit))
	cur, err := s.episodes.Find(ctx, filter, opts)
	if err != nil {
		return nil, fmt.Errorf("runwatch: list due episodes: %w", err)
	}
	defer cur.Close(ctx)
	var out []Episode
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *MongoStore) ClaimEpisode(ctx context.Context, id, owner string, now time.Time, lease time.Duration) (Episode, bool, error) {
	filter := bson.M{"_id": id, "next_attempt_at": bson.M{"$lte": now}, "$or": bson.A{
		bson.M{"state": EpisodePending},
		bson.M{"state": EpisodeProcessing, "$or": bson.A{bson.M{"lease_until": bson.M{"$lte": now}}, bson.M{"lease_until": nil}}},
	}}
	update := bson.M{"$set": bson.M{"state": EpisodeProcessing, "lease_owner": owner, "lease_until": now.Add(lease), "updated_at": now}, "$inc": bson.M{"attempts": 1}}
	var ep Episode
	err := s.episodes.FindOneAndUpdate(ctx, filter, update, options.FindOneAndUpdate().SetReturnDocument(options.After)).Decode(&ep)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return Episode{}, false, nil
	}
	if err != nil {
		return Episode{}, false, fmt.Errorf("runwatch: claim episode: %w", err)
	}
	return ep, true, nil
}

func (s *MongoStore) ReleaseEpisode(ctx context.Context, id, owner string, next time.Time, msg string) error {
	return s.finish(ctx, id, owner, EpisodePending, next, msg, false)
}
func (s *MongoStore) BlockEpisode(ctx context.Context, id, owner string, now time.Time, msg string) error {
	return s.finish(ctx, id, owner, EpisodeBlocked, now, msg, false)
}
func (s *MongoStore) CompleteEpisode(ctx context.Context, id, owner string, now time.Time) error {
	return s.finish(ctx, id, owner, EpisodeDone, now, "", true)
}

func (s *MongoStore) finish(ctx context.Context, id, owner string, state EpisodeState, at time.Time, msg string, delivered bool) error {
	set := bson.M{"state": state, "lease_owner": "", "lease_until": nil, "next_attempt_at": at, "last_error": msg, "updated_at": at}
	if delivered {
		set["delivered_at"] = at
	}
	var ep Episode
	err := s.episodes.FindOneAndUpdate(ctx, bson.M{"_id": id, "state": EpisodeProcessing, "lease_owner": owner}, bson.M{"$set": set}, options.FindOneAndUpdate().SetReturnDocument(options.After)).Decode(&ep)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("runwatch: finish episode: %w", err)
	}
	if delivered {
		_, err = s.watches.UpdateOne(ctx, bson.M{"_id": ep.WatchID}, bson.M{"$inc": bson.M{"delivered_episodes": 1}, "$set": bson.M{"last_delivered_at": at, "updated_at": at}})
	}
	return err
}

var _ Store = (*MongoStore)(nil)

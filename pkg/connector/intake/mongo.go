package intake

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
	colIntakes = "connector_intakes"
	colPairs   = "connector_pairs"
)

// MongoStore is the cloud twin. Every state change is one conditional update
// on one document: a claim is a FindOneAndUpdate on the claimable predicate,
// and a fenced write matches the row's id, state and generation together.
type MongoStore struct {
	intakes *mongo.Collection
	pairs   *mongo.Collection
}

// NewMongoStore returns the store over db; EnsureSchema creates its indexes.
func NewMongoStore(db *mongo.Database) *MongoStore {
	return &MongoStore{intakes: db.Collection(colIntakes), pairs: db.Collection(colPairs)}
}

// EnsureSchema creates the indexes idempotently.
//
// The claim query reads (state, not_before); the dead-letter view reads a
// tenant's rows by state, newest first; a pair list reads an intake's pairs;
// a polled pair's launch reads its subscription's launched pairs of the same
// item, earliest first.
// expires_at is a TTL index with no delay: a row expires at its own instant,
// and a row without one never does.
func EnsureSchema(ctx context.Context, db *mongo.Database) error {
	ttl := options.Index().SetExpireAfterSeconds(0)
	if _, err := db.Collection(colIntakes).Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "state", Value: 1}, {Key: "not_before", Value: 1}, {Key: "_id", Value: 1}}, Options: options.Index().SetName("intake_claim")},
		{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "state", Value: 1}, {Key: "received_at", Value: -1}}, Options: options.Index().SetName("intake_tenant_state_recent")},
		{Keys: bson.D{{Key: "expires_at", Value: 1}}, Options: ttl.SetName("intake_expiry")},
		// A backfill reads a poller's rows by sequence, highest first.
		{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "poller_id", Value: 1}, {Key: "sequence", Value: -1}}, Options: options.Index().SetName("intake_tenant_poller_sequence")},
	}); err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("intake: ensure intake indexes: %w", err)
	}
	ttl = options.Index().SetExpireAfterSeconds(0)
	if _, err := db.Collection(colPairs).Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "state", Value: 1}, {Key: "not_before", Value: 1}, {Key: "_id", Value: 1}}, Options: options.Index().SetName("pair_claim")},
		{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "intake_id", Value: 1}, {Key: "subscription_id", Value: 1}}, Options: options.Index().SetName("pair_tenant_intake")},
		{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "state", Value: 1}, {Key: "received_at", Value: -1}}, Options: options.Index().SetName("pair_tenant_state_recent")},
		{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "subscription_id", Value: 1}, {Key: "item_key", Value: 1}, {Key: "state", Value: 1}, {Key: "launched_at", Value: 1}, {Key: "_id", Value: 1}}, Options: options.Index().SetName("pair_subscription_item_launched")},
		{Keys: bson.D{{Key: "expires_at", Value: 1}}, Options: ttl.SetName("pair_expiry")},
	}); err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("intake: ensure pair indexes: %w", err)
	}
	return nil
}

func (s *MongoStore) Accept(ctx context.Context, in Intake) (Intake, bool, error) {
	in, err := prepareIntake(in)
	if err != nil {
		return Intake{}, false, err
	}
	res, err := s.intakes.UpdateOne(ctx, bson.M{"_id": in.ID},
		bson.M{"$setOnInsert": in}, options.UpdateOne().SetUpsert(true))
	if err != nil && !mongo.IsDuplicateKeyError(err) {
		return Intake{}, false, fmt.Errorf("intake: accept %s: %w", in.ID, err)
	}
	// A duplicate key is a concurrent Accept of the same delivery: the row
	// exists, which is the answer.
	created := err == nil && res.UpsertedCount == 1
	var stored Intake
	if err := s.intakes.FindOne(ctx, bson.M{"_id": in.ID}).Decode(&stored); err != nil {
		// The write's verdict stands even when its read-back fails: the
		// caller tells its own row from a twin's by it.
		return Intake{}, created, fmt.Errorf("intake: read accepted %s: %w", in.ID, err)
	}
	return stored, created, nil
}

func (s *MongoStore) GetIntake(ctx context.Context, tenantID, id string) (Intake, error) {
	var in Intake
	err := s.intakes.FindOne(ctx, bson.M{"_id": id, "tenant_id": tenantID}).Decode(&in)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return Intake{}, fmt.Errorf("%w: intake %s", ErrNotFound, id)
	}
	if err != nil {
		return Intake{}, fmt.Errorf("intake: get intake %s: %w", id, err)
	}
	return in, nil
}

func (s *MongoStore) CountBacklog(ctx context.Context, tenantID string) (int, error) {
	n, err := s.intakes.CountDocuments(ctx, bson.M{"tenant_id": tenantID, "state": bson.M{"$in": bson.A{IntakePending, IntakeClaimed}}})
	if err != nil {
		return 0, fmt.Errorf("intake: count backlog: %w", err)
	}
	return int(n), nil
}

func (s *MongoStore) ListIntakes(ctx context.Context, tenantID, state string, limit int) ([]Intake, error) {
	filter := bson.M{"tenant_id": tenantID}
	if state != "" {
		filter["state"] = state
	}
	opts := options.Find().SetSort(bson.D{{Key: "received_at", Value: -1}, {Key: "_id", Value: 1}})
	if limit > 0 {
		opts.SetLimit(int64(limit))
	}
	cur, err := s.intakes.Find(ctx, filter, opts)
	if err != nil {
		return nil, fmt.Errorf("intake: list intakes: %w", err)
	}
	var out []Intake
	if err := cur.All(ctx, &out); err != nil {
		return nil, fmt.Errorf("intake: decode intakes: %w", err)
	}
	return out, nil
}

func (s *MongoStore) ListPollerIntakes(ctx context.Context, tenantID, pollerID string, maxSeq int64, limit int) ([]Intake, error) {
	if pollerID == "" {
		return nil, nil
	}
	filter := bson.M{"tenant_id": tenantID, "poller_id": pollerID, "sequence": bson.M{"$lte": maxSeq}}
	opts := options.Find().SetSort(bson.D{{Key: "sequence", Value: -1}, {Key: "_id", Value: 1}})
	if limit > 0 {
		opts.SetLimit(int64(limit))
	}
	cur, err := s.intakes.Find(ctx, filter, opts)
	if err != nil {
		return nil, fmt.Errorf("intake: list poller %s intakes: %w", pollerID, err)
	}
	var out []Intake
	if err := cur.All(ctx, &out); err != nil {
		return nil, fmt.Errorf("intake: decode poller %s intakes: %w", pollerID, err)
	}
	return out, nil
}

// claim flips up to limit claimable rows of col to claimed for owner —
// pending rows past their NotBefore and claims past their expiry, in one
// NotBefore order — one atomic update per row. The update is a pipeline so
// the attempt a reclaim spends is decided on the row's own state in the same
// write. decode appends each flipped row to the caller's result.
func claim(ctx context.Context, col *mongo.Collection, pending, claimed, owner string, now time.Time, ttl time.Duration, limit int, decode func(*mongo.SingleResult) error) error {
	if err := checkClaim(owner, ttl, limit); err != nil {
		return err
	}
	now = stamp(now)
	update := mongo.Pipeline{{{Key: "$set", Value: bson.M{
		"attempts": bson.M{"$cond": bson.A{
			bson.M{"$eq": bson.A{"$state", bson.M{"$literal": claimed}}},
			bson.M{"$add": bson.A{"$attempts", 1}},
			"$attempts",
		}},
		"generation": bson.M{"$add": bson.A{"$generation", 1}},
		"state":      bson.M{"$literal": claimed},
		"owner":      bson.M{"$literal": owner},
		"not_before": stamp(now.Add(ttl)),
		"updated_at": now,
	}}}}
	for got := 0; got < limit; got++ {
		res := col.FindOneAndUpdate(ctx,
			bson.M{"state": bson.M{"$in": bson.A{pending, claimed}}, "not_before": bson.M{"$lte": now}},
			update,
			options.FindOneAndUpdate().
				SetSort(bson.D{{Key: "not_before", Value: 1}, {Key: "_id", Value: 1}}).
				SetReturnDocument(options.After))
		if errors.Is(res.Err(), mongo.ErrNoDocuments) {
			return nil
		}
		if res.Err() != nil {
			return fmt.Errorf("intake: claim from %s: %w", col.Name(), res.Err())
		}
		if err := decode(res); err != nil {
			return fmt.Errorf("intake: decode claim from %s: %w", col.Name(), err)
		}
	}
	return nil
}

func (s *MongoStore) ClaimIntakes(ctx context.Context, owner string, now time.Time, ttl time.Duration, limit int) ([]Intake, error) {
	var out []Intake
	err := claim(ctx, s.intakes, IntakePending, IntakeClaimed, owner, now, ttl, limit, func(res *mongo.SingleResult) error {
		var in Intake
		if err := res.Decode(&in); err != nil {
			return err
		}
		out = append(out, in)
		return nil
	})
	return out, err
}

// fenced applies update to the row id of col while it is claimed at
// generation, and says why not otherwise.
func fenced(ctx context.Context, col *mongo.Collection, kind, claimed, id string, generation int64, update bson.M) error {
	res, err := col.UpdateOne(ctx, bson.M{"_id": id, "state": claimed, "generation": generation}, update)
	if err != nil {
		return fmt.Errorf("intake: update %s %s: %w", kind, id, err)
	}
	if res.MatchedCount == 1 {
		return nil
	}
	return missedFence(ctx, col, kind, id, generation)
}

// missedFence explains a fenced write that matched nothing: the row is gone,
// or its claim is no longer the writer's.
func missedFence(ctx context.Context, col *mongo.Collection, kind, id string, generation int64) error {
	var cur struct {
		State      string `bson:"state"`
		Generation int64  `bson:"generation"`
	}
	err := col.FindOne(ctx, bson.M{"_id": id}).Decode(&cur)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return fmt.Errorf("%w: %s %s", ErrNotFound, kind, id)
	}
	if err != nil {
		return fmt.Errorf("intake: read %s %s after a missed fenced write: %w", kind, id, err)
	}
	return fmt.Errorf("%w: %s %s is %s at generation %d, the write holds generation %d", ErrClaimLost, kind, id, cur.State, cur.Generation, generation)
}

func (s *MongoStore) MarkIntakeMatched(ctx context.Context, id string, generation int64, at time.Time) error {
	at = stamp(at)
	return fenced(ctx, s.intakes, "intake", IntakeClaimed, id, generation, bson.M{
		"$set":   bson.M{"state": IntakeMatched, "matched_at": at, "updated_at": at},
		"$unset": bson.M{"owner": "", "last_error": ""},
	})
}

func (s *MongoStore) RetryIntake(ctx context.Context, id string, generation int64, attempts int, notBefore time.Time, lastErr string) error {
	return fenced(ctx, s.intakes, "intake", IntakeClaimed, id, generation, bson.M{
		"$set": bson.M{"state": IntakePending, "attempts": attempts, "not_before": stamp(notBefore),
			"last_error": lastErr, "updated_at": stamp(time.Now())},
		"$unset": bson.M{"owner": ""},
	})
}

func (s *MongoStore) BuryIntake(ctx context.Context, id string, generation int64, attempts int, lastErr string) error {
	return fenced(ctx, s.intakes, "intake", IntakeClaimed, id, generation, bson.M{
		"$set":   bson.M{"state": IntakeDead, "attempts": attempts, "last_error": lastErr, "updated_at": stamp(time.Now())},
		"$unset": bson.M{"owner": ""},
	})
}

func (s *MongoStore) ReplayIntake(ctx context.Context, tenantID, id string, now time.Time) (Intake, error) {
	var in Intake
	err := s.intakes.FindOneAndUpdate(ctx,
		bson.M{"_id": id, "tenant_id": tenantID, "state": IntakeDead},
		bson.M{
			"$set":   bson.M{"state": IntakePending, "attempts": 0, "not_before": stamp(now), "updated_at": stamp(now)},
			"$inc":   bson.M{"generation": 1},
			"$unset": bson.M{"last_error": ""},
		},
		options.FindOneAndUpdate().SetReturnDocument(options.After)).Decode(&in)
	if err == nil {
		return in, nil
	}
	if !errors.Is(err, mongo.ErrNoDocuments) {
		return Intake{}, fmt.Errorf("intake: replay intake %s: %w", id, err)
	}
	cur, gerr := s.GetIntake(ctx, tenantID, id)
	if gerr != nil {
		return Intake{}, gerr
	}
	return Intake{}, fmt.Errorf("%w: intake %s is %s", ErrNotReplayable, id, cur.State)
}

func (s *MongoStore) WritePairs(ctx context.Context, pairs []Pair) error {
	if len(pairs) == 0 {
		return nil
	}
	models := make([]mongo.WriteModel, 0, len(pairs))
	for _, p := range pairs {
		p, err := preparePair(p)
		if err != nil {
			return err
		}
		models = append(models, mongo.NewUpdateOneModel().
			SetFilter(bson.M{"_id": p.ID}).
			SetUpdate(bson.M{"$setOnInsert": p}).
			SetUpsert(true))
	}
	_, err := s.pairs.BulkWrite(ctx, models, options.BulkWrite().SetOrdered(false))
	if err != nil && !onlyDuplicateKeys(err) {
		return fmt.Errorf("intake: write pairs: %w", err)
	}
	return nil
}

// onlyDuplicateKeys reports a bulk upsert whose every failure is a duplicate
// key: concurrent writers of the same pairs, whose rows exist — the answer.
func onlyDuplicateKeys(err error) bool {
	var bwe mongo.BulkWriteException
	if !errors.As(err, &bwe) || bwe.WriteConcernError != nil || len(bwe.WriteErrors) == 0 {
		return false
	}
	for _, we := range bwe.WriteErrors {
		if we.Code != 11000 {
			return false
		}
	}
	return true
}

func (s *MongoStore) GetPair(ctx context.Context, tenantID, id string) (Pair, error) {
	var p Pair
	err := s.pairs.FindOne(ctx, bson.M{"_id": id, "tenant_id": tenantID}).Decode(&p)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return Pair{}, fmt.Errorf("%w: pair %s", ErrNotFound, id)
	}
	if err != nil {
		return Pair{}, fmt.Errorf("intake: get pair %s: %w", id, err)
	}
	return p, nil
}

func (s *MongoStore) ListPairs(ctx context.Context, tenantID string, q PairQuery, limit int) ([]Pair, error) {
	filter := bson.M{"tenant_id": tenantID}
	if q.IntakeID != "" {
		filter["intake_id"] = q.IntakeID
	}
	if q.State != "" {
		filter["state"] = q.State
	}
	opts := options.Find().SetSort(bson.D{{Key: "received_at", Value: -1}, {Key: "intake_id", Value: 1}, {Key: "subscription_id", Value: 1}})
	if limit > 0 {
		opts.SetLimit(int64(limit))
	}
	cur, err := s.pairs.Find(ctx, filter, opts)
	if err != nil {
		return nil, fmt.Errorf("intake: list pairs: %w", err)
	}
	var out []Pair
	if err := cur.All(ctx, &out); err != nil {
		return nil, fmt.Errorf("intake: decode pairs: %w", err)
	}
	return out, nil
}

func (s *MongoStore) LaunchedPair(ctx context.Context, tenantID, subscriptionID, itemKey string) (Pair, bool, error) {
	if err := checkItemKey(subscriptionID, itemKey); err != nil {
		return Pair{}, false, err
	}
	var p Pair
	err := s.pairs.FindOne(ctx,
		bson.M{"tenant_id": tenantID, "subscription_id": subscriptionID, "item_key": itemKey, "state": PairLaunched},
		options.FindOne().SetSort(bson.D{{Key: "launched_at", Value: 1}, {Key: "_id", Value: 1}})).Decode(&p)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return Pair{}, false, nil
	}
	if err != nil {
		return Pair{}, false, fmt.Errorf("intake: read subscription %s's launched pair of item %s: %w", subscriptionID, itemKey, err)
	}
	return p, true, nil
}

func (s *MongoStore) ClaimPairs(ctx context.Context, owner string, now time.Time, ttl time.Duration, limit int) ([]Pair, error) {
	var out []Pair
	err := claim(ctx, s.pairs, PairPending, PairClaimed, owner, now, ttl, limit, func(res *mongo.SingleResult) error {
		var p Pair
		if err := res.Decode(&p); err != nil {
			return err
		}
		out = append(out, p)
		return nil
	})
	return out, err
}

func (s *MongoStore) MarkPairLaunched(ctx context.Context, id string, generation int64, runID string, at time.Time) error {
	if runID == "" {
		return fmt.Errorf("%w: pair %s launched without a run id", ErrInvalid, id)
	}
	at = stamp(at)
	return fenced(ctx, s.pairs, "pair", PairClaimed, id, generation, bson.M{
		"$set":   bson.M{"state": PairLaunched, "run_id": runID, "launched_at": at, "updated_at": at},
		"$unset": bson.M{"owner": "", "last_error": ""},
	})
}

func (s *MongoStore) RetryPair(ctx context.Context, id string, generation int64, r PairRetry) error {
	// The epoch never moves back: the filter carries it, so a retry that
	// would lower it misses and reads as what it is.
	res, err := s.pairs.UpdateOne(ctx,
		bson.M{"_id": id, "state": PairClaimed, "generation": generation, "epoch": bson.M{"$lte": r.Epoch}},
		bson.M{
			"$set": bson.M{"state": PairPending, "attempts": r.Attempts, "not_before": stamp(r.NotBefore),
				"last_error": r.LastError, "epoch": r.Epoch, "updated_at": stamp(time.Now())},
			"$unset": bson.M{"owner": ""},
		})
	if err != nil {
		return fmt.Errorf("intake: retry pair %s: %w", id, err)
	}
	if res.MatchedCount == 1 {
		return nil
	}
	var cur Pair
	if gerr := s.pairs.FindOne(ctx, bson.M{"_id": id}).Decode(&cur); gerr == nil &&
		cur.State == PairClaimed && cur.Generation == generation && cur.Epoch > r.Epoch {
		return fmt.Errorf("%w: pair %s is at epoch %d, a retry cannot move it back to %d", ErrInvalid, id, cur.Epoch, r.Epoch)
	}
	return missedFence(ctx, s.pairs, "pair", id, generation)
}

func (s *MongoStore) RefusePair(ctx context.Context, id string, generation int64, reason string) error {
	return fenced(ctx, s.pairs, "pair", PairClaimed, id, generation, bson.M{
		"$set":   bson.M{"state": PairRefused, "reason": reason, "updated_at": stamp(time.Now())},
		"$unset": bson.M{"owner": ""},
	})
}

func (s *MongoStore) BuryPair(ctx context.Context, id string, generation int64, attempts int, lastErr string) error {
	return fenced(ctx, s.pairs, "pair", PairClaimed, id, generation, bson.M{
		"$set":   bson.M{"state": PairDead, "attempts": attempts, "last_error": lastErr, "updated_at": stamp(time.Now())},
		"$unset": bson.M{"owner": ""},
	})
}

func (s *MongoStore) ReplayPair(ctx context.Context, tenantID, id string, now time.Time) (Pair, error) {
	// One pipeline update: a launch-refused pair moves to the next epoch, a
	// dead one keeps its own (see Store.ReplayPair), decided on the row's own
	// state in the same write.
	var p Pair
	err := s.pairs.FindOneAndUpdate(ctx,
		bson.M{"_id": id, "tenant_id": tenantID, "$or": bson.A{
			bson.M{"state": PairDead},
			bson.M{"state": PairRefused, "match_refusal": bson.M{"$ne": true}},
		}},
		mongo.Pipeline{{{Key: "$set", Value: bson.M{
			"epoch": bson.M{"$cond": bson.A{
				bson.M{"$eq": bson.A{"$state", bson.M{"$literal": PairRefused}}},
				bson.M{"$add": bson.A{"$epoch", 1}},
				"$epoch",
			}},
			"generation": bson.M{"$add": bson.A{"$generation", 1}},
			"state":      bson.M{"$literal": PairPending},
			"attempts":   0,
			"not_before": stamp(now),
			"updated_at": stamp(now),
			"reason":     "$$REMOVE",
			"last_error": "$$REMOVE",
		}}}},
		options.FindOneAndUpdate().SetReturnDocument(options.After)).Decode(&p)
	if err == nil {
		return p, nil
	}
	if !errors.Is(err, mongo.ErrNoDocuments) {
		return Pair{}, fmt.Errorf("intake: replay pair %s: %w", id, err)
	}
	cur, gerr := s.GetPair(ctx, tenantID, id)
	if gerr != nil {
		return Pair{}, gerr
	}
	if rerr := replayable(&cur); rerr != nil {
		return Pair{}, rerr
	}
	return Pair{}, fmt.Errorf("%w: pair %s changed while it was replayed", ErrNotReplayable, id)
}

func (s *MongoStore) RejudgePair(ctx context.Context, tenantID, id string, v Verdict, now time.Time) (Pair, error) {
	state, reason, matchRefusal := rejudged(v)
	set := bson.M{
		"state":         state,
		"match_refusal": matchRefusal,
		"not_before":    stamp(now),
		"updated_at":    stamp(now),
	}
	unset := bson.M{"last_error": ""}
	if reason != "" {
		set["reason"] = reason
	} else {
		unset["reason"] = ""
	}
	var p Pair
	err := s.pairs.FindOneAndUpdate(ctx,
		bson.M{"_id": id, "tenant_id": tenantID, "state": PairRefused, "match_refusal": true},
		bson.M{"$set": set, "$unset": unset, "$inc": bson.M{"generation": 1}},
		options.FindOneAndUpdate().SetReturnDocument(options.After)).Decode(&p)
	if err == nil {
		return p, nil
	}
	if !errors.Is(err, mongo.ErrNoDocuments) {
		return Pair{}, fmt.Errorf("intake: rejudge pair %s: %w", id, err)
	}
	cur, gerr := s.GetPair(ctx, tenantID, id)
	if gerr != nil {
		return Pair{}, gerr
	}
	return Pair{}, fmt.Errorf("%w: pair %s is %s, not refused by matching", ErrNotReplayable, cur.ID, cur.State)
}

var _ Store = (*MongoStore)(nil)

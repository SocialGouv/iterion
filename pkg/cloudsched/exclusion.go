package cloudsched

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/SocialGouv/iterion/pkg/lease"
	"github.com/SocialGouv/iterion/pkg/schedgate"
	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// The renewable lease covers the slow guard/compile/resolve path. The lease
// alone is insufficient: the run publisher must commit under the ownership
// check installed on this context, in the SAME transaction as its run write.
// An expired worker therefore cannot persist behind a successor's overlap read.
//
// No handover policy (lease.Spec.Health), decided rather than omitted: TryRun
// serves ONE term for ONE schedule launch, so there is no run of passes to
// classify and nothing a term could hand over to — the work either commits
// under this claim or does not, and the scheduler's next tick campaigns again
// on its own cadence. The handover of #2685 is for the repeating nets, whose
// holder keeps a term across passes and can therefore blackhole one.
func (s *MongoStore) WithSchedule(ctx context.Context, id string, fn func(context.Context)) (bool, error) {
	db := s.kit.Coll().Database()
	name, owner := "schedule-launch:"+id, intentClaim()
	return lease.TryRun(ctx, lease.NewMongoStore(db), lease.Spec{Name: name, Owner: owner, TTL: intentLease}, func(ctx context.Context) {
		ctx = store.WithLaunchPersistenceGuard(ctx, func(ctx context.Context, write func(context.Context) error) error {
			session, err := db.Client().StartSession()
			if err != nil {
				return fmt.Errorf("%w: schedule transaction: %v", schedgate.ErrMaintenanceHold, err)
			}
			defer session.EndSession(context.WithoutCancel(ctx))
			_, err = session.WithTransaction(ctx, func(ctx context.Context) (any, error) {
				// Writing this document conflicts with takeover. Checking the token in
				// a separate read would reopen the very read/persistence race we fence.
				res, err := db.Collection(lease.Collection).UpdateOne(ctx,
					bson.M{"_id": name, "owner": owner, "expires_at": bson.M{"$gt": time.Now().UTC()}},
					bson.M{"$inc": bson.M{"launch_commits": 1}})
				if err != nil {
					return nil, err
				}
				if res.MatchedCount != 1 {
					return nil, schedgate.ErrMaintenanceHold
				}
				return nil, write(ctx)
			})
			if err != nil {
				return fmt.Errorf("%w: schedule persistence: %v", schedgate.ErrMaintenanceHold, err)
			}
			return nil
		})
		fn(ctx)
	})
}

func (s *MemoryStore) WithSchedule(ctx context.Context, id string, fn func(context.Context)) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	s.scopeMu.Lock()
	if s.scopes == nil {
		s.scopes = map[string]string{}
	}
	if s.scopes[id] != "" {
		s.scopeMu.Unlock()
		return false, nil
	}
	token := intentClaim()
	s.scopes[id] = token
	s.scopeMu.Unlock()
	defer func() { s.scopeMu.Lock(); delete(s.scopes, id); s.scopeMu.Unlock() }()
	ctx = store.WithLaunchPersistenceGuard(ctx, func(ctx context.Context, write func(context.Context) error) error {
		s.scopeMu.Lock()
		defer s.scopeMu.Unlock()
		if s.scopes[id] != token {
			return schedgate.ErrMaintenanceHold
		}
		return write(ctx)
	})
	fn(ctx)
	return true, nil
}

// ListIntentSchedules ranks candidate schedules by the age of their OLDEST
// due intent, tie-broken on the id. A lexical first-N selection lets a durably
// held prefix re-occupy the whole selection at every tick, so a schedule
// beyond the cap is never attempted; ranking by the oldest due not_before
// rotates the cap toward the work nobody has retried recently (a fresh
// intent's zero not_before is the oldest there is).
func (s *MemoryStore) ListIntentSchedules(_ context.Context, now time.Time, limit int) ([]string, error) {
	s.intentMu.Lock()
	defer s.intentMu.Unlock()
	oldest := map[string]time.Time{}
	for _, r := range s.intents {
		if r.Done || r.NotBefore.After(now) {
			continue
		}
		cur, ok := oldest[r.Schedule.ID]
		if !ok || r.NotBefore.Before(cur) {
			oldest[r.Schedule.ID] = r.NotBefore
		}
	}
	ids := make([]string, 0, len(oldest))
	for id := range oldest {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := oldest[ids[i]], oldest[ids[j]]
		if !a.Equal(b) {
			return a.Before(b)
		}
		return ids[i] < ids[j]
	})
	if limit > 0 && len(ids) > limit {
		ids = ids[:limit]
	}
	return ids, nil
}

// The Mongo twin of the memory selection above: same seniority ranking, pushed
// into an aggregation so the LIMIT lands on the oldest-work-first order rather
// than on the lexical one.
func (s *MongoStore) ListIntentSchedules(ctx context.Context, now time.Time, limit int) ([]string, error) {
	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: bson.M{"done": false, "not_before": bson.M{"$lte": now}}}},
		{{Key: "$group", Value: bson.M{"_id": "$schedule._id", "oldest": bson.M{"$min": "$not_before"}}}},
		{{Key: "$sort", Value: bson.D{{Key: "oldest", Value: 1}, {Key: "_id", Value: 1}}}},
	}
	if limit > 0 {
		pipeline = append(pipeline, bson.D{{Key: "$limit", Value: limit}})
	}
	cur, err := s.intentColl().Aggregate(ctx, pipeline)
	if err != nil {
		return nil, err
	}
	var groups []struct {
		ID string `bson:"_id"`
	}
	if err := cur.All(ctx, &groups); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(groups))
	for _, g := range groups {
		ids = append(ids, g.ID)
	}
	return ids, nil
}

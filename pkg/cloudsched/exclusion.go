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
)

// The renewable lease covers the slow guard/compile/resolve path. The lease
// alone is insufficient: the run publisher must commit under the ownership
// check installed on this context, in the SAME transaction as its run write.
// An expired worker therefore cannot persist behind a successor's overlap read.
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

func (s *MemoryStore) ListIntentSchedules(_ context.Context, now time.Time, limit int) ([]string, error) {
	s.intentMu.Lock()
	defer s.intentMu.Unlock()
	seen := map[string]bool{}
	for _, r := range s.intents {
		if !r.Done && !r.NotBefore.After(now) {
			seen[r.Schedule.ID] = true
		}
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if limit > 0 && len(ids) > limit {
		ids = ids[:limit]
	}
	return ids, nil
}

func (s *MongoStore) ListIntentSchedules(ctx context.Context, now time.Time, limit int) ([]string, error) {
	var ids []string
	err := s.intentColl().Distinct(ctx, "schedule._id", bson.M{"done": false, "not_before": bson.M{"$lte": now}}).Decode(&ids)
	if err != nil {
		return nil, err
	}
	sort.Strings(ids)
	if limit > 0 && len(ids) > limit {
		ids = ids[:limit]
	}
	return ids, nil
}

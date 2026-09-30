package lease

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/internal/mongoutil"
)

// Collection is the Mongo collection holding one document per lease name:
// {_id: name, owner, expires_at, renewed_at}. The _id is the only key it is
// ever queried by, so it needs no schema beyond the default index.
const Collection = "leases"

// MongoStore is the cloud Store, shared by every replica of a deployment.
type MongoStore struct {
	coll *mongo.Collection
}

// NewMongoStore builds the Mongo-backed lease store on db.
func NewMongoStore(db *mongo.Database) *MongoStore {
	return &MongoStore{coll: db.Collection(Collection)}
}

var _ Store = (*MongoStore)(nil)

// Acquire is one conditional upsert. The filter matches the lease document
// only when owner already holds it or it expired at now; the upsert then
// writes owner and the new expiry. When the document exists but matches
// neither branch — another owner holds it unexpired — the upsert tries to
// insert a second document under the same _id and the unique index refuses
// it: that duplicate-key error IS the "held by another owner" answer.
//
// Success is read off MatchedCount/UpsertedCount, never ModifiedCount: a
// renewal stamping the same millisecond (BSON datetimes are millisecond
// precision) modifies nothing and would read as a loss. The expiry moves with
// $max, so an older stamp that lands late never shortens a lease.
func (s *MongoStore) Acquire(ctx context.Context, name, owner string, now time.Time, ttl time.Duration) (bool, error) {
	if err := validate(name, owner, ttl); err != nil {
		return false, err
	}
	name, owner, now = strings.TrimSpace(name), strings.TrimSpace(owner), stamp(now)
	res, err := s.coll.UpdateOne(ctx,
		bson.M{"_id": name, "$or": bson.A{
			bson.M{"owner": owner},
			bson.M{"expires_at": bson.M{"$lte": now}},
		}},
		bson.M{
			"$set": bson.M{"owner": owner, "renewed_at": now},
			"$max": bson.M{"expires_at": stamp(now.Add(ttl))},
		},
		options.UpdateOne().SetUpsert(true))
	if err != nil {
		if mongoutil.IsDuplicateKey(err) {
			return false, nil
		}
		return false, fmt.Errorf("lease %q: acquire for %s: %w", name, owner, err)
	}
	return res.MatchedCount > 0 || res.UpsertedCount > 0, nil
}

// Renew is Acquire without the upsert and without the takeover branch: it
// matches only the document owner holds, so it cannot re-create a lease its
// holder released, nor take one another owner holds.
func (s *MongoStore) Renew(ctx context.Context, name, owner string, now time.Time, ttl time.Duration) (bool, error) {
	if err := validate(name, owner, ttl); err != nil {
		return false, err
	}
	name, owner, now = strings.TrimSpace(name), strings.TrimSpace(owner), stamp(now)
	res, err := s.coll.UpdateOne(ctx,
		bson.M{"_id": name, "owner": owner},
		bson.M{
			"$set": bson.M{"renewed_at": now},
			"$max": bson.M{"expires_at": stamp(now.Add(ttl))},
		})
	if err != nil {
		return false, fmt.Errorf("lease %q: renew for %s: %w", name, owner, err)
	}
	return res.MatchedCount > 0, nil
}

// Release deletes the lease document when owner holds it. A delete that
// matched nothing is disambiguated with one read, because the two causes are
// different answers: a lease nobody holds is a no-op, a lease another owner
// took is ErrLost.
func (s *MongoStore) Release(ctx context.Context, name, owner string) error {
	name, owner = strings.TrimSpace(name), strings.TrimSpace(owner)
	res, err := s.coll.DeleteOne(ctx, bson.M{"_id": name, "owner": owner})
	if err != nil {
		return fmt.Errorf("lease %q: release for %s: %w", name, owner, err)
	}
	if res.DeletedCount > 0 {
		return nil
	}
	err = s.coll.FindOne(ctx, bson.M{"_id": name}).Err()
	switch {
	case errors.Is(err, mongo.ErrNoDocuments):
		return nil
	case err != nil:
		return fmt.Errorf("lease %q: release for %s: read back: %w", name, owner, err)
	default:
		return ErrLost
	}
}

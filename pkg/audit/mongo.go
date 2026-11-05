package audit

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/internal/mongoutil"
	"github.com/SocialGouv/iterion/pkg/internal/storekit"
)

const colAudit = "audit_events"

// MongoStore is the production audit log.
type MongoStore struct{ kit *storekit.Mongo[Event] }

func NewMongoStore(db *mongo.Database) *MongoStore {
	return &MongoStore{kit: storekit.NewMongo[Event](db.Collection(colAudit), ErrNotFound, "audit")}
}

// EnsureSchema creates the audit indexes idempotently.
func EnsureSchema(ctx context.Context, db *mongo.Database) error {
	col := db.Collection(colAudit)
	if _, err := col.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "created_at", Value: -1}}, Options: options.Index().SetName("tenant_recent")},
		{Keys: bson.D{{Key: "scope", Value: 1}, {Key: "created_at", Value: -1}}, Options: options.Index().SetName("scope_recent")},
		{Keys: bson.D{{Key: "action", Value: 1}, {Key: "created_at", Value: -1}}, Options: options.Index().SetName("action_recent")},
		{Keys: bson.D{{Key: "created_at", Value: 1}}, Options: options.Index().SetName("audit_ttl").SetExpireAfterSeconds(int32(RetentionDays * 24 * 60 * 60))},
	}); err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("audit: ensure indexes: %w", err)
	}
	return nil
}

func (s *MongoStore) Insert(ctx context.Context, e Event) error {
	return s.kit.Insert(ctx, e, nil, "insert")
}

func (s *MongoStore) ListByTenant(ctx context.Context, tenantID string, p Page) ([]Event, error) {
	filter := bson.M{"tenant_id": tenantID, "scope": ScopeTenant}
	return s.list(ctx, filter, p)
}

func (s *MongoStore) ListPlatform(ctx context.Context, p Page) ([]Event, error) {
	return s.list(ctx, bson.M{"scope": ScopePlatform}, p)
}

// ScrubActor unsets the identifying fields in two UpdateMany calls, each
// filtered on the rows that still carry one of the fields it unsets, so the
// count it answers is of rows changed — the memory twin's reading. The
// first takes the rows the account acted in; the second the rows it is the
// target of, or addressed by, that another actor wrote.
func (s *MongoStore) ScrubActor(ctx context.Context, userID, email string, digests []string) (int64, error) {
	if userID == "" {
		return 0, errors.New("audit: scrub actor: no user id")
	}
	carriesMeta := bson.A{}
	unsetMeta := bson.M{}
	for _, k := range ScrubbedMetaKeys {
		carriesMeta = append(carriesMeta, bson.M{"meta." + k: bson.M{"$exists": true}})
		unsetMeta["meta."+k] = ""
	}
	carriesAll := append(bson.A{
		bson.M{"ip": bson.M{"$exists": true}},
		bson.M{"user_agent": bson.M{"$exists": true}},
	}, carriesMeta...)
	unsetAll := bson.M{"ip": "", "user_agent": ""}
	for k := range unsetMeta {
		unsetAll[k] = ""
	}
	// The person's own rows: as actor, as the target of an attempt with no
	// actor (actor_id is omitted when empty), as a refusal by address.
	own := bson.A{
		bson.M{"actor_id": userID},
		bson.M{"actor_id": bson.M{"$in": bson.A{"", nil}}, "target_id": userID},
	}
	if len(digests) > 0 {
		own = append(own, bson.M{"meta.subject_hash": bson.M{"$in": digests}})
	}
	res, err := s.kit.Coll().UpdateMany(ctx,
		bson.M{"$and": bson.A{bson.M{"$or": own}, bson.M{"$or": carriesAll}}},
		bson.M{"$unset": unsetAll})
	if err != nil {
		return 0, fmt.Errorf("audit: scrub the events of user %s: %w", userID, err)
	}
	n := res.ModifiedCount
	names := bson.A{bson.M{"target_id": userID}}
	if addr := strings.ToLower(strings.TrimSpace(email)); addr != "" {
		names = append(names, bson.M{"meta.email": bson.M{
			"$regex":   `^\s*` + regexp.QuoteMeta(addr) + `\s*$`,
			"$options": "i",
		}})
	}
	res, err = s.kit.Coll().UpdateMany(ctx,
		bson.M{"actor_id": bson.M{"$ne": userID}, "$and": bson.A{
			bson.M{"$or": names},
			bson.M{"$or": carriesMeta},
		}},
		bson.M{"$unset": unsetMeta})
	if err != nil {
		return n, fmt.Errorf("audit: scrub the events naming user %s: %w", userID, err)
	}
	return n + res.ModifiedCount, nil
}

func (s *MongoStore) list(ctx context.Context, filter bson.M, p Page) ([]Event, error) {
	if p.Action != "" {
		filter["action"] = p.Action
	}
	if p.ActorID != "" {
		filter["actor_id"] = p.ActorID
	}
	created := bson.M{}
	if !p.From.IsZero() {
		created["$gte"] = p.From
	}
	if !p.To.IsZero() {
		created["$lte"] = p.To
	}
	if len(created) > 0 {
		filter["created_at"] = created
	}
	return s.kit.List(ctx, filter, "list", "decode", options.Find().
		SetSort(bson.M{"created_at": -1}).
		SetSkip(int64(p.Offset)).
		SetLimit(int64(ClampLimit(p.Limit))))
}

package trigger

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

// SubscriptionsCollectionName is the Mongo collection backing the cloud-mode
// SubscriptionStore.
const SubscriptionsCollectionName = "trigger_subscriptions"

// MongoSubscriptionStore is the cloud-mode SubscriptionStore. Its index shape
// mirrors forge.MongoRepoIntegrationStore: a {tenant_id, repo} index for the
// candidate/by-repo queries and a {tenant_id, bot_id} index for by-bot.
type MongoSubscriptionStore struct {
	coll *mongo.Collection
}

func NewMongoSubscriptionStore(db *mongo.Database) *MongoSubscriptionStore {
	return &MongoSubscriptionStore{coll: db.Collection(SubscriptionsCollectionName)}
}

func (s *MongoSubscriptionStore) EnsureSchema(ctx context.Context) error {
	_, err := s.coll.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "repo", Value: 1}}, Options: options.Index().SetName("tenant_repo")},
		{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "bot_id", Value: 1}}, Options: options.Index().SetName("tenant_bot")},
		{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "origin", Value: 1}}, Options: options.Index().SetName("tenant_origin")},
	})
	if err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("trigger: ensure subscriptions indexes: %w", err)
	}
	return nil
}

func (s *MongoSubscriptionStore) Create(ctx context.Context, sub Subscription) error {
	if _, err := s.coll.InsertOne(ctx, sub); err != nil {
		return fmt.Errorf("trigger: insert subscription: %w", err)
	}
	return nil
}

func (s *MongoSubscriptionStore) Get(ctx context.Context, id string) (Subscription, error) {
	return mongoutil.FindOne[Subscription](ctx, s.coll, bson.M{"_id": id}, ErrSubscriptionNotFound, "trigger: get subscription")
}

// subscriptionHealthFields are the marks' fields (MarkLaunchError,
// MarkRunOutcome): targeted writes that bump no version, so a full-document
// Update re-stamps them from the stored row instead of writing its copy's
// over them.
var subscriptionHealthFields = []string{
	"last_error", "last_error_at",
	"last_run_id", "last_run_status", "last_run_error", "last_run_error_code", "last_run_at",
}

// Update replaces the row as a full document, committing only against the
// version the caller's copy carries: a zero-match disambiguates — row gone
// answers ErrSubscriptionNotFound, a row still there moved under the caller
// and answers ErrSubscriptionConflict. The version-0 arm also matches a
// document written before the field existed, so a deployed row's first
// versioned write lands. The replace stamps the next version (the doc
// carries it incremented), mirroring the memory twin's CAS. The health
// stamps are not the copy's to write: the marks that set them bump no
// version — a keepalive firing every tick must not conflict every operator
// edit — so a copy read before a fire passes the CAS, and the replace keeps
// the stored stamps (absent ones stay absent), as the webhooks twin keeps
// last_used_at.
func (s *MongoSubscriptionStore) Update(ctx context.Context, sub Subscription) error {
	filter := storekit.VersionCAS(sub.ID, sub.Version)
	sub.Version++
	raw, err := bson.Marshal(sub)
	if err != nil {
		return fmt.Errorf("trigger: update subscription: %w", err)
	}
	var doc bson.M
	if err := bson.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("trigger: update subscription: %w", err)
	}
	stored := bson.M{}
	for _, field := range subscriptionHealthFields {
		delete(doc, field)
		stored[field] = "$" + field
	}
	pipeline := mongo.Pipeline{bson.D{{Key: "$replaceWith", Value: bson.M{"$mergeObjects": bson.A{stored, bson.M{"$literal": doc}}}}}}
	res, err := s.coll.UpdateOne(ctx, filter, pipeline)
	if err != nil {
		return fmt.Errorf("trigger: update subscription: %w", err)
	}
	if res.MatchedCount == 0 {
		if _, getErr := s.Get(ctx, sub.ID); getErr != nil {
			return getErr
		}
		return ErrSubscriptionConflict
	}
	return nil
}

func (s *MongoSubscriptionStore) MarkLaunchError(ctx context.Context, id, lastError string, at time.Time) error {
	update := bson.M{"$set": bson.M{"last_error": lastError, "last_error_at": at.UTC()}}
	if lastError == "" {
		update = bson.M{"$unset": bson.M{"last_error": "", "last_error_at": ""}}
	}
	res, err := s.coll.UpdateOne(ctx, bson.M{"_id": id}, update)
	if err != nil {
		return fmt.Errorf("trigger: mark subscription launch error: %w", err)
	}
	if res.MatchedCount == 0 {
		return ErrSubscriptionNotFound
	}
	return nil
}

// MarkRunOutcome stamps the last run's terminal outcome on the subscription
// — same targeted-field discipline as MarkLaunchError, because the writer
// (the run-outcome spine) holds no copy of the row.
//
// The write carries a MONOTONICITY GUARD: it matches only while the row's
// last_run_at is absent or not newer than `at`, so an out-of-order delivery
// cannot move the health fields backwards. A zero-match is ambiguous — "row
// gone" vs "stale outcome" — and is disambiguated with one Get: the former
// is ErrSubscriptionNotFound, the latter a silent no-op.
func (s *MongoSubscriptionStore) MarkRunOutcome(ctx context.Context, id, runID, status, errMsg, errCode string, at time.Time) error {
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
	res, err := s.coll.UpdateOne(ctx, filter, update)
	if err != nil {
		return fmt.Errorf("trigger: mark subscription run outcome: %w", err)
	}
	if res.MatchedCount == 0 {
		if _, getErr := s.Get(ctx, id); getErr != nil {
			if errors.Is(getErr, ErrSubscriptionNotFound) {
				return ErrSubscriptionNotFound
			}
			// A transient store failure must not be laundered into
			// ErrSubscriptionNotFound — the caller warns on real errors and
			// stays silent only on a purged row.
			return fmt.Errorf("trigger: mark subscription run outcome: disambiguate zero-match: %w", getErr)
		}
		// The row exists but holds a NEWER outcome: a stale delivery. Not
		// an error — the record already says something truer.
	}
	return nil
}

func (s *MongoSubscriptionStore) Delete(ctx context.Context, id string) error {
	return mongoutil.DeleteOneChecked(ctx, s.coll, bson.M{"_id": id}, ErrSubscriptionNotFound, "trigger: delete subscription")
}

// ClearCredentialPins removes every pin entry whose value names recordID —
// the Mongo twin of the memory store's value-scoped drop, run by the clear
// both pin-bearing stores share (storekit.MongoPinClear): judged against the
// row as the store holds it at write time, one value-guarded $unset per
// entry with a version bump, and a version-CAS rewrite for a name no path
// can address. No update lane resurrects a swept pin — a full-document
// editor holding a pre-sweep snapshot conflicts on the bumped version — and
// the row born mid-delete is the write routes' landed-pin reconciliation's
// to sweep.
func (s *MongoSubscriptionStore) ClearCredentialPins(ctx context.Context, id, recordID string) (bool, error) {
	return storekit.MongoPinClear[Subscription]{
		Coll:    s.coll,
		Get:     s.Get,
		Version: func(sub Subscription) int64 { return sub.Version },
		Fields:  subscriptionPinFields,
		Op:      "trigger: clear subscription credential pins",
	}.Clear(ctx, id, recordID)
}

// subscriptionPinFields are the subscription's two credential-pin maps.
var subscriptionPinFields = []storekit.PinField[Subscription]{
	{Path: "key_overrides", Of: func(sub Subscription) map[string]string { return sub.KeyOverrides }},
	{Path: "secret_overrides", Of: func(sub Subscription) map[string]string { return sub.SecretOverrides }},
}

func (s *MongoSubscriptionStore) ListByTenant(ctx context.Context, tenantID string) ([]Subscription, error) {
	return s.find(ctx, bson.M{"tenant_id": tenantID})
}

func (s *MongoSubscriptionStore) ListByRepo(ctx context.Context, tenantID, repo string) ([]Subscription, error) {
	// Repo's omitempty tag stores workspace-wide subscriptions without the field.
	return s.find(ctx, bson.M{"tenant_id": tenantID, "repo": bson.M{"$in": bson.A{repo, "", nil}}})
}

func (s *MongoSubscriptionStore) ListByBot(ctx context.Context, tenantID, botID string) ([]Subscription, error) {
	return s.find(ctx, bson.M{"tenant_id": tenantID, "bot_id": botID})
}

func (s *MongoSubscriptionStore) ListByOrigin(ctx context.Context, tenantID, origin string) ([]Subscription, error) {
	return s.find(ctx, bson.M{"tenant_id": tenantID, "origin": origin})
}

func (s *MongoSubscriptionStore) ListCandidates(ctx context.Context, ev Event) ([]Subscription, error) {
	return s.find(ctx, bson.M{
		"tenant_id": ev.TenantID,
		"enabled":   true,
		"repo":      bson.M{"$in": bson.A{ev.Repo, "", nil}},
	})
}

// DistinctBoardTenants returns the tenants holding at least one ENABLED
// board-kind subscription — the set the cloud board source poll-tails.
// Tenants without board triggers cost nothing.
func (s *MongoSubscriptionStore) DistinctBoardTenants(ctx context.Context) ([]string, error) {
	res := s.coll.Distinct(ctx, "tenant_id", bson.M{"invocation": "board", "enabled": true})
	var tenants []string
	if err := res.Decode(&tenants); err != nil {
		return nil, fmt.Errorf("trigger: distinct board tenants: %w", err)
	}
	return tenants, nil
}

func (s *MongoSubscriptionStore) find(ctx context.Context, filter bson.M) ([]Subscription, error) {
	return mongoutil.FindAllSorted[Subscription](ctx, s.coll, filter, "created_at",
		"trigger: list subscriptions", "trigger: decode subscriptions")
}

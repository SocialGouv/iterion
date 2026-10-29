package webhooks

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

// Collection names.
const (
	colConfigs    = "webhook_configs"
	colDeliveries = "webhook_deliveries"
	colQuotas     = "webhook_quotas"
	colDeferred   = "webhook_deferred_launches"
)

// DeliveryTTLDays caps how long delivery audit rows are retained.
const DeliveryTTLDays = 90

// MongoStores bundles the three Mongo-backed stores over one database
// (reuse via the cloud store's DB() accessor). Each sub-store satisfies
// one interface; they are split because ConfigStore + DeliveryStore both
// declare an Update method.
type MongoStores struct {
	Configs    *MongoConfigStore
	Deliveries *MongoDeliveryStore
	Counter    *MongoCounter
	Deferred   *MongoDeferredLaunchStore
}

func NewMongoStores(db *mongo.Database) *MongoStores {
	return &MongoStores{
		Configs:    &MongoConfigStore{kit: storekit.NewMongo[Config](db.Collection(colConfigs), ErrNotFound, "webhooks")},
		Deliveries: &MongoDeliveryStore{kit: storekit.NewMongo[Delivery](db.Collection(colDeliveries), ErrNotFound, "webhooks")},
		Counter:    &MongoCounter{col: db.Collection(colQuotas)},
		Deferred:   &MongoDeferredLaunchStore{col: db.Collection(colDeferred)},
	}
}

// EnsureSchema creates every webhook index idempotently.
func EnsureSchema(ctx context.Context, db *mongo.Database) error {
	configs := db.Collection(colConfigs)
	if _, err := configs.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "token_hash", Value: 1}}, Options: options.Index().SetUnique(true).SetName("token_hash_unique")},
		{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "created_at", Value: -1}}, Options: options.Index().SetName("tenant_created")},
	}); err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("webhooks: ensure config indexes: %w", err)
	}
	deliveries := db.Collection(colDeliveries)
	if _, err := deliveries.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "idempotency_key", Value: 1}}, Options: options.Index().SetUnique(true).SetName("idempotency_unique")},
		{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "webhook_id", Value: 1}, {Key: "received_at", Value: -1}}, Options: options.Index().SetName("tenant_webhook_recent")},
		{Keys: bson.D{{Key: "status", Value: 1}, {Key: "next_attempt_at", Value: 1}, {Key: "_id", Value: 1}}, Options: options.Index().SetName("delivery_retry_due")},
		{Keys: bson.D{{Key: "received_at", Value: 1}}, Options: options.Index().SetName("deliveries_ttl").SetExpireAfterSeconds(int32(DeliveryTTLDays * 24 * 60 * 60))},
	}); err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("webhooks: ensure delivery indexes: %w", err)
	}
	deferred := db.Collection(colDeferred)
	if _, err := deferred.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "fire_at", Value: 1}}, Options: options.Index().SetName("deferred_fire_at")},
		// Backstop TTL: a row nothing can launch any more (webhook config
		// deleted between defer and fire) must not sit forever. The sweep
		// deletes launched rows itself; this only catches orphans.
		{Keys: bson.D{{Key: "created_at", Value: 1}}, Options: options.Index().SetName("deferred_ttl").SetExpireAfterSeconds(int32(7 * 24 * 60 * 60))},
	}); err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("webhooks: ensure deferred-launch indexes: %w", err)
	}
	return nil
}

// ---- MongoConfigStore ----

type MongoConfigStore struct{ kit *storekit.Mongo[Config] }

func (s *MongoConfigStore) Create(ctx context.Context, c Config) error {
	return s.kit.Insert(ctx, c, ErrDuplicate, "insert config")
}

func (s *MongoConfigStore) Get(ctx context.Context, id string) (Config, error) {
	return s.kit.GetByID(ctx, id, "get config")
}

func (s *MongoConfigStore) Update(ctx context.Context, c Config) error {
	filter := bson.M{"_id": c.ID, "version": c.Version, "provision_frozen": bson.M{"$ne": true}}
	if c.Version == 0 {
		delete(filter, "version")
		filter["$or"] = bson.A{bson.M{"version": 0}, bson.M{"version": bson.M{"$exists": false}}}
	}
	c.Version++
	raw, err := bson.Marshal(c)
	if err != nil {
		return err
	}
	var doc bson.M
	if err = bson.Unmarshal(raw, &doc); err != nil {
		return err
	}
	delete(doc, "last_used_at")
	// A request snapshot cannot erase a delivery's independently stamped usage.
	pipeline := mongo.Pipeline{bson.D{{Key: "$replaceWith", Value: bson.M{"$mergeObjects": bson.A{bson.M{"last_used_at": "$last_used_at"}, bson.M{"$literal": doc}}}}}}
	r, err := s.kit.Coll().UpdateOne(ctx, filter, pipeline)
	if mongo.IsDuplicateKeyError(err) {
		return ErrDuplicate
	}
	if err != nil {
		return fmt.Errorf("webhooks: update config: %w", err)
	}
	if r.MatchedCount == 0 {
		if _, err := s.Get(ctx, c.ID); err != nil {
			return err
		}
		return ErrConflict
	}
	return nil
}

func (s *MongoConfigStore) DisableProvision(ctx context.Context, id, integrationID string) error {
	r, err := s.kit.Coll().UpdateOne(ctx,
		bson.M{"_id": id, "provision_integration_id": integrationID},
		bson.M{"$set": bson.M{"enabled": false}, "$inc": bson.M{"version": 1}})
	if err != nil {
		return fmt.Errorf("webhooks: disable provisioned config: %w", err)
	}
	if r.MatchedCount == 0 {
		if _, err := s.Get(ctx, id); err != nil {
			return err
		}
		return ErrConflict
	}
	return nil
}

func (s *MongoConfigStore) Delete(ctx context.Context, id string) error {
	return s.kit.Delete(ctx, id, "delete config")
}

func (s *MongoConfigStore) ListByTenant(ctx context.Context, tenantID string) ([]Config, error) {
	return s.kit.List(ctx, bson.M{"tenant_id": tenantID}, "list configs", "decode configs",
		options.Find().SetSort(bson.M{"created_at": 1}))
}

func (s *MongoConfigStore) MarkUsed(ctx context.Context, id string, t time.Time) error {
	return s.kit.SetAny(ctx, id, bson.M{"last_used_at": t}, "mark used")
}

// ---- MongoDeliveryStore ----

type MongoDeliveryStore struct{ kit *storekit.Mongo[Delivery] }

func (s *MongoDeliveryStore) Get(ctx context.Context, id string) (Delivery, error) {
	return s.kit.GetByID(ctx, id, "get delivery")
}

func (s *MongoDeliveryStore) ListRetryDue(ctx context.Context, now time.Time, limit int) ([]Delivery, error) {
	if limit <= 0 {
		return nil, nil
	}
	return s.kit.List(ctx, bson.M{
		"status": StatusLaunchError, "run_id": bson.M{"$in": bson.A{"", nil}},
		"retry_target": bson.M{"$ne": nil}, "next_attempt_at": bson.M{"$lte": now, "$ne": nil},
	}, "list retry deliveries", "decode retry deliveries",
		options.Find().SetSort(bson.D{{Key: "next_attempt_at", Value: 1}, {Key: "_id", Value: 1}}).SetLimit(int64(limit)))
}

func (s *MongoDeliveryStore) RetryObsolete(ctx context.Context, d Delivery, supersede bool) (bool, error) {
	if d.SubjectID == "" {
		return false, nil
	}
	parent := d.ParentSubjectID
	if parent == "" {
		parent = d.SubjectID
	}
	outcomes := bson.A{bson.M{"closes_subject": true, "subject_id": parent}}
	if supersede {
		outcomes = append(outcomes, bson.M{"bot_id": d.BotID, "subject_id": d.SubjectID, "$or": bson.A{
			bson.M{"run_id": bson.M{"$nin": bson.A{nil, ""}}}, bson.M{"retry_target": bson.M{"$ne": nil}},
		}})
	}
	newer := bson.M{"received_at": bson.M{"$gte": d.ReceivedAt.Truncate(time.Millisecond)}}
	if d.ReceiptOrder != 0 {
		newer = bson.M{"$or": bson.A{
			bson.M{"receipt_order": bson.M{"$gt": d.ReceiptOrder}},
			bson.M{"receipt_order": bson.M{"$in": bson.A{nil, 0}}, "received_at": bson.M{"$gte": d.ReceivedAt.Truncate(time.Millisecond)}},
		}}
	}
	_, err := s.kit.FindOne(ctx, bson.M{
		"_id": bson.M{"$ne": d.ID}, "tenant_id": d.TenantID, "webhook_id": d.WebhookID,
		"project_path": d.ProjectPath, "$and": bson.A{newer, bson.M{"$or": outcomes}},
	}, "check retry subject")
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

func (s *MongoDeliveryStore) Insert(ctx context.Context, d Delivery) error {
	return s.kit.Insert(ctx, d, ErrDuplicate, "insert delivery")
}

func (s *MongoDeliveryStore) GetByIdempotencyKey(ctx context.Context, key string) (Delivery, error) {
	return s.kit.FindOne(ctx, bson.M{"idempotency_key": key}, "get delivery")
}

func (s *MongoDeliveryStore) Update(ctx context.Context, d Delivery) error {
	return s.kit.Replace(ctx, d.ID, d, nil, "update delivery")
}

// ClaimFailedRetry is the compare-and-set take-over of a failed row (see
// DeliveryStore). Keep its semantics in lock-step with the memory twin.
func (s *MongoDeliveryStore) ClaimFailedRetry(ctx context.Context, d Delivery, expectAttempts int) (bool, error) {
	// attempts is omitempty, so a row that never counted one carries NO
	// field — and {attempts: 0} does not match a missing field. Match both
	// spellings of "none yet", or a first retry could never be claimed.
	attempts := bson.M{"$eq": expectAttempts}
	if expectAttempts == 0 {
		attempts = bson.M{"$in": bson.A{0, nil}}
	}
	res, err := s.kit.Coll().ReplaceOne(ctx, bson.M{
		"_id": d.ID, "status": StatusLaunchError, "attempts": attempts, "run_id": bson.M{"$in": bson.A{"", nil}},
	}, d)
	if err != nil {
		return false, fmt.Errorf("webhooks: claim failed delivery %s: %w", d.ID, err)
	}
	return res.MatchedCount > 0, nil
}

// MarkRunOutcome stamps the outcome of the run this delivery launched (see
// DeliveryStore). Keep its semantics in lock-step with the memory twin.
//
// One conditional update carries both guards: `run_id` must still name the
// run being described, and `last_run_at` must be absent or not newer than
// `at`. A zero-match is therefore ambiguous — row gone, row relaunched under
// another run, or stale outcome — and is disambiguated with one Get: the
// first is ErrNotFound, the other two are a silent no-op.
func (s *MongoDeliveryStore) MarkRunOutcome(ctx context.Context, id, runID, status, errMsg, errCode string, at time.Time) error {
	if runID == "" {
		// Nothing to match: a row whose run_id is empty launched no run, so
		// there is no outcome of ITS run to stamp. Refused here rather than
		// trusting the filter — `run_id: ""` would also match a row that
		// never launched.
		return nil
	}
	stamped := at.UTC()
	set := bson.M{"last_run_status": status, "last_run_at": stamped}
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
	res, err := s.kit.Coll().UpdateOne(ctx, bson.M{
		"_id": id, "run_id": runID,
		"$or": bson.A{
			bson.M{"last_run_at": bson.M{"$exists": false}},
			bson.M{"last_run_at": bson.M{"$lte": stamped}},
		},
	}, update)
	if err != nil {
		return fmt.Errorf("webhooks: mark delivery run outcome %s: %w", id, err)
	}
	if res.MatchedCount == 0 {
		if _, getErr := s.Get(ctx, id); getErr != nil {
			if errors.Is(getErr, ErrNotFound) {
				return ErrNotFound
			}
			// A transient store failure must not be laundered into
			// ErrNotFound — the spine warns on real errors and stays silent
			// only on a purged row.
			return fmt.Errorf("webhooks: mark delivery run outcome %s: disambiguate zero-match: %w", id, getErr)
		}
		// The row exists but reports another run, or already holds a NEWER
		// outcome. Not an error — it already says something truer.
	}
	return nil
}

func (s *MongoDeliveryStore) ListByWebhook(ctx context.Context, tenantID, webhookID string, limit int) ([]Delivery, error) {
	if limit <= 0 {
		limit = 50
	}
	return s.kit.List(ctx,
		bson.M{"tenant_id": tenantID, "webhook_id": webhookID},
		"list deliveries", "decode deliveries",
		options.Find().SetSort(bson.M{"received_at": -1}).SetLimit(int64(limit)))
}

func (s *MongoDeliveryStore) CountLaunched(ctx context.Context, tenantID, webhookID, eventKind, projectPath, subjectID string) (int, error) {
	n, err := s.kit.Coll().CountDocuments(ctx, bson.M{
		"tenant_id":    tenantID,
		"webhook_id":   webhookID,
		"event_kind":   eventKind,
		"project_path": projectPath,
		"subject_id":   subjectID,
		"run_id":       bson.M{"$nin": bson.A{nil, ""}},
	})
	if err != nil {
		return 0, fmt.Errorf("webhooks: count launched deliveries: %w", err)
	}
	return int(n), nil
}

// ListLaunchedBySubject returns the subject's launched deliveries, newest
// first. Unbounded by design (see the interface): one pull request's own
// launches are few, and the point is to miss none.
func (s *MongoDeliveryStore) ListLaunchedBySubject(ctx context.Context, tenantID, webhookID, projectPath, subjectID string) ([]Delivery, error) {
	return s.kit.List(ctx, bson.M{
		"tenant_id":    tenantID,
		"webhook_id":   webhookID,
		"project_path": projectPath,
		"run_id":       bson.M{"$nin": bson.A{nil, ""}},
		"$or": bson.A{
			bson.M{"subject_id": subjectID},
			bson.M{"parent_subject_id": subjectID},
		},
	}, "list launched deliveries by subject", "decode launched deliveries",
		options.Find().SetSort(bson.M{"received_at": -1}))
}

// ---- MongoDeferredLaunchStore ----

// MongoDeferredLaunchStore keeps semantics in lock-step with
// MemoryDeferredLaunchStore: _id IS the subject key, so the debounce
// upsert is one atomic FindOneAndUpdate and the multi-replica claim is
// a per-row CAS on the lease.
type MongoDeferredLaunchStore struct{ col *mongo.Collection }

// upsertCASRounds bounds the update-then-insert retry below. Two
// replicas racing to park the first payload of a subject resolve in one
// extra round; three is slack, not a bound anyone should reach.
const upsertCASRounds = 3

func (s *MongoDeferredLaunchStore) Upsert(ctx context.Context, d DeferredLaunch) (bool, error) {
	set := bson.M{
		"tenant_id":     d.TenantID,
		"webhook_id":    d.WebhookID,
		"fire_at":       d.FireAt,
		"event_kind":    d.EventKind,
		"event_action":  d.EventAction,
		"project_path":  d.ProjectPath,
		"subject_id":    d.SubjectID,
		"subject_url":   d.SubjectURL,
		"subject_sha":   d.SubjectSHA,
		"sender_handle": d.SenderHandle,
		"payload_hash":  d.PayloadHash,
		"source_ip":     d.SourceIP,
		"public_base":   d.PublicBase,
		"targets":       d.Targets,
		"order_key":     d.OrderKey,
		// A fresh push is a fresh payload: it re-arms even a subject
		// mid-claim, and it gets the full retry budget back (the handler
		// builds d with Attempts zero).
		"attempts":      d.Attempts,
		"claimed_until": time.Time{},
	}
	// Update-then-insert CAS, NOT one upsert carrying the staleness
	// predicate in its filter: an upsert whose filter misses the existing
	// (newer) row makes Mongo attempt an INSERT on the same _id and
	// return a duplicate-key ERROR — indistinguishable from a store
	// outage to the caller, whose outage path launches immediately, i.e.
	// launches exactly the stale payload the predicate just refused.
	// Here the duplicate key is the ANSWER, not an error: it proves a row
	// exists, so the next round's update evaluates the predicate against
	// it and either wins or reports the payload stale.
	for round := 0; round < upsertCASRounds; round++ {
		filter := bson.M{"_id": d.SubjectKey}
		if d.OrderKey != "" {
			// Mirrors DeferredPayloadIsStale: only a stored key that is
			// present AND strictly greater refuses the write.
			filter["$or"] = bson.A{
				bson.M{"order_key": bson.M{"$exists": false}},
				bson.M{"order_key": ""},
				bson.M{"order_key": bson.M{"$lte": d.OrderKey}},
			}
		}
		res, err := s.col.UpdateOne(ctx, filter,
			bson.M{"$set": set, "$inc": bson.M{"generation": 1}})
		if err != nil {
			return false, fmt.Errorf("webhooks: upsert deferred launch: %w", err)
		}
		if res.MatchedCount > 0 {
			return true, nil
		}
		// Nothing matched: either no row for this subject, or the parked
		// one is strictly newer. The insert tells us which.
		ins := d
		ins.Generation = 1
		ins.ClaimedUntil = time.Time{}
		if _, err := s.col.InsertOne(ctx, ins); err == nil {
			return true, nil
		} else if !mongo.IsDuplicateKeyError(err) {
			return false, fmt.Errorf("webhooks: upsert deferred launch: %w", err)
		}
	}
	return false, nil // a strictly newer payload holds the row
}

func (s *MongoDeferredLaunchStore) ClaimDue(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]DeferredLaunch, error) {
	if limit <= 0 {
		limit = 50
	}
	var out []DeferredLaunch
	for len(out) < limit {
		var d DeferredLaunch
		err := s.col.FindOneAndUpdate(ctx,
			bson.M{
				"fire_at": bson.M{"$lte": now},
				"$or": bson.A{
					bson.M{"claimed_until": bson.M{"$exists": false}},
					bson.M{"claimed_until": bson.M{"$lte": now}},
				},
			},
			bson.M{"$set": bson.M{"claimed_until": now.Add(lease)}},
			options.FindOneAndUpdate().SetReturnDocument(options.After).
				SetSort(bson.M{"fire_at": 1}),
		).Decode(&d)
		if errors.Is(err, mongo.ErrNoDocuments) {
			break
		}
		if err != nil {
			return out, fmt.Errorf("webhooks: claim deferred launches: %w", err)
		}
		out = append(out, d)
	}
	return out, nil
}

func (s *MongoDeferredLaunchStore) Reschedule(ctx context.Context, subjectKey string, generation int64, fireAt time.Time, attempts int) error {
	// Generation-guarded and deliberately NOT an upsert: a row a fresh
	// push replaced (higher generation) or a closed PR purged must stay
	// replaced/purged — a stale re-arm must never resurrect it.
	if _, err := s.col.UpdateOne(ctx,
		bson.M{"_id": subjectKey, "generation": generation},
		bson.M{"$set": bson.M{"fire_at": fireAt, "attempts": attempts, "claimed_until": time.Time{}}},
	); err != nil {
		return fmt.Errorf("webhooks: reschedule deferred launch: %w", err)
	}
	return nil
}

func (s *MongoDeferredLaunchStore) Delete(ctx context.Context, subjectKey string, generation int64) error {
	if _, err := s.col.DeleteOne(ctx, bson.M{"_id": subjectKey, "generation": generation}); err != nil {
		return fmt.Errorf("webhooks: delete deferred launch: %w", err)
	}
	return nil
}

func (s *MongoDeferredLaunchStore) DeleteBySubject(ctx context.Context, subjectKey string) error {
	if _, err := s.col.DeleteOne(ctx, bson.M{"_id": subjectKey}); err != nil {
		return fmt.Errorf("webhooks: purge deferred launch: %w", err)
	}
	return nil
}

// ---- MongoCounter ----

type MongoCounter struct{ col *mongo.Collection }

// Allow increments the org (and optional per-webhook) monthly counters
// and rolls back + denies when a cap is breached. Counters are
// eventually consistent under heavy concurrency (a denied call rolls
// back its increment); the allow/deny decision is atomic per
// findOneAndUpdate, which is the property a monthly call cap needs.
func (s *MongoCounter) Allow(ctx context.Context, tenantID, webhookID string, when time.Time, lim Limits) (bool, error) {
	m := monthKey(when)
	orgKey := "org|" + tenantID + "|" + m
	if ok, err := s.bump(ctx, orgKey, lim.PerOrgMonthly); err != nil || !ok {
		return false, err
	}
	if lim.PerWebhookMonthly > 0 {
		whKey := "wh|" + tenantID + "|" + webhookID + "|" + m
		ok, err := s.bump(ctx, whKey, lim.PerWebhookMonthly)
		if err != nil || !ok {
			// Detached ctx so a cancelled request still rolls back the org
			// increment; otherwise the per-org monthly count leaks a unit.
			_, _ = s.col.UpdateOne(context.WithoutCancel(ctx), bson.M{"_id": orgKey}, bson.M{"$inc": bson.M{"count": -1}})
			return false, err
		}
	}
	return true, nil
}

func (s *MongoCounter) bump(ctx context.Context, key string, limit int) (bool, error) {
	var doc struct {
		Count int `bson:"count"`
	}
	err := s.col.FindOneAndUpdate(ctx,
		bson.M{"_id": key},
		bson.M{"$inc": bson.M{"count": 1}},
		options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.After),
	).Decode(&doc)
	if err != nil {
		return false, fmt.Errorf("webhooks: counter bump: %w", err)
	}
	if limit > 0 && doc.Count > limit {
		_, _ = s.col.UpdateOne(ctx, bson.M{"_id": key}, bson.M{"$inc": bson.M{"count": -1}})
		return false, nil
	}
	return true, nil
}

func (s *MongoCounter) OrgCount(ctx context.Context, tenantID string, when time.Time) (int, error) {
	var doc struct {
		Count int `bson:"count"`
	}
	err := s.col.FindOne(ctx, bson.M{"_id": "org|" + tenantID + "|" + monthKey(when)}).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("webhooks: org count: %w", err)
	}
	return doc.Count, nil
}

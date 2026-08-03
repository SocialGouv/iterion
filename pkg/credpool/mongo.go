package credpool

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/internal/mongoutil"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// Collection names.
const (
	colPools   = "cred_pools"
	colPledges = "cred_pledges"
	colLeases  = "cred_leases"
	colLedger  = "cred_pool_usage"
)

// EnsureSchema creates every index this package needs, idempotently.
func EnsureSchema(ctx context.Context, db *mongo.Database) error {
	specs := []struct {
		col   string
		model []mongo.IndexModel
	}{
		{colPools, []mongo.IndexModel{
			{Keys: bson.D{{Key: "org_id", Value: 1}}, Options: options.Index().SetName("pool_org")},
		}},
		{colPledges, []mongo.IndexModel{
			{Keys: bson.D{{Key: "pool_id", Value: 1}}, Options: options.Index().SetName("pledge_pool")},
			{Keys: bson.D{{Key: "user_id", Value: 1}}, Options: options.Index().SetName("pledge_user")},
		}},
		{colLeases, []mongo.IndexModel{
			// The concurrency + commitment reading: live leases of one pledge.
			{Keys: bson.D{{Key: "pledge_id", Value: 1}, {Key: "closed", Value: 1}, {Key: "expires_at", Value: 1}},
				Options: options.Index().SetName("lease_live")},
			// "which donor is serving this run right now", newest first.
			{Keys: bson.D{{Key: "run_id", Value: 1}, {Key: "closed", Value: 1}, {Key: "acquired_at", Value: -1}},
				Options: options.Index().SetName("lease_run")},
			// The sweeper's scan.
			{Keys: bson.D{{Key: "closed", Value: 1}, {Key: "expires_at", Value: 1}},
				Options: options.Index().SetName("lease_expiry")},
			// The donor's history view.
			{Keys: bson.D{{Key: "donor_id", Value: 1}, {Key: "acquired_at", Value: -1}},
				Options: options.Index().SetName("lease_donor")},
			// "has this pledge already admitted this run" (the resume test).
			{Keys: bson.D{{Key: "run_id", Value: 1}, {Key: "pledge_id", Value: 1}},
				Options: options.Index().SetName("lease_run_pledge")},
			// Retention. Anchored on acquired_at so a lease is evicted a
			// fixed time after it was granted whether or not it ever
			// closed — an abandoned lease must not become immortal.
			{Keys: bson.D{{Key: "acquired_at", Value: 1}},
				Options: options.Index().SetName("lease_ttl").SetExpireAfterSeconds(int32(LeaseRetention / time.Second))},
		}},
		{colLedger, []mongo.IndexModel{
			{Keys: bson.D{{Key: "period_start", Value: 1}},
				Options: options.Index().SetName("ledger_ttl").SetExpireAfterSeconds(int32(LedgerRetentionDays * 24 * 60 * 60))},
		}},
	}
	for _, s := range specs {
		if _, err := db.Collection(s.col).Indexes().CreateMany(ctx, s.model); err != nil && !mongoutil.IsIndexConflict(err) {
			return fmt.Errorf("credpool: ensure %s indexes: %w", s.col, err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Pools
// ---------------------------------------------------------------------------

type MongoPoolStore struct{ col *mongo.Collection }

func NewMongoPoolStore(db *mongo.Database) *MongoPoolStore {
	return &MongoPoolStore{col: db.Collection(colPools)}
}

func (s *MongoPoolStore) GetByOrg(ctx context.Context, orgID string) (Pool, error) {
	return mongoutil.FindOne[Pool](ctx, s.col, bson.M{"org_id": orgID}, ErrNotFound, "credpool: get pool by org")
}

func (s *MongoPoolStore) ListEnabled(ctx context.Context) ([]Pool, error) {
	return mongoutil.FindAllSorted[Pool](ctx, s.col, bson.M{"enabled": true}, "_id",
		"credpool: list enabled pools", "credpool: decode pool")
}

func (s *MongoPoolStore) Upsert(ctx context.Context, p Pool) error {
	now := time.Now().UTC()
	p.UpdatedAt = now
	if p.CreatedAt.IsZero() {
		p.CreatedAt = now
	}
	set, err := mongoutil.SetBodyWithoutID(p, "credpool: pool")
	if err != nil {
		return err
	}
	delete(set, "created_at") // never rewrite the original creation instant
	_, err = s.col.UpdateOne(ctx, bson.M{"_id": p.ID}, bson.M{
		"$set":         set,
		"$setOnInsert": bson.M{"_id": p.ID, "created_at": p.CreatedAt},
	}, options.UpdateOne().SetUpsert(true))
	if err != nil {
		return fmt.Errorf("credpool: upsert pool: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Pledges
// ---------------------------------------------------------------------------

type MongoPledgeStore struct{ col *mongo.Collection }

func NewMongoPledgeStore(db *mongo.Database) *MongoPledgeStore {
	return &MongoPledgeStore{col: db.Collection(colPledges)}
}

func (s *MongoPledgeStore) Get(ctx context.Context, id string) (Pledge, error) {
	return mongoutil.FindOne[Pledge](ctx, s.col, bson.M{"_id": id}, ErrNotFound, "credpool: get pledge")
}

func (s *MongoPledgeStore) ListByPool(ctx context.Context, poolID string) ([]Pledge, error) {
	return mongoutil.FindAllSorted[Pledge](ctx, s.col, bson.M{"pool_id": poolID}, "_id",
		"credpool: list pledges", "credpool: decode pledge")
}

func (s *MongoPledgeStore) ListByUser(ctx context.Context, userID string) ([]Pledge, error) {
	return mongoutil.FindAllSorted[Pledge](ctx, s.col, bson.M{"user_id": userID}, "_id",
		"credpool: list user pledges", "credpool: decode pledge")
}

func (s *MongoPledgeStore) Upsert(ctx context.Context, p Pledge) error {
	if p.ID == "" {
		p.ID = PledgeID(p.UserID, p.Kind)
	}
	now := time.Now().UTC()
	p.UpdatedAt = now
	if p.CreatedAt.IsZero() {
		p.CreatedAt = now
	}
	set, err := mongoutil.SetBodyWithoutID(p, "credpool: pledge")
	if err != nil {
		return err
	}
	delete(set, "created_at")
	// A nil cooldown/window must actually clear a previously-set value —
	// omitempty drops the key from the marshalled body, so $unset it.
	unset := bson.M{}
	if p.CooldownUntil == nil {
		unset["cooldown_until"] = ""
	}
	if p.Window == nil {
		unset["window"] = ""
	}
	update := bson.M{
		"$set":         set,
		"$setOnInsert": bson.M{"_id": p.ID, "created_at": p.CreatedAt},
	}
	if len(unset) > 0 {
		update["$unset"] = unset
	}
	if _, err := s.col.UpdateOne(ctx, bson.M{"_id": p.ID}, update, options.UpdateOne().SetUpsert(true)); err != nil {
		return fmt.Errorf("credpool: upsert pledge: %w", err)
	}
	return nil
}

func (s *MongoPledgeStore) TouchLastServed(ctx context.Context, id string, when time.Time) error {
	t := when.UTC()
	return mongoutil.UpdateOneChecked(ctx, s.col, bson.M{"_id": id},
		bson.M{"$set": bson.M{"last_served_at": t, "updated_at": t}},
		ErrNotFound, "credpool: touch pledge")
}

func (s *MongoPledgeStore) Delete(ctx context.Context, id string) error {
	return mongoutil.DeleteOneChecked(ctx, s.col, bson.M{"_id": id}, ErrNotFound, "credpool: delete pledge")
}

// ---------------------------------------------------------------------------
// Leases
// ---------------------------------------------------------------------------

type MongoLeaseStore struct{ col *mongo.Collection }

func NewMongoLeaseStore(db *mongo.Database) *MongoLeaseStore {
	return &MongoLeaseStore{col: db.Collection(colLeases)}
}

func (s *MongoLeaseStore) Put(ctx context.Context, l Lease) error {
	if l.ID == "" {
		return fmt.Errorf("credpool: lease without an id")
	}
	// Insert: every attempt gets its own record, so a finished attempt's
	// cost and outcome — the donor's evidence for the charge on their
	// ledger — is never overwritten by a later one.
	if _, err := s.col.InsertOne(ctx, l); err != nil {
		return fmt.Errorf("credpool: put lease: %w", err)
	}
	return nil
}

func (s *MongoLeaseStore) Get(ctx context.Context, leaseID string) (Lease, error) {
	return mongoutil.FindOne[Lease](ctx, s.col, bson.M{"_id": leaseID}, ErrNotFound, "credpool: get lease")
}

func (s *MongoLeaseStore) GetOpenByRun(ctx context.Context, runID string) (Lease, error) {
	open, err := s.ListOpenByRun(ctx, runID)
	if err != nil {
		return Lease{}, err
	}
	if len(open) == 0 {
		return Lease{}, ErrNotFound
	}
	return open[0], nil // newest first
}

func (s *MongoLeaseStore) ListOpenByRun(ctx context.Context, runID string) ([]Lease, error) {
	// Newest first: an unsorted FindOne would pick arbitrarily, and that
	// choice decides which donor gets charged.
	cur, err := s.col.Find(ctx, bson.M{"run_id": runID, "closed": false},
		options.Find().SetSort(bson.D{{Key: "acquired_at", Value: -1}}))
	if err != nil {
		return nil, fmt.Errorf("credpool: list open leases: %w", err)
	}
	defer cur.Close(ctx)
	var out []Lease
	if err := cur.All(ctx, &out); err != nil {
		return nil, fmt.Errorf("credpool: decode open leases: %w", err)
	}
	return out, nil
}

func (s *MongoLeaseStore) HasServedAttempt(ctx context.Context, runID, pledgeID string) (bool, error) {
	n, err := s.col.CountDocuments(ctx, bson.M{
		"run_id":    runID,
		"pledge_id": pledgeID,
		"outcome":   bson.M{"$nin": nonAdmissionOutcomes},
	})
	if err != nil {
		return false, fmt.Errorf("credpool: prior attempt lookup: %w", err)
	}
	return n > 0, nil
}

func (s *MongoLeaseStore) Close(ctx context.Context, leaseID string, costUSD float64, outcome string, when time.Time) (bool, error) {
	// Conditional on closed:false — winning this CAS is what earns the
	// right to charge the donor, so a redelivered report loses it and
	// charges nothing.
	res, err := s.col.UpdateOne(ctx,
		bson.M{"_id": leaseID, "closed": false},
		bson.M{"$set": bson.M{
			"closed":    true,
			"cost_usd":  costUSD,
			"outcome":   outcome,
			"closed_at": when.UTC(),
		}})
	if err != nil {
		return false, fmt.Errorf("credpool: close lease: %w", err)
	}
	if res.MatchedCount > 0 {
		return true, nil
	}
	n, cerr := s.col.CountDocuments(ctx, bson.M{"_id": leaseID})
	if cerr == nil && n == 0 {
		return false, ErrNotFound
	}
	return false, nil
}

func (s *MongoLeaseStore) LiveCommitment(ctx context.Context, pledgeID, excludeRunID string, now time.Time) (int, float64, error) {
	filter := bson.M{
		"pledge_id":  pledgeID,
		"closed":     false,
		"expires_at": bson.M{"$gt": now.UTC()},
	}
	if excludeRunID != "" {
		filter["run_id"] = bson.M{"$ne": excludeRunID}
	}
	cur, err := s.col.Aggregate(ctx, []bson.M{
		{"$match": filter},
		{"$group": bson.M{
			"_id":       nil,
			"runs":      bson.M{"$sum": 1},
			"committed": bson.M{"$sum": "$granted_cost_usd"},
		}},
	})
	if err != nil {
		return 0, 0, fmt.Errorf("credpool: live commitment: %w", err)
	}
	defer cur.Close(ctx)
	var out []struct {
		Runs      int     `bson:"runs"`
		Committed float64 `bson:"committed"`
	}
	if err := cur.All(ctx, &out); err != nil {
		return 0, 0, fmt.Errorf("credpool: decode live commitment: %w", err)
	}
	if len(out) == 0 {
		return 0, 0, nil
	}
	return out[0].Runs, out[0].Committed, nil
}

func (s *MongoLeaseStore) ListExpired(ctx context.Context, now time.Time, limit int) ([]Lease, error) {
	opts := options.Find().SetSort(bson.D{{Key: "expires_at", Value: 1}})
	if limit > 0 {
		opts = opts.SetLimit(int64(limit))
	}
	cur, err := s.col.Find(ctx, bson.M{"closed": false, "expires_at": bson.M{"$lte": now.UTC()}}, opts)
	if err != nil {
		return nil, fmt.Errorf("credpool: list expired leases: %w", err)
	}
	defer cur.Close(ctx)
	var out []Lease
	if err := cur.All(ctx, &out); err != nil {
		return nil, fmt.Errorf("credpool: decode expired leases: %w", err)
	}
	return out, nil
}

func (s *MongoLeaseStore) ListByDonor(ctx context.Context, donorID string, limit int) ([]Lease, error) {
	opts := options.Find().SetSort(bson.D{{Key: "acquired_at", Value: -1}})
	if limit > 0 {
		opts = opts.SetLimit(int64(limit))
	}
	cur, err := s.col.Find(ctx, bson.M{"donor_id": donorID}, opts)
	if err != nil {
		return nil, fmt.Errorf("credpool: list donor leases: %w", err)
	}
	defer cur.Close(ctx)
	var out []Lease
	if err := cur.All(ctx, &out); err != nil {
		return nil, fmt.Errorf("credpool: decode donor leases: %w", err)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Ledger
// ---------------------------------------------------------------------------

// MongoLedger is the production Ledger. One document per (pledge, period);
// the daily run counter is bumped through findOneAndUpdate so the admission
// decision reads a post-increment document — the same CAS strategy as
// orgusage.MongoCounter.
type MongoLedger struct {
	col    *mongo.Collection
	logger *iterlog.Logger
}

func NewMongoLedger(db *mongo.Database) *MongoLedger {
	return &MongoLedger{col: db.Collection(colLedger)}
}

// WithLogger lets the ledger report a lost rollback, which would otherwise
// silently inflate a donor's daily run count.
func (l *MongoLedger) WithLogger(lg *iterlog.Logger) *MongoLedger { l.logger = lg; return l }

type ledgerDoc struct {
	Runs         int       `bson:"runs"`
	CostMillis   int64     `bson:"cost_usd_millis"`
	InputTokens  int64     `bson:"input_tokens"`
	OutputTokens int64     `bson:"output_tokens"`
	PeriodStart  time.Time `bson:"period_start"`
}

func (l *MongoLedger) Reserve(ctx context.Context, pledgeID string, when time.Time, lim Limits, live LiveCommitment) (float64, DenyReason, error) {
	// Concurrency first: it is the cheapest refusal and the one a donor
	// most expects to be immediate.
	if lim.MaxConcurrentRuns > 0 && live.Runs >= lim.MaxConcurrentRuns {
		return 0, DenyConcurrency, nil
	}
	dayID := ledgerKey(pledgeID, periodDay, dayKey(when))
	var day ledgerDoc
	err := l.col.FindOneAndUpdate(ctx,
		bson.M{"_id": dayID},
		bson.M{
			"$inc":         bson.M{"runs": 1},
			"$setOnInsert": bson.M{"period_start": dayStart(when)},
		},
		options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.After),
	).Decode(&day)
	if err != nil {
		return 0, DenyNone, fmt.Errorf("credpool: reserve: %w", err)
	}

	// The weekly bucket only ever caps COST, which AddSpend accumulates
	// after the fact — so it is a plain read, no increment to roll back.
	// Skipped entirely for a donor who set no weekly cap (the common
	// shape): it would be a round trip per candidate, per launch, whose
	// result nothing reads.
	var week ledgerDoc
	if lim.MaxUSDPerWeek > 0 {
		werr := l.col.FindOne(ctx, bson.M{"_id": ledgerKey(pledgeID, periodWeek, weekKey(when))}).Decode(&week)
		if werr != nil && !errors.Is(werr, mongo.ErrNoDocuments) {
			l.rollback(ctx, dayID)
			return 0, DenyNone, fmt.Errorf("credpool: reserve (week): %w", werr)
		}
	}

	// day.Runs is already post-increment, which is the count decide judges.
	remaining, deny := decide(lim, day.Runs, millisToCost(day.CostMillis), millisToCost(week.CostMillis), live)
	if deny != DenyNone {
		l.rollback(ctx, dayID)
		return 0, deny, nil
	}
	return remaining, DenyNone, nil
}

// Renew re-checks the spend caps for an already-admitted run: two plain
// reads, no increment and nothing to roll back.
func (l *MongoLedger) Renew(ctx context.Context, pledgeID string, when time.Time, lim Limits, live LiveCommitment) (float64, DenyReason, error) {
	if lim.MaxConcurrentRuns > 0 && live.Runs >= lim.MaxConcurrentRuns {
		return 0, DenyConcurrency, nil
	}
	day, err := l.readBucket(ctx, pledgeID, periodDay, dayKey(when))
	if err != nil {
		return 0, DenyNone, err
	}
	week, err := l.readBucket(ctx, pledgeID, periodWeek, weekKey(when))
	if err != nil {
		return 0, DenyNone, err
	}
	return decideRenew(lim, day.CostUSD, week.CostUSD, live)
}

// rollback releases an optimistically-consumed run unit. Detached ctx: a
// cancelled request must still give the quota back, else a refused
// admission leaves the donor's counter permanently inflated.
//
// A lost rollback is exactly that inflation, and it is invisible from the
// donor's side — so it is reported rather than swallowed.
func (l *MongoLedger) rollback(ctx context.Context, dayID string) {
	if _, err := l.col.UpdateOne(context.WithoutCancel(ctx), bson.M{"_id": dayID}, bson.M{"$inc": bson.M{"runs": -1}}); err != nil && l.logger != nil {
		l.logger.Warn("credpool: could not roll back the reserved run unit of %s: %v (this donor's daily run count now over-reports by one)", dayID, err)
	}
}

func (l *MongoLedger) ReleaseRun(ctx context.Context, pledgeID string, when time.Time) error {
	// No upsert, and a floor at zero: decrementing a fresh (or TTL-evicted)
	// document would drive the count NEGATIVE, which silently hands the
	// donor extra runs for the rest of the day. The memory ledger has the
	// same guard — the two must decide identically.
	_, err := l.col.UpdateOne(ctx,
		bson.M{"_id": ledgerKey(pledgeID, periodDay, dayKey(when)), "runs": bson.M{"$gt": 0}},
		bson.M{"$inc": bson.M{"runs": -1}})
	if err != nil {
		return fmt.Errorf("credpool: release run: %w", err)
	}
	return nil
}

func (l *MongoLedger) AddSpend(ctx context.Context, pledgeID string, when time.Time, costUSD float64, in, out int64) error {
	inc := bson.M{}
	if m := CostToMillis(costUSD); m > 0 {
		inc["cost_usd_millis"] = m
	}
	if in > 0 {
		inc["input_tokens"] = in
	}
	if out > 0 {
		inc["output_tokens"] = out
	}
	if len(inc) == 0 {
		return nil
	}
	for _, b := range []struct {
		id    string
		start time.Time
	}{
		{ledgerKey(pledgeID, periodDay, dayKey(when)), dayStart(when)},
		{ledgerKey(pledgeID, periodWeek, weekKey(when)), weekStart(when)},
	} {
		if _, err := l.col.UpdateOne(ctx, bson.M{"_id": b.id}, bson.M{
			"$inc":         inc,
			"$setOnInsert": bson.M{"period_start": b.start},
		}, options.UpdateOne().SetUpsert(true)); err != nil {
			return fmt.Errorf("credpool: add spend: %w", err)
		}
	}
	return nil
}

func (l *MongoLedger) Usage(ctx context.Context, pledgeID string, when time.Time) (Usage, Usage, error) {
	day, err := l.readBucket(ctx, pledgeID, periodDay, dayKey(when))
	if err != nil {
		return Usage{}, Usage{}, err
	}
	week, err := l.readBucket(ctx, pledgeID, periodWeek, weekKey(when))
	if err != nil {
		return Usage{}, Usage{}, err
	}
	return day, week, nil
}

func (l *MongoLedger) readBucket(ctx context.Context, pledgeID, period, key string) (Usage, error) {
	u := Usage{Period: periodName(period), Key: key}
	var doc ledgerDoc
	err := l.col.FindOne(ctx, bson.M{"_id": ledgerKey(pledgeID, period, key)}).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return u, nil
	}
	if err != nil {
		return u, fmt.Errorf("credpool: usage: %w", err)
	}
	u.Runs = doc.Runs
	u.CostUSD = millisToCost(doc.CostMillis)
	u.InputTokens = doc.InputTokens
	u.OutputTokens = doc.OutputTokens
	return u, nil
}

func (l *MongoLedger) UsageMany(ctx context.Context, pledgeIDs []string, when time.Time) (map[string]Usage, error) {
	key := dayKey(when)
	out := make(map[string]Usage, len(pledgeIDs))
	ids := make([]string, 0, len(pledgeIDs))
	byDocID := make(map[string]string, len(pledgeIDs))
	for _, p := range pledgeIDs {
		// Absent buckets must still appear as zero usage: a donor who has
		// served nothing today is the LEAST consumed, and dropping them
		// here would rank them last instead of first.
		out[p] = Usage{Period: "day", Key: key}
		id := ledgerKey(p, periodDay, key)
		ids = append(ids, id)
		byDocID[id] = p
	}
	if len(ids) == 0 {
		return out, nil
	}
	cur, err := l.col.Find(ctx, bson.M{"_id": bson.M{"$in": ids}})
	if err != nil {
		return nil, fmt.Errorf("credpool: usage many: %w", err)
	}
	defer cur.Close(ctx)
	for cur.Next(ctx) {
		var raw struct {
			ID        string `bson:"_id"`
			ledgerDoc `bson:",inline"`
		}
		if err := cur.Decode(&raw); err != nil {
			return nil, fmt.Errorf("credpool: decode usage: %w", err)
		}
		pledgeID, ok := byDocID[raw.ID]
		if !ok {
			continue
		}
		out[pledgeID] = Usage{
			Period:       "day",
			Key:          key,
			Runs:         raw.Runs,
			CostUSD:      millisToCost(raw.CostMillis),
			InputTokens:  raw.InputTokens,
			OutputTokens: raw.OutputTokens,
		}
	}
	return out, cur.Err()
}

package credusage

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

const colCredUsage = "credential_usage"

// MongoCounter is the production Counter. One document per (credential,
// tier, tenant, month); every increment goes through an upserting $inc so
// concurrent runner pods accumulate without a read-modify-write between
// them — the same CAS strategy as orgusage.MongoCounter.
type MongoCounter struct{ col *mongo.Collection }

func NewMongoCounter(db *mongo.Database) *MongoCounter {
	return &MongoCounter{col: db.Collection(colCredUsage)}
}

// EnsureSchema creates the indexes idempotently.
func EnsureSchema(ctx context.Context, db *mongo.Database) error {
	col := db.Collection(colCredUsage)
	if _, err := col.Indexes().CreateMany(ctx, []mongo.IndexModel{
		// The two listing shapes: a tenant's month, and one credential
		// across tenants (what a platform or lent key really cost).
		{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "month", Value: 1}},
			Options: options.Index().SetName("credusage_tenant_month")},
		{Keys: bson.D{{Key: "fingerprint", Value: 1}, {Key: "month", Value: 1}},
			Options: options.Index().SetName("credusage_fp_month")},
		// The platform tier's own month: its rows live under the tenants
		// it served, so this cannot be asked by tenant.
		{Keys: bson.D{{Key: "tier", Value: 1}, {Key: "month", Value: 1}},
			Options: options.Index().SetName("credusage_tier_month")},
		// One repository's month, across credentials and tenants — what a
		// per-repo quota is checked against, so it is a lookup on the hot
		// path rather than a reporting convenience. Sparse: the documents
		// that name no repository are the majority and are never an answer
		// here.
		{Keys: bson.D{{Key: "repo_id", Value: 1}, {Key: "month", Value: 1}},
			Options: options.Index().SetName("credusage_repo_month").SetSparse(true)},
		{Keys: bson.D{{Key: "month_start", Value: 1}},
			Options: options.Index().SetName("credusage_ttl").
				SetExpireAfterSeconds(int32(RetentionDays * 24 * 60 * 60))},
	}); err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("credusage: ensure indexes: %w", err)
	}
	return nil
}

type usageDoc struct {
	Month       string `bson:"month"`
	Fingerprint string `bson:"fingerprint"`
	Provider    string `bson:"provider"`
	Tier        string `bson:"tier"`
	TenantID    string `bson:"tenant_id"`
	// Absent on every document written before repositories became an
	// accounting dimension, and on any run that targets none — which is why
	// docID omits an empty repo rather than encoding it: those documents keep
	// their id and go on accumulating.
	RepoID        string `bson:"repo_id,omitempty"`
	Nature        string `bson:"nature"`
	CostUSDMillis int64  `bson:"cost_usd_millis"`
	InputTokens   int64  `bson:"input_tokens"`
	OutputTokens  int64  `bson:"output_tokens"`
	// Absent on documents written before the aggregate got its own
	// counter: those months' aggregates are inside InputTokens and
	// cannot be separated after the fact.
	AggregateTokens int64     `bson:"aggregate_tokens"`
	Runs            int       `bson:"runs"`
	Backends        []string  `bson:"backends,omitempty"`
	MonthStart      time.Time `bson:"month_start"`
}

func (d usageDoc) view() MonthlyUsage {
	return MonthlyUsage{
		Month:           d.Month,
		Fingerprint:     d.Fingerprint,
		Provider:        d.Provider,
		Tier:            Tier(d.Tier),
		TenantID:        d.TenantID,
		RepoID:          d.RepoID,
		Nature:          Nature(d.Nature),
		CostUSD:         millisToCost(d.CostUSDMillis),
		InputTokens:     d.InputTokens,
		OutputTokens:    d.OutputTokens,
		AggregateTokens: d.AggregateTokens,
		Runs:            d.Runs,
		Backends:        d.Backends,
	}
}

func (c *MongoCounter) AddSpend(ctx context.Context, when time.Time, s Spend) error {
	if !s.recordable() {
		return nil
	}
	inc := bson.M{"runs": 1}
	if m := CostToMillis(s.CostUSD); m > 0 {
		inc["cost_usd_millis"] = m
	}
	if s.InputTokens > 0 {
		inc["input_tokens"] = s.InputTokens
	}
	if s.OutputTokens > 0 {
		inc["output_tokens"] = s.OutputTokens
	}
	if s.AggregateTokens > 0 {
		inc["aggregate_tokens"] = s.AggregateTokens
	}
	update := bson.M{
		"$inc": inc,
		"$setOnInsert": bson.M{
			"month":       monthKey(when),
			"month_start": monthStart(when),
			"fingerprint": s.Fingerprint,
			"provider":    s.Provider,
			"tier":        string(s.Tier),
			"tenant_id":   s.TenantID,
			"repo_id":     s.RepoID,
			// Nature is a property of the CREDENTIAL, not of a call: it
			// cannot change within a month for one fingerprint+tier, so
			// the first writer settles it and later ones leave it alone.
			"nature": string(s.Nature),
		},
	}
	if s.Backend != "" {
		update["$addToSet"] = bson.M{"backends": s.Backend}
	}
	_, err := c.col.UpdateOne(ctx, bson.M{"_id": docID(s.Key, when)}, update,
		options.UpdateOne().SetUpsert(true))
	if err != nil {
		return fmt.Errorf("credusage: add spend: %w", err)
	}
	return nil
}

func (c *MongoCounter) Usage(ctx context.Context, when time.Time, k Key) (MonthlyUsage, error) {
	out := MonthlyUsage{
		Month: monthKey(when), Fingerprint: k.Fingerprint,
		Provider: k.Provider, Tier: k.Tier, TenantID: k.TenantID, RepoID: k.RepoID,
	}
	if !k.Valid() {
		return out, nil
	}
	var doc usageDoc
	err := c.col.FindOne(ctx, bson.M{"_id": docID(k, when)}).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return out, nil
	}
	if err != nil {
		return out, fmt.Errorf("credusage: usage: %w", err)
	}
	return doc.view(), nil
}

func (c *MongoCounter) List(ctx context.Context, when time.Time, tenantID string) ([]MonthlyUsage, error) {
	return c.summed(ctx, bson.M{"tenant_id": tenantID, "month": monthKey(when)})
}

func (c *MongoCounter) ListByFingerprint(ctx context.Context, when time.Time, fingerprint string) ([]MonthlyUsage, error) {
	if fingerprint == "" {
		return nil, nil
	}
	return c.summed(ctx, bson.M{"fingerprint": fingerprint, "month": monthKey(when)})
}

func (c *MongoCounter) ListByTier(ctx context.Context, when time.Time, tier Tier) ([]MonthlyUsage, error) {
	if tier == "" {
		return nil, nil
	}
	return c.summed(ctx, bson.M{"tier": string(tier), "month": monthKey(when)})
}

// ListByRepo keeps the per-repository rows apart — it is the one listing
// scoped to a single repo, so summing them would collapse the very dimension
// asked for.
func (c *MongoCounter) ListByRepo(ctx context.Context, when time.Time, repoID string) ([]MonthlyUsage, error) {
	if repoID == "" {
		return nil, nil
	}
	rows, err := c.find(ctx, bson.M{"repo_id": repoID, "month": monthKey(when)})
	if err != nil {
		return nil, err
	}
	sortUsage(rows)
	return rows, nil
}

// summed is the shape every listing older than the repo dimension takes: the
// rows a credential accumulated across repositories, added back together.
func (c *MongoCounter) summed(ctx context.Context, filter bson.M) ([]MonthlyUsage, error) {
	rows, err := c.find(ctx, filter)
	if err != nil {
		return nil, err
	}
	rows = aggregateRepos(rows)
	sortUsage(rows)
	return rows, nil
}

func (c *MongoCounter) find(ctx context.Context, filter bson.M) ([]MonthlyUsage, error) {
	// Ordered by _id, which the memory twin mirrors by sorting its own ids:
	// aggregateRepos keeps first-seen order, so an undefined scan order would
	// make the summed sequence differ between the twins — and between two
	// calls — wherever two rows tie on cost. The conformance suite compares
	// sequences, so "usually the same" is a flake waiting for a busy month.
	cur, err := c.col.Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}))
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, nil
		}
		return nil, fmt.Errorf("credusage: list: %w", err)
	}
	defer func() { _ = cur.Close(ctx) }()
	var docs []usageDoc
	if err := cur.All(ctx, &docs); err != nil {
		return nil, fmt.Errorf("credusage: list: decode: %w", err)
	}
	out := make([]MonthlyUsage, 0, len(docs))
	for _, d := range docs {
		out = append(out, d.view())
	}
	// The caller sorts: the presentation order is biggest-spend-first over
	// cost_usd_millis, and after summing a credential's repositories the
	// totals — not the individual rows — are what has to be ranked.
	return out, nil
}

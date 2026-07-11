package orgusage

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

const colUsage = "org_usage"

// MongoCounter is the production Counter. One document per
// (org, month); all increments go through findOneAndUpdate / $inc so
// the allow/deny decision is atomic per call (same CAS strategy as
// webhooks.MongoCounter).
type MongoCounter struct{ col *mongo.Collection }

func NewMongoCounter(db *mongo.Database) *MongoCounter {
	return &MongoCounter{col: db.Collection(colUsage)}
}

// EnsureSchema creates the usage indexes idempotently.
func EnsureSchema(ctx context.Context, db *mongo.Database) error {
	col := db.Collection(colUsage)
	if _, err := col.Indexes().CreateMany(ctx, []mongo.IndexModel{
		// month_start drives TTL eviction of stale months. _id already
		// keys (org, month) so no secondary lookup index is needed.
		{Keys: bson.D{{Key: "month_start", Value: 1}},
			Options: options.Index().SetName("usage_ttl").SetExpireAfterSeconds(int32(RetentionDays * 24 * 60 * 60))},
	}); err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("orgusage: ensure indexes: %w", err)
	}
	return nil
}

type usageDoc struct {
	Runs          int       `bson:"runs"`
	CostUSDMillis int64     `bson:"cost_usd_millis"`
	InputTokens   int64     `bson:"input_tokens"`
	OutputTokens  int64     `bson:"output_tokens"`
	MonthStart    time.Time `bson:"month_start"`
}

func (c *MongoCounter) AllowRun(ctx context.Context, tenantID string, when time.Time, maxRuns int, maxCostMillis int64) (DenyReason, error) {
	key := usageKey(tenantID, when)
	var doc usageDoc
	err := c.col.FindOneAndUpdate(ctx,
		bson.M{"_id": key},
		bson.M{
			"$inc":         bson.M{"runs": 1},
			"$setOnInsert": bson.M{"month_start": monthStart(when)},
		},
		options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.After),
	).Decode(&doc)
	if err != nil {
		return DenyNone, fmt.Errorf("orgusage: run bump: %w", err)
	}
	// Both caps read off the SAME post-increment document — one round
	// trip covers run quota AND cost cap. A denied call rolls back the
	// optimistic increment so it doesn't consume quota. Eventually
	// consistent under heavy concurrency; the allow/deny decision
	// itself is atomic — the property a monthly cap needs.
	deny := DenyNone
	switch {
	case maxRuns > 0 && doc.Runs > maxRuns:
		deny = DenyRuns
	case maxCostMillis > 0 && doc.CostUSDMillis >= maxCostMillis:
		deny = DenyCost
	}
	if deny != DenyNone {
		// Detached ctx: a cancelled request must still release the
		// optimistically-consumed quota unit, else a denied call leaves the
		// monthly run counter permanently inflated.
		_, _ = c.col.UpdateOne(context.WithoutCancel(ctx), bson.M{"_id": key}, bson.M{"$inc": bson.M{"runs": -1}})
	}
	return deny, nil
}

// ReleaseRun mirrors the deny-path rollback in AllowRun: one $inc -1
// on the month's run counter, no upsert (a TTL-evicted or never-created
// month document stays absent — decrementing a fresh doc below zero
// would corrupt the metering).
func (c *MongoCounter) ReleaseRun(ctx context.Context, tenantID string, when time.Time) error {
	_, err := c.col.UpdateOne(ctx,
		bson.M{"_id": usageKey(tenantID, when)},
		bson.M{"$inc": bson.M{"runs": -1}})
	if err != nil {
		return fmt.Errorf("orgusage: release run: %w", err)
	}
	return nil
}

func (c *MongoCounter) AddSpend(ctx context.Context, tenantID string, when time.Time, costUSD float64, inputTokens, outputTokens int64) error {
	inc := bson.M{}
	if m := CostToMillis(costUSD); m > 0 {
		inc["cost_usd_millis"] = m
	}
	if inputTokens > 0 {
		inc["input_tokens"] = inputTokens
	}
	if outputTokens > 0 {
		inc["output_tokens"] = outputTokens
	}
	if len(inc) == 0 {
		return nil
	}
	_, err := c.col.UpdateOne(ctx,
		bson.M{"_id": usageKey(tenantID, when)},
		bson.M{
			"$inc":         inc,
			"$setOnInsert": bson.M{"month_start": monthStart(when)},
		},
		options.UpdateOne().SetUpsert(true),
	)
	if err != nil {
		return fmt.Errorf("orgusage: add spend: %w", err)
	}
	return nil
}

func (c *MongoCounter) Usage(ctx context.Context, tenantID string, when time.Time) (MonthlyUsage, error) {
	out := MonthlyUsage{Month: monthKey(when)}
	var doc usageDoc
	err := c.col.FindOne(ctx, bson.M{"_id": usageKey(tenantID, when)}).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return out, nil
	}
	if err != nil {
		return out, fmt.Errorf("orgusage: usage: %w", err)
	}
	out.Runs = doc.Runs
	out.CostUSD = millisToCost(doc.CostUSDMillis)
	out.InputTokens = doc.InputTokens
	out.OutputTokens = doc.OutputTokens
	return out, nil
}

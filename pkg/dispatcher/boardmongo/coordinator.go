package boardmongo

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
)

// Candidate is one dispatch-eligible board issue plus the tenant that owns it.
type Candidate struct {
	Tenant string
	Issue  native.Issue
}

// Coordinator is the cross-tenant view of the Mongo board the cloud
// dispatcher polls: it lists eligible issues across all tenants in one query
// and hands out tenant-scoped stores for claim/transition. Multi-replica
// safety comes from the per-issue Claim CAS (no leader election).
type Coordinator struct {
	db   *mongo.Database
	coll *mongo.Collection
}

// NewCoordinator builds a cross-tenant coordinator over db.
func NewCoordinator(db *mongo.Database) *Coordinator {
	return &Coordinator{db: db, coll: db.Collection(IssuesCollection)}
}

// StoreFor returns a tenant-scoped board store (for claim/transition/release).
func (c *Coordinator) StoreFor(tenant string) *Store { return New(c.db, tenant) }

// ListEligible returns up to `limit` UNCLAIMED issues whose state is in
// `eligible`, across every tenant, in the requested update order:
// oldest-updated first (newestFirst=false) for the dispatch tick — FIFO
// fairness — or newest-updated first for the stranded-card sweeps, whose
// capped window must always contain the freshest strandings (a stranding
// bumps UpdatedAt, and a sweep that leaves a card in place does not, so
// under oldest-first a saturated board's forgotten pile occupies the window
// permanently and starves exactly the cards the sweep exists to rescue).
// (v1 assumes the default-board eligibility passed by the caller; a
// per-tenant custom board schema is a future refinement. Blocker gating is
// left to the per-issue processor.)
func (c *Coordinator) ListEligible(ctx context.Context, eligible []string, limit int, newestFirst bool) ([]Candidate, error) {
	if len(eligible) == 0 {
		return nil, nil
	}
	order := 1
	if newestFirst {
		order = -1
	}
	opt := options.Find().SetSort(bson.D{{Key: "issue.updatedat", Value: order}})
	if limit > 0 {
		opt.SetLimit(int64(limit))
	}
	cur, err := c.coll.Find(ctx, bson.M{
		"issue.state": bson.M{"$in": eligible},
		"issue.claim": "",
	}, opt)
	if err != nil {
		return nil, fmt.Errorf("boardmongo: list eligible: %w", err)
	}
	var docs []issueDoc
	if err := cur.All(ctx, &docs); err != nil {
		return nil, fmt.Errorf("boardmongo: decode eligible: %w", err)
	}
	out := make([]Candidate, 0, len(docs))
	for _, d := range docs {
		out = append(out, Candidate{Tenant: d.Tenant, Issue: d.Issue})
	}
	return out, nil
}

// Claim / SetState / Release delegate to the tenant-scoped store, with the
// context-carrying signatures the dispatcher loop uses. The CAS lives in
// Store.Claim.
func (c *Coordinator) Claim(_ context.Context, tenant, id, marker string) error {
	return c.StoreFor(tenant).Claim(id, marker)
}

func (c *Coordinator) SetState(_ context.Context, tenant, id, state string) error {
	_, err := c.StoreFor(tenant).SetState(id, state)
	return err
}

func (c *Coordinator) Release(_ context.Context, tenant, id, marker string) error {
	return c.StoreFor(tenant).Release(id, marker)
}

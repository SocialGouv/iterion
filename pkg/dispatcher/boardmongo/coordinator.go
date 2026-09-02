package boardmongo

import (
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/dispatcher/tracker"

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

// DistinctEffectTenants lists the tenants holding non-terminal trigger-effect
// rows. The effect drain unions this with the subscription-derived tenant
// list: a tenant whose LAST board subscription was disabled after rows were
// materialized must still have those rows executed (or parked) — otherwise
// they hibernate until a re-enable fires days-old events at once.
func (c *Coordinator) DistinctEffectTenants(ctx context.Context) ([]string, error) {
	res := c.db.Collection(EffectsCollection).Distinct(ctx, "tenant_id",
		bson.M{"state": bson.M{"$in": bson.A{"pending", "claimed"}}})
	if err := res.Err(); err != nil {
		return nil, fmt.Errorf("boardmongo: distinct effect tenants: %w", err)
	}
	var vals []string
	if err := res.Decode(&vals); err != nil {
		return nil, fmt.Errorf("boardmongo: decode effect tenants: %w", err)
	}
	return vals, nil
}

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
// Store.Claim. Claim returns the ownership token so the cloud dispatcher
// can heartbeat and fence its writes (the tokenless form is gone on
// purpose — a caller that discards the token cannot renew its lease).
func (c *Coordinator) Claim(_ context.Context, tenant, id, marker string) (tracker.ClaimToken, error) {
	return c.StoreFor(tenant).Claim(id, marker)
}

func (c *Coordinator) SetState(_ context.Context, tenant, id, state string) error {
	_, err := c.StoreFor(tenant).SetState(id, state)
	return err
}

func (c *Coordinator) Release(_ context.Context, tenant, id, marker string) error {
	return c.StoreFor(tenant).Release(id, marker)
}

// RenewClaim / SetStateOwned / ReleaseOwned are the fenced delegates the
// cloud dispatcher's claim session uses: every write is a CAS on the
// ownership token, so a replica whose claim was superseded finds typed
// refusals instead of clobbering the new owner.
func (c *Coordinator) RenewClaim(_ context.Context, tenant, id string, tok tracker.ClaimToken) error {
	return c.StoreFor(tenant).RenewClaim(id, tok)
}

func (c *Coordinator) SetStateOwned(_ context.Context, tenant, id, state string, tok tracker.ClaimToken) error {
	_, err := c.StoreFor(tenant).SetStateOwned(id, state, tok)
	return err
}

func (c *Coordinator) ReleaseOwned(_ context.Context, tenant, id string, tok tracker.ClaimToken) error {
	return c.StoreFor(tenant).ReleaseOwned(id, tok)
}

// ExpiredCandidate is one cross-tenant reap candidate.
type ExpiredCandidate struct {
	Tenant string
	Claim  tracker.ExpiredClaim
}

// ListExpiredClaimCandidates is the reaper's cross-tenant listing — on
// the Coordinator, not the tenant-scoped store, because the cloud
// reaper (one per replica, CAS-serialized per card) must see every
// tenant's expired claims (the plan review's F12).
func (c *Coordinator) ListExpiredClaimCandidates(ctx context.Context, cutoff time.Time, limit int) ([]ExpiredCandidate, error) {
	if limit <= 0 {
		limit = 100
	}
	cur, err := c.coll.Find(ctx, bson.M{
		"issue.claim":           bson.M{"$ne": ""},
		"issue.claimleaseuntil": bson.M{"$gt": time.Time{}, "$lt": cutoff},
	}, options.Find().SetSort(bson.D{{Key: "issue.claimleaseuntil", Value: 1}}).SetLimit(int64(limit)))
	if err != nil {
		return nil, fmt.Errorf("boardmongo: list expired claims (cross-tenant): %w", err)
	}
	var docs []issueDoc
	if err := cur.All(ctx, &docs); err != nil {
		return nil, fmt.Errorf("boardmongo: decode expired claims (cross-tenant): %w", err)
	}
	out := make([]ExpiredCandidate, 0, len(docs))
	for _, d := range docs {
		out = append(out, ExpiredCandidate{Tenant: d.Tenant, Claim: tracker.ExpiredClaim{
			IssueID:    d.Issue.ID,
			Identifier: d.Issue.ID,
			State:      d.Issue.State,
			LastRunID:  d.Issue.LastRunID,
			Prev:       tracker.ClaimToken{Marker: d.Issue.Claim, Epoch: d.Issue.ClaimEpoch},
		}})
	}
	return out, nil
}

// ReclaimExpired delegates the CAS transfer to the tenant-scoped store.
func (c *Coordinator) ReclaimExpired(_ context.Context, tenant, id string, prev tracker.ClaimToken, marker string, cutoff time.Time) (tracker.ClaimToken, error) {
	return c.StoreFor(tenant).ReclaimExpired(id, prev, marker, cutoff)
}

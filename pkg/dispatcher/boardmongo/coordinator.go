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
		"issue.claim": bson.M{"$in": bson.A{"", nil}},
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
	// The SAME two arms as the tenant-scoped listing (reclaimableLease),
	// queried separately for the same two reasons: a candidate this
	// produces must be one ReclaimExpired can accept, and positive
	// evidence (an expired lease) must not be starved by claims that
	// merely lack one. This is the listing the CLOUD reaper calls —
	// teaching only the Store left the un-leased recovery path dead on
	// the twin that has no boot sweep to fall back on.
	out := make([]ExpiredCandidate, 0, limit)
	for _, arm := range reclaimableLease(cutoff) {
		if len(out) >= limit {
			break
		}
		filter := bson.M{"issue.claim": bson.M{"$gt": ""}}
		for k, v := range arm {
			filter[k] = v
		}
		cur, err := c.coll.Find(ctx, filter,
			options.Find().SetSort(bson.D{{Key: "issue.claimleaseuntil", Value: 1}}).
				SetLimit(int64(limit-len(out))))
		if err != nil {
			return nil, fmt.Errorf("boardmongo: list expired claims (cross-tenant): %w", err)
		}
		var docs []issueDoc
		if err := cur.All(ctx, &docs); err != nil {
			return nil, fmt.Errorf("boardmongo: decode expired claims (cross-tenant): %w", err)
		}
		for _, d := range docs {
			iss := d.Issue
			out = append(out, ExpiredCandidate{Tenant: d.Tenant, Claim: native.ExpiredClaimFrom(&iss)})
		}
	}
	return out, nil
}

// ListAbandonedRecoveryClaims lists expired claims held under a marker
// namespace — the watchdog's own, so a replica can repair what a crashed
// one left behind. It SELECTS on the marker instead of filtering a
// general batch, because a recovery claim is stamped with a fresh lease
// at the moment of the transfer: it therefore sorts after every ordinary
// dead owner, and a post-hoc filter over a capped, lease-ordered batch
// sees exactly none of them on any board that has been running a while.
func (c *Coordinator) ListAbandonedRecoveryClaims(ctx context.Context, markerPrefix string, cutoff time.Time, limit int) ([]ExpiredCandidate, error) {
	if limit <= 0 {
		limit = 100
	}
	if markerPrefix == "" {
		return nil, fmt.Errorf("boardmongo: list abandoned recovery claims: empty marker prefix would match every claim")
	}
	out := make([]ExpiredCandidate, 0, limit)
	for _, arm := range reclaimableLease(cutoff) {
		if len(out) >= limit {
			break
		}
		upper, uerr := prefixUpperBound(markerPrefix)
		if uerr != nil {
			return nil, uerr
		}
		filter := bson.M{"issue.claim": bson.M{"$gte": markerPrefix, "$lt": upper}}
		for k, v := range arm {
			filter[k] = v
		}
		cur, err := c.coll.Find(ctx, filter,
			options.Find().SetSort(bson.D{{Key: "issue.claimleaseuntil", Value: 1}}).
				SetLimit(int64(limit-len(out))))
		if err != nil {
			return nil, fmt.Errorf("boardmongo: list abandoned recovery claims: %w", err)
		}
		var docs []issueDoc
		if err := cur.All(ctx, &docs); err != nil {
			return nil, fmt.Errorf("boardmongo: decode abandoned recovery claims: %w", err)
		}
		for _, d := range docs {
			iss := d.Issue
			out = append(out, ExpiredCandidate{Tenant: d.Tenant, Claim: native.ExpiredClaimFrom(&iss)})
		}
	}
	return out, nil
}

// prefixUpperBound is the exclusive end of a string-prefix range: the
// last byte incremented, so the scan stays an index range rather than a
// regex the planner cannot bound. A prefix with no upper bound (empty,
// or all 0xFF) has no such range — returning the prefix itself would
// silently match NOTHING, so it is an error the caller must see.
func prefixUpperBound(prefix string) (string, error) {
	b := []byte(prefix)
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] < 0xFF {
			b[i]++
			return string(b[:i+1]), nil
		}
	}
	return "", fmt.Errorf("boardmongo: marker prefix %q has no upper bound — a prefix range built from it would match nothing", prefix)
}

func (c *Coordinator) ListUnleasedClaims(ctx context.Context, cutoff time.Time, limit int) ([]ExpiredCandidate, error) {
	if limit <= 0 {
		limit = 100
	}
	filter := UnleasedArm(cutoff)
	filter["issue.claim"] = bson.M{"$gt": ""}
	cur, err := c.coll.Find(ctx, filter,
		options.Find().SetSort(bson.D{{Key: "issue.updatedat", Value: 1}}).SetLimit(int64(limit)))
	if err != nil {
		return nil, fmt.Errorf("boardmongo: list un-leased claims: %w", err)
	}
	var docs []issueDoc
	if err := cur.All(ctx, &docs); err != nil {
		return nil, fmt.Errorf("boardmongo: decode un-leased claims: %w", err)
	}
	out := make([]ExpiredCandidate, 0, len(docs))
	for _, d := range docs {
		iss := d.Issue
		out = append(out, ExpiredCandidate{Tenant: d.Tenant, Claim: native.ExpiredClaimFrom(&iss)})
	}
	return out, nil
}

func (c *Coordinator) ReclaimExpired(_ context.Context, tenant, id string, prev tracker.ClaimToken, marker string, cutoff time.Time) (tracker.ClaimToken, string, error) {
	return c.StoreFor(tenant).ReclaimExpired(id, prev, marker, cutoff)
}

// ServerNow reads the DATABASE's clock. The lease is stamped with
// `$$NOW` (server-side) precisely so a pod with a fast local clock cannot
// mint itself extra lease — but the reaper then compared those
// server-stamped leases against its OWN clock, which re-opened the same
// hole from the other end: a replica running N minutes fast sees every
// lease younger than N minutes as expired and reclaims cards from LIVE
// owners. One round-trip per pass (a minute apart) buys the comparison a
// single clock.
func (c *Coordinator) ServerNow(ctx context.Context) (time.Time, error) {
	cur, err := c.coll.Aggregate(ctx, mongo.Pipeline{
		{{Key: "$limit", Value: 1}},
		{{Key: "$project", Value: bson.M{"_id": 0, "now": "$$NOW"}}},
	})
	if err != nil {
		return time.Time{}, fmt.Errorf("boardmongo: server clock: %w", err)
	}
	defer func() { _ = cur.Close(ctx) }()
	var rows []struct {
		Now time.Time `bson:"now"`
	}
	if err := cur.All(ctx, &rows); err != nil {
		return time.Time{}, fmt.Errorf("boardmongo: decode server clock: %w", err)
	}
	if len(rows) == 0 || rows[0].Now.IsZero() {
		// An empty collection has no document to project from. The caller
		// falls back to its own clock — with nothing claimed there is also
		// nothing for a skewed cutoff to steal.
		return time.Time{}, nil
	}
	return rows[0].Now.UTC(), nil
}

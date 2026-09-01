package mongo

import (
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/store"
)

// ClaimRouteDecision atomically inserts the (run_id, outcome_seq) row
// (see store.RouteDecisionStore). The unique index is the claim: a
// duplicate key means the episode is already decided — the existing
// row comes back so the caller can report WHO decided and how it went.
func (s *Store) ClaimRouteDecision(ctx context.Context, d store.RouteDecision) (bool, *store.RouteDecision, error) {
	if d.RunID == "" {
		return false, nil, fmt.Errorf("store/mongo: claim route decision without run_id")
	}
	d.ID = fmt.Sprintf("%s:%d", d.RunID, d.OutcomeSeq)
	d.State = store.RouteDecisionClaimed
	d.ClaimedAt = time.Now().UTC()
	stampTenantDecision(ctx, &d)
	if _, err := s.routeDecisions.InsertOne(ctx, d); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			var existing store.RouteDecision
			ferr := s.routeDecisions.FindOne(ctx, bson.M{"run_id": d.RunID, "outcome_seq": d.OutcomeSeq}).Decode(&existing)
			if ferr != nil {
				return false, nil, fmt.Errorf("store/mongo: route decision %s exists but is unreadable: %w", d.ID, ferr)
			}
			return false, &existing, nil
		}
		return false, nil, fmt.Errorf("store/mongo: claim route decision %s: %w", d.ID, err)
	}
	return true, nil, nil
}

// FinishRouteDecision moves the claimed row to its terminal state.
func (s *Store) FinishRouteDecision(ctx context.Context, runID string, outcomeSeq int64, state, actionErr string) error {
	if state != store.RouteDecisionSucceeded && state != store.RouteDecisionFailed {
		return fmt.Errorf("store/mongo: finish route decision: invalid state %q", state)
	}
	now := time.Now().UTC()
	res, err := s.routeDecisions.UpdateOne(ctx,
		bson.M{"run_id": runID, "outcome_seq": outcomeSeq, "state": store.RouteDecisionClaimed},
		bson.M{"$set": bson.M{"state": state, "error": actionErr, "finished_at": now}})
	if err != nil {
		return fmt.Errorf("store/mongo: finish route decision %s:%d: %w", runID, outcomeSeq, err)
	}
	if res.MatchedCount == 0 {
		return fmt.Errorf("store/mongo: finish route decision %s:%d: no claimed row", runID, outcomeSeq)
	}
	return nil
}

// ListRouteDecisions returns a run's decisions, newest episode first.
func (s *Store) ListRouteDecisions(ctx context.Context, runID string) ([]store.RouteDecision, error) {
	cur, err := s.routeDecisions.Find(ctx, bson.M{"run_id": runID},
		options.Find().SetSort(bson.D{{Key: "outcome_seq", Value: -1}}))
	if err != nil {
		return nil, fmt.Errorf("store/mongo: list route decisions %s: %w", runID, err)
	}
	var out []store.RouteDecision
	if err := cur.All(ctx, &out); err != nil {
		return nil, fmt.Errorf("store/mongo: list route decisions %s: %w", runID, err)
	}
	return out, nil
}

// stampTenantDecision mirrors the run-doc tenant stamping: the registry
// row carries the tenant the context resolved, so per-tenant audit
// views filter naturally.
func stampTenantDecision(ctx context.Context, d *store.RouteDecision) {
	if d.TenantID != "" {
		return
	}
	if tenant, ok := store.TenantFromContext(ctx); ok {
		d.TenantID = tenant
	}
}

// ListRoutableRuns — the sweep net's query (see store.RouteDecisionStore).
func (s *Store) ListRoutableRuns(ctx context.Context, since time.Time, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 200
	}
	cur, err := s.runs.Find(ctx, notDeleted(bson.M{
		"routing_policy": bson.M{"$exists": true, "$ne": nil},
		"status": bson.M{"$in": bson.A{
			store.RunStatusFinished, store.RunStatusFailed, store.RunStatusFailedResumable,
		}},
		"updated_at": bson.M{"$gte": since},
	}), options.Find().
		SetProjection(bson.M{"_id": 1}).
		SetSort(bson.D{{Key: "updated_at", Value: -1}}).
		SetLimit(int64(limit)))
	if err != nil {
		return nil, fmt.Errorf("store/mongo: list routable runs: %w", err)
	}
	var rows []struct {
		ID string `bson:"_id"`
	}
	if err := cur.All(ctx, &rows); err != nil {
		return nil, fmt.Errorf("store/mongo: list routable runs: %w", err)
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ID)
	}
	return out, nil
}

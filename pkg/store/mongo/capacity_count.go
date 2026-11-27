package mongo

import (
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/store"
)

var _ store.AliveCapacityLister = (*Store)(nil)

// capacityListingRow is the projection ListAliveRunsForCapacity reads: the
// fields store.ProjectForCapacity keeps, by their document names.
type capacityListingRow struct {
	ID                 string                  `bson:"_id"`
	TenantID           string                  `bson:"tenant_id"`
	BotID              string                  `bson:"bot_id"`
	Status             store.RunStatus         `bson:"status"`
	ContinuationState  store.ContinuationState `bson:"continuation_state"`
	ContinuationUntil  *time.Time              `bson:"continuation_until"`
	ParentRunID        string                  `bson:"parent_run_id"`
	ParentNodeID       string                  `bson:"parent_node_id"`
	CredFingerprints   []string                `bson:"cred_fingerprints"`
	CapacitySubjects   []string                `bson:"capacity_subjects"`
	CapacityStampedFor string                  `bson:"capacity_stamped_for"`
	ContentBoundary    *struct {
		KeyID string `bson:"key_id"`
	} `bson:"content_boundary"`
}

// ListAliveRunsForCapacity is the Mongo twin (see store.AliveCapacityLister):
// the runs that hold capacity at the listing's instant (holdsCapacityBranches,
// the arms the ledger's count reads), not deleted, across every tenant.
// Deliberately NO withTenantFilter: it would scope the read to whatever tenant
// ctx carries — the admin routes stamp the platform tenant — and a shared
// gateway's holders belong to every team. The status_created index serves the
// status arm, capacity_transit the transit arm. A record that does not decode
// is an error, never a holder left out.
func (s *Store) ListAliveRunsForCapacity(ctx context.Context) ([]store.Run, error) {
	filter := rootedHolderFilter([]bson.M{{}}, time.Now(), notDeleted(bson.M{}))
	projection := bson.M{
		"_id": 1, "tenant_id": 1, "bot_id": 1, "status": 1,
		"continuation_state": 1, "continuation_until": 1,
		"parent_run_id": 1, "parent_node_id": 1,
		"cred_fingerprints": 1, "capacity_subjects": 1, "capacity_stamped_for": 1,
		"content_boundary.key_id": 1,
	}
	cur, err := s.runs.Find(ctx, filter, options.Find().SetProjection(projection).SetSort(bson.D{{Key: "_id", Value: 1}}))
	if err != nil {
		return nil, fmt.Errorf("store/mongo: list alive runs for capacity: %w", err)
	}
	defer cur.Close(ctx)
	out := []store.Run{}
	for cur.Next(ctx) {
		var row capacityListingRow
		if err := cur.Decode(&row); err != nil {
			return nil, fmt.Errorf("store/mongo: decode alive run for capacity: %w", err)
		}
		if row.ID == "" {
			return nil, fmt.Errorf("store/mongo: alive run for capacity has no identity")
		}
		run := store.Run{
			ID: row.ID, TenantID: row.TenantID, BotID: row.BotID, Status: row.Status,
			ContinuationState: row.ContinuationState, ContinuationUntil: row.ContinuationUntil,
			ParentRunID: row.ParentRunID, ParentNodeID: row.ParentNodeID,
			CredFingerprints: row.CredFingerprints, CapacitySubjects: row.CapacitySubjects, CapacityStampedFor: row.CapacityStampedFor,
		}
		if row.ContentBoundary != nil {
			run.ContentBoundary = &store.RunContentBoundary{KeyID: row.ContentBoundary.KeyID}
		}
		out = append(out, store.ProjectForCapacity(&run))
	}
	if err := cur.Err(); err != nil {
		return nil, fmt.Errorf("store/mongo: alive runs for capacity cursor: %w", err)
	}
	return out, nil
}

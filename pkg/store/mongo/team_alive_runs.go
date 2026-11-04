package mongo

import (
	"context"
	"fmt"
	"sort"

	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

var _ store.TeamAliveRunsReader = (*Store)(nil)

func (s *Store) ListAliveRunsForTeam(ctx context.Context, teamID string) ([]string, error) {
	if teamID == "" {
		return nil, fmt.Errorf("store/mongo: alive team runs: empty team")
	}
	// withTenantFilter overwrites a filter's tenant_id. Refuse a contradictory
	// caller scope before constructing it, rather than returning another team.
	if tenant, ok := store.TenantFromContext(ctx); ok && tenant != teamID {
		return nil, fmt.Errorf("store/mongo: alive team runs: context tenant differs from target team")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Exclude only scalar, recognized inactive states. A query $nin also
	// excludes malformed arrays containing an inactive value; such metadata
	// must remain visible here and fail strict decoding instead.
	filter := bson.M{"$expr": bson.M{"$not": bson.A{bson.M{"$and": bson.A{
		bson.M{"$eq": bson.A{bson.M{"$type": "$status"}, "string"}},
		bson.M{"$in": bson.A{"$status", store.TeamCutoverInactiveStatuses()}},
	}}}}}
	ctx = store.WithTenant(ctx, teamID)
	cur, err := s.runs.Find(ctx, notDeleted(withTenantFilter(ctx, filter)),
		options.Find().SetProjection(bson.M{"_id": 1, "status": 1}))
	if err != nil {
		return nil, fmt.Errorf("store/mongo: list alive team runs: %w", err)
	}
	defer cur.Close(ctx)
	ids := []string{}
	for cur.Next(ctx) {
		var row struct {
			ID     string          `bson:"_id"`
			Status store.RunStatus `bson:"status"`
		}
		if err := cur.Decode(&row); err != nil {
			return nil, fmt.Errorf("store/mongo: decode alive run metadata: %w", err)
		}
		if row.ID == "" {
			return nil, fmt.Errorf("store/mongo: alive run has no identity")
		}
		alive, err := store.TeamCutoverStatusAlive(row.Status)
		if err != nil {
			return nil, fmt.Errorf("store/mongo: alive run %s: %w", row.ID, err)
		}
		if alive {
			ids = append(ids, row.ID)
		}
	}
	if err := cur.Err(); err != nil {
		return nil, fmt.Errorf("store/mongo: alive run cursor: %w", err)
	}
	sort.Strings(ids)
	return ids, nil
}

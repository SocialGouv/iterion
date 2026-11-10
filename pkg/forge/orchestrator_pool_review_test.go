package forge

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/cloudsched"
	"github.com/SocialGouv/iterion/pkg/credpool"
)

// flakyListSchedules fails ListByIntegration once — a transient read.
type flakyListSchedules struct {
	*cloudsched.MemoryStore
	failNext bool
}

func (f *flakyListSchedules) ListByIntegration(ctx context.Context, tenantID, integrationID string) ([]cloudsched.ScheduledBot, error) {
	if f.failNext {
		f.failNext = false
		return nil, errors.New("mongo: server selection timeout")
	}
	return f.MemoryStore.ListByIntegration(ctx, tenantID, integrationID)
}

// RVA-H: syncSchedulesWithPool swallows a ListByIntegration error, deletes
// every row and rebuilds from the manifests — the operator's OWN row block
// (here an explicit opt-out, enabled:false) is gone and the projection
// writes the integration's ENABLED pool over it (sb.Pool == nil → project).
func TestPoolForgeReview_TransientRowReadReplacesTheOperatorsOptOutWithTheProjection(t *testing.T) {
	mem := cloudsched.NewMemoryStore()
	store := &flakyListSchedules{MemoryStore: mem}
	now := time.Unix(1700000000, 0).UTC()
	o := &Orchestrator{Schedules: store, Now: func() time.Time { return now }, NewID: func() string { return "s1" }}
	inv := map[string][]bundle.Invocation{
		"docs-refresh": {{Kind: bundle.InvocationKindSchedule, Schedule: &bundle.InvocationSchedule{SuggestedCron: "0 3 * * *"}}},
	}
	ctx := context.Background()
	integ := poolTestBlock("pool-a")
	if err := o.syncSchedulesWithPool(ctx, "t1", "ri-1", "https://github.com/org/app.git", inv, nil, "u1", nil, integ); err != nil {
		t.Fatal(err)
	}
	rows, _ := mem.ListByIntegration(ctx, "t1", "ri-1")
	optOut := &credpool.Binding{PoolID: "pool-a", Enabled: false}
	if _, err := mem.Update(ctx, rows[0].ID, cloudsched.SchedulePatch{UpdatedAt: now.Add(time.Minute), Pool: optOut}); err != nil {
		t.Fatal(err)
	}
	store.failNext = true
	err := o.syncSchedulesWithPool(ctx, "t1", "ri-1", "https://github.com/org/app.git", inv, nil, "u1", nil, integ)
	rows, _ = mem.ListByIntegration(ctx, "t1", "ri-1")
	t.Logf("sync after a transient row read: err=%v, row pool=%+v fromIntegration=%v", err, rows[0].Pool, rows[0].PoolFromIntegration)
	if !rows[0].Pool.Equal(optOut) {
		t.Errorf("the operator's row opt-out was replaced by the integration's projection (err=%v)", err)
	}
}

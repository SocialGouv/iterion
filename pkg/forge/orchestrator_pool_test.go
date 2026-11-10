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

// The integration's pool block (ADR-164) rides the routing block's exact
// path: the record, the webhook config (the enforcement half the webhook
// lane reads), the scheduled rows (with provenance), the read-back verify,
// and a divergence between the two stores refused on a silent request.

func poolTestBlock(poolID string) *credpool.Binding {
	margin := 70.0
	return &credpool.Binding{
		PoolID:   poolID,
		Enabled:  true,
		Strategy: credpool.Strategy{Mode: credpool.StrategyPriority, MarginPct: &margin, Triggers: []string{"usage_window"}},
	}
}

// TestProvision_PoolChanges_Clears_AndSurvives walks the three directions
// of the three-way rule on ONE repo, both stores asserted at each step.
func TestProvision_PoolChanges_Clears_AndSurvives(t *testing.T) {
	o, _, _ := newTestOrch(t)
	conn := seedConn(t, o, o.Sealer)
	ctx := context.Background()

	provision := func(req ProvisionRequest) {
		t.Helper()
		req.TenantID, req.ConnectionID, req.RepoFullName, req.ActorID = "t1", conn.ID, "group/api", "tester"
		if len(req.BotIDs) == 0 {
			req.BotIDs = []string{"review-pr"}
		}
		if _, err := o.Provision(ctx, req); err != nil {
			t.Fatalf("provision: %v", err)
		}
	}
	assertPool := func(phase string, want *credpool.Binding) {
		t.Helper()
		ri, err := o.Integrations.GetByConnRepo(ctx, "t1", conn.ID, "group/api")
		if err != nil {
			t.Fatalf("%s: read integration: %v", phase, err)
		}
		cfg, err := o.Webhooks.Get(ctx, ri.WebhookID)
		if err != nil {
			t.Fatalf("%s: read webhook: %v", phase, err)
		}
		if !ri.Pool.Equal(want) {
			t.Errorf("%s: integration pool = %+v, want %+v", phase, ri.Pool, want)
		}
		if !cfg.Pool.Equal(want) {
			t.Errorf("%s: webhook pool = %+v, want %+v — the config is the ENFORCEMENT half the webhook lane reads", phase, cfg.Pool, want)
		}
	}

	provision(ProvisionRequest{Pool: poolTestBlock("pool-a"), PoolSet: true})
	assertPool("changed: set from nothing", poolTestBlock("pool-a"))

	provision(ProvisionRequest{})
	assertPool("unspecified: preserved", poolTestBlock("pool-a"))

	// A silent REBUILD (one more bot): the literal adopts the stored block.
	provision(ProvisionRequest{BotIDs: []string{"review-pr", "revi-converse"}})
	assertPool("unspecified: survives the rebuild", poolTestBlock("pool-a"))

	provision(ProvisionRequest{BotIDs: []string{"review-pr", "revi-converse"}, PoolSet: true})
	assertPool("cleared: both stores", nil)

	provision(ProvisionRequest{BotIDs: []string{"review-pr", "revi-converse"}})
	assertPool("cleared: survives the next silent provision", nil)

	// A replace riding a rebuild wins over prev.
	provision(ProvisionRequest{BotIDs: []string{"review-pr", "revi-converse", "dep-guard"}, Pool: poolTestBlock("pool-b"), PoolSet: true})
	assertPool("changed: the rebuild's new block wins", poolTestBlock("pool-b"))

	// The clear riding a rebuild (the Replace unbind the approval apply
	// replays) lands on BOTH stores — nothing resurrects it from prev.
	provision(ProvisionRequest{BotIDs: []string{"review-pr", "revi-converse"}, Replace: true, PoolSet: true})
	assertPool("cleared: the rebuild that carried the clear", nil)
}

// TestProvision_ADivergedPoolPairRefusesASilentRequest: the integration and
// its webhook config disagree on the pool block (a partial write, a hand
// edit) — a silent re-provision cannot tell a deliberate drop from a stale
// store, so it refuses instead of picking one; an explicit block converges.
func TestProvision_ADivergedPoolPairRefusesASilentRequest(t *testing.T) {
	o, _, _ := newTestOrch(t)
	conn := seedConn(t, o, o.Sealer)
	ctx := context.Background()
	base := ProvisionRequest{TenantID: "t1", ConnectionID: conn.ID, RepoFullName: "group/api", BotIDs: []string{"review-pr"}, ActorID: "tester"}

	req := base
	req.Pool, req.PoolSet = poolTestBlock("pool-a"), true
	res, err := o.Provision(ctx, req)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	cfg, err := o.Webhooks.Get(ctx, res.WebhookID)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Pool = poolTestBlock("pool-stale")
	if err := o.Webhooks.Update(ctx, cfg); err != nil {
		t.Fatal(err)
	}

	if _, err := o.Provision(ctx, base); !errors.Is(err, ErrProvisionDiverged) {
		t.Fatalf("silent re-provision over a diverged pool pair = %v, want ErrProvisionDiverged", err)
	}
	req = base
	req.Pool, req.PoolSet = poolTestBlock("pool-a"), true
	if _, err := o.Provision(ctx, req); err != nil {
		t.Fatalf("an explicit block must converge the pair: %v", err)
	}
}

// TestVerifyOperatorSettings_RefusesAPoolDivergence: the read-back pass
// certifies the pool block on both stores like every operator field.
func TestVerifyOperatorSettings_RefusesAPoolDivergence(t *testing.T) {
	o, _, _ := newTestOrch(t)
	conn := seedConn(t, o, o.Sealer)
	ctx := context.Background()
	res, err := o.Provision(ctx, ProvisionRequest{
		TenantID: "t1", ConnectionID: conn.ID, RepoFullName: "group/api",
		BotIDs: []string{"review-pr"}, ActorID: "tester",
		Pool: poolTestBlock("pool-a"), PoolSet: true,
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	integ, err := o.Integrations.Get(ctx, res.IntegrationID)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := o.Webhooks.Get(ctx, res.WebhookID)
	if err != nil {
		t.Fatal(err)
	}
	wantInteg, wantCfg := integ, cfg

	integ.Pool = poolTestBlock("pool-tampered")
	if err := o.Integrations.Update(ctx, integ); err != nil {
		t.Fatal(err)
	}
	if err := o.verifyOperatorSettings(ctx, wantInteg, wantCfg); !errors.Is(err, ErrProvisionDiverged) {
		t.Fatalf("integration half: verifyOperatorSettings = %v, want ErrProvisionDiverged", err)
	}

	fresh, err := o.Integrations.Get(ctx, res.IntegrationID)
	if err != nil {
		t.Fatal(err)
	}
	fresh.Pool = wantInteg.Pool
	if err := o.Integrations.Update(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	wantInteg.Version = fresh.Version + 1
	cfg.Pool = poolTestBlock("pool-tampered")
	if err := o.Webhooks.Update(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if err := o.verifyOperatorSettings(ctx, wantInteg, wantCfg); !errors.Is(err, ErrProvisionDiverged) {
		t.Fatalf("webhook half: verifyOperatorSettings = %v, want ErrProvisionDiverged", err)
	}
}

// TestSyncSchedules_ProjectedPoolFollowsTheIntegration: the scheduled row
// is the only layer a scheduled launch resolves, so the integration's
// block projects onto it — and follows every later integration write (a
// clear and a replace) while an operator's own row block stands.
func TestSyncSchedules_ProjectedPoolFollowsTheIntegration(t *testing.T) {
	mem := cloudsched.NewMemoryStore()
	now := time.Unix(1700000000, 0).UTC()
	o := &Orchestrator{Schedules: mem, Now: func() time.Time { return now }, NewID: func() string { return "s1" }}
	inv := map[string][]bundle.Invocation{
		"docs-refresh": {{Kind: bundle.InvocationKindSchedule, Schedule: &bundle.InvocationSchedule{SuggestedCron: "0 3 * * *"}}},
	}
	ctx := context.Background()
	sync := func(pool *credpool.Binding) {
		t.Helper()
		if err := o.syncSchedulesWithPool(ctx, "t1", "ri-1", "https://github.com/org/app.git", inv, nil, "u1", nil, pool); err != nil {
			t.Fatalf("sync: %v", err)
		}
	}
	row := func(phase string) cloudsched.ScheduledBot {
		t.Helper()
		rows, err := mem.ListByIntegration(ctx, "t1", "ri-1")
		if err != nil || len(rows) != 1 {
			t.Fatalf("%s: list rows: %v (%d rows)", phase, err, len(rows))
		}
		return rows[0]
	}

	sync(poolTestBlock("pool-a"))
	if r := row("projected"); !r.Pool.Equal(poolTestBlock("pool-a")) || !r.PoolFromIntegration {
		t.Fatalf("projected: row pool = %+v (from integration %v), want pool-a projected", r.Pool, r.PoolFromIntegration)
	}

	sync(nil)
	if r := row("after integration clear"); r.Pool != nil || r.PoolFromIntegration {
		t.Fatalf("after integration clear: row pool = %+v (from integration %v), want nil — a projected copy must not shadow the clear", r.Pool, r.PoolFromIntegration)
	}

	sync(poolTestBlock("pool-b"))
	if r := row("after integration replace"); !r.Pool.Equal(poolTestBlock("pool-b")) {
		t.Fatalf("after integration replace: row pool = %+v, want pool-b", r.Pool)
	}

	// The OPERATOR writes the row's own block through the schedule PATCH:
	// the provenance flag drops, and the integration no longer speaks
	// through the row in either direction.
	own := poolTestBlock("pool-operator")
	r := row("before operator patch")
	if _, err := mem.Update(ctx, r.ID, cloudsched.SchedulePatch{UpdatedAt: now.Add(time.Minute), Pool: own}); err != nil {
		t.Fatalf("operator row patch: %v", err)
	}
	if r := row("operator patch"); r.PoolFromIntegration {
		t.Fatalf("operator patch: the provenance flag survived the operator's own write — the next sync would overwrite the operator's block")
	}
	sync(poolTestBlock("pool-c"))
	if r := row("operator block survives a replace"); !r.Pool.Equal(own) {
		t.Fatalf("operator block survives a replace: row pool = %+v, want the operator's", r.Pool)
	}
	sync(nil)
	if r := row("operator block survives a clear"); !r.Pool.Equal(own) {
		t.Fatalf("operator block survives a clear: row pool = %+v, want the operator's", r.Pool)
	}
}

// TestClonePoolBinding_ProjectedRowsShareNoState: each projected row owns
// its block — the margin pointer and the trigger slice are copies.
func TestClonePoolBinding_ProjectedRowsShareNoState(t *testing.T) {
	src := poolTestBlock("pool-a")
	c := clonePoolBinding(src)
	*c.Strategy.MarginPct = 10
	c.Strategy.Triggers[0] = "unavailable"
	if *src.Strategy.MarginPct != 70 || src.Strategy.Triggers[0] != "usage_window" {
		t.Fatalf("a clone's edit leaked into its source: %+v", src.Strategy)
	}
	if clonePoolBinding(nil) != nil {
		t.Fatal("clone(nil) must stay nil — an absent block is not an empty one")
	}
}

// listFailingStore fails ListByIntegration on demand — a transient read.
type listFailingStore struct {
	*cloudsched.MemoryStore
	fail bool
}

func (s *listFailingStore) ListByIntegration(ctx context.Context, tenantID, integrationID string) ([]cloudsched.ScheduledBot, error) {
	if s.fail {
		return nil, errors.New("store blip")
	}
	return s.MemoryStore.ListByIntegration(ctx, tenantID, integrationID)
}

// TestSyncSchedules_aFailedRowReadRefusesTheRebuild: the rebuild deletes
// every row and recreates it from what it READ — a read that failed must
// refuse the sync, or the operator's own row block (an opt-out here) is
// erased and replaced by the integration's projection.
func TestSyncSchedules_aFailedRowReadRefusesTheRebuild(t *testing.T) {
	mem := &listFailingStore{MemoryStore: cloudsched.NewMemoryStore()}
	now := time.Unix(1700000000, 0).UTC()
	o := &Orchestrator{Schedules: mem, Now: func() time.Time { return now }, NewID: func() string { return "s1" }}
	inv := map[string][]bundle.Invocation{
		"docs-refresh": {{Kind: bundle.InvocationKindSchedule, Schedule: &bundle.InvocationSchedule{SuggestedCron: "0 3 * * *"}}},
	}
	ctx := context.Background()
	if err := o.syncSchedulesWithPool(ctx, "t1", "ri-1", "https://github.com/org/app.git", inv, nil, "u1", nil, poolTestBlock("pool-a")); err != nil {
		t.Fatalf("seed sync: %v", err)
	}
	rows, _ := mem.MemoryStore.ListByIntegration(ctx, "t1", "ri-1")
	optOut := &credpool.Binding{PoolID: "pool-a", Enabled: false}
	if _, err := mem.Update(ctx, rows[0].ID, cloudsched.SchedulePatch{UpdatedAt: now.Add(time.Minute), Pool: optOut}); err != nil {
		t.Fatal(err)
	}

	mem.fail = true
	if err := o.syncSchedulesWithPool(ctx, "t1", "ri-1", "https://github.com/org/app.git", inv, nil, "u1", nil, poolTestBlock("pool-a")); err == nil {
		t.Fatal("a sync whose row read failed reported success — it rebuilds from nothing")
	}
	mem.fail = false
	rows, _ = mem.MemoryStore.ListByIntegration(ctx, "t1", "ri-1")
	if len(rows) != 1 || !rows[0].Pool.Equal(optOut) || rows[0].PoolFromIntegration {
		t.Fatalf("after the refused sync the row = %+v — the operator's opt-out must survive untouched", rows)
	}
}

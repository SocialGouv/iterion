package cloudpublisher

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The doc's effective-caps snapshot must read the figure the WIRE carries,
// not the un-clamped ask: a credential-pool grant clamps the resume's cost
// cap to the donor's remaining allowance (with CapImposed, so no exit
// grace), and at launch the runner stamps Run.Budget post-clamp. A resume
// that stamped the ask flips the field's meaning from "enforced" to
// "asked" within one run: the studio reads $120 while the pod dies
// BUDGET_EXCEEDED at the donor's $5.
func TestSubmitResume_StampsTheClampedWireCapNotTheAsk(t *testing.T) {
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	ctx := context.Background()
	oauth := secrets.NewMemoryOAuthStore()
	seedOAuth(t, oauth, sealer, "donor", "sk-ant-donated")
	pools := credpool.NewMemoryPoolStore()
	if err := pools.Upsert(ctx, credpool.Pool{
		ID: "pool-1", OrgID: poolOrg, Enabled: true,
		// The requesting team is admitted by name: the publisher under test
		// has no identity store, so the run's org resolves to "".
		Audience: credpool.Audience{Teams: []string{poolTeam}},
	}); err != nil {
		t.Fatalf("seed pool: %v", err)
	}
	pledges := credpool.NewMemoryPledgeStore()
	if err := pledges.Upsert(ctx, credpool.Pledge{
		ID: credpool.PledgeID("donor", credpool.SourceOAuth, "claude_code"), PoolID: "pool-1",
		UserID: "donor", Credential: credpool.Credential{Source: credpool.SourceOAuth, Ref: "claude_code"},
		Enabled: true, Health: credpool.HealthOK, Limits: credpool.Limits{MaxUSDPerDay: 5},
	}); err != nil {
		t.Fatalf("seed pledge: %v", err)
	}
	broker := credpool.NewBroker(credpool.BrokerConfig{
		Pools: pools, Pledges: pledges, Leases: credpool.NewMemoryLeaseStore(), Ledger: credpool.NewMemoryLedger(),
		OAuth: oauth, Sealer: sealer, Logger: testLogger(),
	})
	if broker == nil {
		t.Fatal("broker is nil with every dependency wired")
	}

	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	var published *queue.RunMessage
	p := &Publisher{
		store:      st,
		runSecrets: secrets.NewMemoryRunSecretsStore(),
		sealer:     sealer,
		credPool:   broker,
		logger:     iterlog.New(iterlog.LevelError, nil),
		publishRun: func(_ context.Context, m *queue.RunMessage) error {
			published = m
			return nil
		},
	}
	tctx := store.WithIdentity(ctx, poolTeam, "requester")
	const runID = "run-donor-clamped-resume"
	if err := st.SaveRun(tctx, &store.Run{
		ID: runID, TenantID: poolTeam, OwnerID: "requester",
		Status: store.RunStatusFailedResumable,
		Budget: &store.RunBudget{MaxCostUSD: 20}, // the launch stamp
	}); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	wf := &ir.Workflow{Name: "wf", Budget: &ir.Budget{MaxCostUSD: 20}}
	spec := runview.ResumeSpec{
		RunID: runID, FilePath: "wf.bot",
		Source: "workflow wf:\n  entry: done\n",
		Budget: &ir.BudgetOverrides{MaxCostUSD: 120},
	}
	if err := p.SubmitResume(tctx, spec, wf, "hash"); err != nil {
		t.Fatalf("SubmitResume: %v", err)
	}
	if published == nil || published.Budget == nil {
		t.Fatal("nothing published or budget dropped from the wire")
	}
	if published.Budget.MaxCostUSD != 5 || !published.Budget.CapImposed {
		t.Fatalf("wire budget = %+v, want the donor's $5 with CapImposed (the premise: the grant clamps the wire)", published.Budget)
	}
	got, err := st.LoadRun(tctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if got.Budget == nil || got.Budget.MaxCostUSD != 5 {
		t.Fatalf("doc Budget = %+v, want MaxCostUSD 5 — the doc advertises the un-clamped ask while the run is capped at the donor's $5 (CapImposed, no exit grace) and dies BUDGET_EXCEEDED there", got.Budget)
	}
	// The replay source keeps the un-clamped ask: the allowance is
	// re-derived against the CURRENT grant on every resume.
	if got.BudgetOverrides == nil || got.BudgetOverrides.MaxCostUSD != 120 {
		t.Fatalf("doc BudgetOverrides = %+v, want the raw $120 ask persisted un-clamped", got.BudgetOverrides)
	}
}

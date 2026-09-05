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
// BUDGET_EXCEEDED at the donor's $5. And the allowance is re-derived on
// EVERY resume — the ask-less auto-retry included — so the stamp must
// follow the wire on every resume, in both directions.

// donorClampedPublisher builds a publisher whose only credential is a
// pooled donor subscription with the given daily allowance; the pool
// admits the requesting team by name (no identity store, so the run's
// org resolves to ""). Every publish is captured.
func donorClampedPublisher(t *testing.T, maxUSDPerDay float64) (*Publisher, store.RunStore, *[]*queue.RunMessage) {
	t.Helper()
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
		Audience: credpool.Audience{Teams: []string{poolTeam}},
	}); err != nil {
		t.Fatalf("seed pool: %v", err)
	}
	pledges := credpool.NewMemoryPledgeStore()
	if err := pledges.Upsert(ctx, credpool.Pledge{
		ID: credpool.PledgeID("donor", credpool.SourceOAuth, "claude_code"), PoolID: "pool-1",
		UserID: "donor", Credential: credpool.Credential{Source: credpool.SourceOAuth, Ref: "claude_code"},
		Enabled: true, Health: credpool.HealthOK, Limits: credpool.Limits{MaxUSDPerDay: maxUSDPerDay},
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
	published := &[]*queue.RunMessage{}
	p := &Publisher{
		store:      st,
		runSecrets: secrets.NewMemoryRunSecretsStore(),
		sealer:     sealer,
		credPool:   broker,
		logger:     iterlog.New(iterlog.LevelError, nil),
		publishRun: func(_ context.Context, m *queue.RunMessage) error {
			*published = append(*published, m)
			return nil
		},
	}
	return p, st, published
}

func seedDonorRun(t *testing.T, st store.RunStore, runID string, budget *store.RunBudget, ask *store.RunBudgetOverrides) context.Context {
	t.Helper()
	tctx := store.WithIdentity(context.Background(), poolTeam, "requester")
	if err := st.SaveRun(tctx, &store.Run{
		ID: runID, TenantID: poolTeam, OwnerID: "requester",
		Status:          store.RunStatusFailedResumable,
		Budget:          budget,
		BudgetOverrides: ask,
	}); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	return tctx
}

func lastPublished(t *testing.T, published *[]*queue.RunMessage) *queue.RunMessage {
	t.Helper()
	if len(*published) == 0 {
		t.Fatal("nothing published")
	}
	return (*published)[len(*published)-1]
}

func TestSubmitResume_StampsTheClampedWireCapNotTheAsk(t *testing.T) {
	p, st, published := donorClampedPublisher(t, 5)
	const runID = "run-donor-clamped-resume"
	tctx := seedDonorRun(t, st, runID, &store.RunBudget{MaxCostUSD: 20}, nil) // the launch stamp
	wf := &ir.Workflow{Name: "wf", Budget: &ir.Budget{MaxCostUSD: 20}}
	spec := runview.ResumeSpec{
		RunID: runID, FilePath: "wf.bot",
		Source: "workflow wf:\n  entry: done\n",
		Budget: &ir.BudgetOverrides{MaxCostUSD: 120},
	}
	if err := p.SubmitResume(tctx, spec, wf, "hash"); err != nil {
		t.Fatalf("SubmitResume: %v", err)
	}
	msg := lastPublished(t, published)
	if msg.Budget == nil || msg.Budget.MaxCostUSD != 5 || !msg.Budget.CapImposed {
		t.Fatalf("wire budget = %+v, want the donor's $5 with CapImposed (the premise: the grant clamps the wire)", msg.Budget)
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

// Over-report: an earlier resume stamped $120 under a $500 donor; the
// ask-less auto-retry lands on a $5 donor. The wire says 5 + CapImposed,
// and so must the doc — the ask stays 120.
func TestSubmitResume_AskLessResumeRestampsTheDocWhenTheAllowanceShrinks(t *testing.T) {
	p, st, published := donorClampedPublisher(t, 5)
	const runID = "run-donor-shrank"
	tctx := seedDonorRun(t, st, runID, &store.RunBudget{MaxCostUSD: 120}, &store.RunBudgetOverrides{MaxCostUSD: 120})
	wf := &ir.Workflow{Name: "wf", Budget: &ir.Budget{MaxCostUSD: 20}}
	spec := runview.ResumeSpec{RunID: runID, FilePath: "wf.bot", Source: "workflow wf:\n  entry: done\n"} // ask-less
	if err := p.SubmitResume(tctx, spec, wf, "hash"); err != nil {
		t.Fatalf("SubmitResume: %v", err)
	}
	msg := lastPublished(t, published)
	if msg.Budget == nil || msg.Budget.MaxCostUSD != 5 || !msg.Budget.CapImposed {
		t.Fatalf("wire budget = %+v, want {5, CapImposed} (the premise)", msg.Budget)
	}
	got, err := st.LoadRun(tctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if got.Budget == nil || got.Budget.MaxCostUSD != 5 {
		t.Fatalf("doc Budget = %+v after an ask-less resume onto a $5 donor, want 5 — the studio shows $120 while the pod dies at $5 with no exit grace", got.Budget)
	}
	if got.BudgetOverrides == nil || got.BudgetOverrides.MaxCostUSD != 120 {
		t.Fatalf("doc BudgetOverrides = %+v, want the $120 ask left un-clamped (a later resume must replay it once the donor recovers)", got.BudgetOverrides)
	}
}

// Under-report: an earlier resume was clamped to $5; the ask-less
// auto-retry lands on a $500 donor. The wire says 120 (no clamp), and so
// must the doc.
func TestSubmitResume_AskLessResumeRestampsTheDocWhenTheAllowanceRecovers(t *testing.T) {
	p, st, published := donorClampedPublisher(t, 500)
	const runID = "run-donor-recovered"
	tctx := seedDonorRun(t, st, runID, &store.RunBudget{MaxCostUSD: 5}, &store.RunBudgetOverrides{MaxCostUSD: 120})
	wf := &ir.Workflow{Name: "wf", Budget: &ir.Budget{MaxCostUSD: 20}}
	spec := runview.ResumeSpec{RunID: runID, FilePath: "wf.bot", Source: "workflow wf:\n  entry: done\n"} // ask-less
	if err := p.SubmitResume(tctx, spec, wf, "hash"); err != nil {
		t.Fatalf("SubmitResume: %v", err)
	}
	msg := lastPublished(t, published)
	if msg.Budget == nil || msg.Budget.MaxCostUSD != 120 || msg.Budget.CapImposed {
		t.Fatalf("wire budget = %+v, want {120, no clamp} (the premise)", msg.Budget)
	}
	got, err := st.LoadRun(tctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if got.Budget == nil || got.Budget.MaxCostUSD != 120 {
		t.Fatalf("doc Budget = %+v after an ask-less resume onto a $500 donor, want 120 — the studio shows $5 while the run may spend $120", got.Budget)
	}
}

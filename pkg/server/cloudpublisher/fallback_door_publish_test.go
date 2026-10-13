package cloudpublisher

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/llmroute"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// The fallback door's PUBLICATION witnesses (ADR-121 § Delivery 2, slice 5;
// the follow-through #2254 named): a door-served run is LEASED exactly like
// a whole-bundle-granted one — its spend report reaches the broker, its
// lease is not superseded as grantless at publication, and a failed
// resume reopens the lease it superseded. The lease semantics ride the
// poolLease() disjunction; these tests pin it at the SubmitLaunch /
// SubmitResume seam.

type doorPublishFixture struct {
	sealer    secrets.Sealer
	rs        *store.FilesystemRunStore
	pub       *Publisher
	broker    *credpool.Broker
	pools     *credpool.MemoryPoolStore
	pledges   *credpool.MemoryPledgeStore
	leases    *credpool.MemoryLeaseStore
	tenantKey string
	logs      *bytes.Buffer
	published []*queue.RunMessage
	publishOK *bool
}

// newDoorPublishFixture wires the whole launch path: a tenant with ONLY an
// anthropic key (the bundle is non-empty — the door's premise), a policy
// whose ladder names the missing zai_key rung, and a zai donor whose
// consent is per-test.
func newDoorPublishFixture(t *testing.T, consenting bool) *doorPublishFixture {
	t.Helper()
	sealer, err := secrets.NewKeyRingSealer(map[string][]byte{"default": make([]byte, 32)}, "default")
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	ctx := context.Background()
	rs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	// The TENANT's own key (BYOK tier).
	keys := secrets.NewMemoryApiKeyStore()
	kid := secrets.NewApiKeyID()
	sealed, _ := secrets.SealAPIKey(sealer, kid, []byte("sk-ant-tenant"))
	if err := keys.Create(store.WithTenant(ctx, poolTeam), secrets.ApiKey{ID: kid, ScopeTeamID: poolTeam,
		Provider: secrets.ProviderAnthropic, Name: "own", SealedSecret: sealed, Fingerprint: "fp-own", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("seed tenant key: %v", err)
	}
	// The DONOR's zai key, pledged through the broker's own store.
	donorKeys := secrets.NewMemoryApiKeyStore()
	dkid := secrets.NewApiKeyID()
	dsealed, _ := secrets.SealAPIKey(sealer, dkid, []byte("zai-donated-key"))
	if err := donorKeys.Create(store.WithTenant(ctx, "donor-team"), secrets.ApiKey{ID: dkid, TenantID: "donor-team",
		ScopeTeamID: "donor-team", ScopeUserID: "donor", Provider: secrets.ProviderZAI, Name: "lent",
		SealedSecret: dsealed, Fingerprint: "fp-donor-zai", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("seed donor key: %v", err)
	}
	// Donor B's key rides the SAME store (a resume's landscape flip is
	// then a pledge toggle, not a store swap).
	dkid2 := secrets.NewApiKeyID()
	dsealed2, _ := secrets.SealAPIKey(sealer, dkid2, []byte("zai-donated-b"))
	if err := donorKeys.Create(store.WithTenant(ctx, "donor-b"), secrets.ApiKey{ID: dkid2, TenantID: "donor-b",
		ScopeTeamID: "donor-b", ScopeUserID: "donor-b", Provider: secrets.ProviderZAI, Name: "lent-b",
		SealedSecret: dsealed2, Fingerprint: "fp-donor-b", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("seed donor B key: %v", err)
	}
	pools := credpool.NewMemoryPoolStore()
	if err := pools.Upsert(ctx, credpool.Pool{ID: "pool-1", OrgID: poolOrg, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	pledges := credpool.NewMemoryPledgeStore()
	if err := pledges.Upsert(ctx, credpool.Pledge{
		ID: credpool.PledgeID("donor", credpool.SourceAPIKey, "zai"), PoolID: "pool-1", UserID: "donor",
		Credential: credpool.Credential{Source: credpool.SourceAPIKey, Ref: "zai", KeyID: dkid},
		Enabled:    true, Health: credpool.HealthOK, Limits: credpool.Limits{MaxUSDPerDay: 5}, FallbackUse: consenting,
	}); err != nil {
		t.Fatalf("seed pledge: %v", err)
	}
	if err := pledges.Upsert(ctx, credpool.Pledge{
		ID: credpool.PledgeID("donor-b", credpool.SourceAPIKey, "zai"), PoolID: "pool-1", UserID: "donor-b",
		Credential: credpool.Credential{Source: credpool.SourceAPIKey, Ref: "zai", KeyID: dkid2},
		Enabled:    true, Health: credpool.HealthOK, Limits: credpool.Limits{MaxUSDPerDay: 5}, FallbackUse: true,
	}); err != nil {
		t.Fatalf("seed pledge B: %v", err)
	}
	leases := credpool.NewMemoryLeaseStore()
	broker := credpool.NewBroker(credpool.BrokerConfig{
		Pools: pools, Pledges: pledges, Leases: leases, Ledger: credpool.NewMemoryLedger(),
		OAuth: secrets.NewMemoryOAuthStore(), APIKeys: donorKeys, Sealer: sealer, Logger: testLogger(),
	})
	logs := &bytes.Buffer{}
	f := &doorPublishFixture{
		sealer: sealer, rs: rs, broker: broker, pools: pools, pledges: pledges, leases: leases, tenantKey: kid,
		logs: logs, published: []*queue.RunMessage{}, publishOK: new(bool),
	}
	*f.publishOK = true
	f.pub = &Publisher{
		identity:   &fakeTeamResolver{orgs: map[string]string{poolTeam: poolOrg}},
		apiKeys:    keys,
		usageCaps:  usagecap.NewMemStore(),
		store:      rs,
		runSecrets: secrets.NewMemoryRunSecretsStore(),
		sealer:     sealer,
		credPool:   broker,
		logger:     iterlog.New(iterlog.LevelInfo, logs),
		publishRun: func(_ context.Context, m *queue.RunMessage) error {
			if *f.publishOK {
				cp := *m
				f.published = append(f.published, &cp)
				return nil
			}
			return context.DeadlineExceeded
		},
	}
	return f
}

func (f *doorPublishFixture) policy() *store.RunLLMRoutePolicy {
	return &store.RunLLMRoutePolicy{
		PairOrder: []string{
			llmroute.Pair(llmroute.HarnessClaw, "anthropic_key"),
			llmroute.Pair(llmroute.HarnessClaw, "zai_key"),
		},
		Triggers: []string{llmroute.TriggerUsageWindow},
	}
}

func (f *doorPublishFixture) wf() *ir.Workflow {
	return &ir.Workflow{Name: "wf", Nodes: map[string]ir.Node{"implement": &ir.AgentNode{
		BaseNode:  ir.BaseNode{ID: "implement"},
		LLMFields: ir.LLMFields{Backend: "claude_code", Model: "claude-opus-5-5"},
		Tools:     []string{},
	}}}
}

func (f *doorPublishFixture) launch(t *testing.T, runID string) {
	t.Helper()
	if _, err := f.pub.SubmitLaunch(store.WithIdentity(context.Background(), poolTeam, "requester"), runID, runview.LaunchSpec{
		FilePath: "wf.bot", Source: "workflow wf:\n  start -> done\n",
		LLMRoutePolicy: f.policy(),
		BotID:          "docs-refresh",
	}, f.wf(), &runview.CompiledSource{Hash: "hash"}); err != nil {
		t.Fatalf("SubmitLaunch: %v", err)
	}
}

func lastMessage(t *testing.T, f *doorPublishFixture) *queue.RunMessage {
	t.Helper()
	if len(f.published) == 0 {
		t.Fatal("no RunMessage published")
	}
	return f.published[len(f.published)-1]
}

// TestFallbackDoorPublish_doorServedRunIsNotGrantless: a door-served run's
// RunMessage carries PoolGrantless=false — its spend report reaches the
// broker (a grantless read silences it and the donor's dead credential
// stays first in rotation) — and the door lease is OPEN after
// publication (the grantless branch would supersede it as if no lease
// served the run).
// Mutant: PoolGrantless or the SupersedeRun gate keyed on creds.grant
// alone → the assertions red.
func TestFallbackDoorPublish_doorServedRunIsNotGrantless(t *testing.T) {
	f := newDoorPublishFixture(t, true)
	f.launch(t, "run-pub")

	msg := lastMessage(t, f)
	if msg.PoolGrantless {
		t.Fatal("a door-served run published PoolGrantless=true — its spend report would never reach the broker")
	}
	open, _ := f.leases.ListOpenByRun(context.Background(), "run-pub")
	if len(open) == 0 {
		t.Fatal("the door lease did not survive publication — the grantless supersede closed it")
	}
}

// TestFallbackDoorPublish_resumeSwitchesDonorsAndSaysSo: launch served by
// donor B; the consent landscape flips (B off, A stays on); the resume
// grants A — B's lease superseded at the acquisition, A's lease OPEN
// after publication (the grantless branch would have superseded every
// open lease of the run, A's fresh one included), PoolGrantless=false.
func TestFallbackDoorPublish_resumeSwitchesDonorsAndSaysSo(t *testing.T) {
	f := newDoorPublishFixture(t, true)
	ctx := store.WithIdentity(context.Background(), poolTeam, "requester")
	f.launch(t, "run-sw")

	run, err := f.rs.LoadRun(ctx, "run-sw")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	run.Status = store.RunStatusPausedOperator
	if err := f.rs.SaveRun(ctx, run); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
	before, _ := f.leases.ListOpenByRun(context.Background(), "run-sw")
	if len(before) != 1 {
		t.Fatalf("open leases before the resume = %+v (%d), want donor A's alone", before, len(before))
	}
	if before[0].DonorID != "donor-b" {
		t.Fatalf("lease donor = %q pledge = %q credential = %+v, want donor B (the id sort picks it on a zero-usage tie)", before[0].DonorID, before[0].PledgeID, before[0].Credential)
	}

	// The landscape flips: B withdraws — the resume must switch to A.
	bid := credpool.PledgeID("donor-b", credpool.SourceAPIKey, "zai")
	if err := f.pledges.Upsert(ctx, credpool.Pledge{
		ID: bid, PoolID: "pool-1", UserID: "donor-b",
		Credential: credpool.Credential{Source: credpool.SourceAPIKey, Ref: "zai"},
		Enabled:    false, Health: credpool.HealthOK, Limits: credpool.Limits{MaxUSDPerDay: 5}, FallbackUse: true,
	}); err != nil {
		t.Fatalf("disable B: %v", err)
	}
	if err := f.pub.SubmitResume(ctx, runview.ResumeSpec{
		RunID: "run-sw", FilePath: "wf.bot", Source: "workflow wf:\n  start -> done\n",
		// The resume replays the run doc's FROZEN policy — no spec field.
	}, f.wf(), &runview.CompiledSource{Hash: "hash"}); err != nil {
		t.Fatalf("SubmitResume: %v", err)
	}

	msg := lastMessage(t, f)
	if msg.PoolGrantless {
		t.Fatal("the resume published PoolGrantless=true — a door-served resume would never report its spend")
	}
	open, _ := f.leases.ListOpenByRun(context.Background(), "run-sw")
	if len(open) != 1 || open[0].DonorID != "donor" {
		t.Fatalf("open leases after the resume = %+v, want donor A's alone (B's superseded at the acquisition, A's alive after publication)", open)
	}
}

// TestFallbackDoorPublish_failedResumeReopensTheSupersededLease: a resume
// whose publication fails rolls back and REOPENS the lease its new grant
// superseded — a nil ReopenSuperseded is a silent no-op that would hold
// the previous donor's slot to the TTL.
func TestFallbackDoorPublish_failedResumeReopensTheSupersededLease(t *testing.T) {
	f := newDoorPublishFixture(t, true)
	ctx := store.WithIdentity(context.Background(), poolTeam, "requester")
	f.launch(t, "run-rollback")

	run, err := f.rs.LoadRun(ctx, "run-rollback")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	run.Status = store.RunStatusPausedOperator
	if err := f.rs.SaveRun(ctx, run); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
	before, _ := f.leases.ListOpenByRun(context.Background(), "run-rollback")
	if len(before) == 0 {
		t.Fatal("no open lease after the launch — the fixture broke")
	}

	// The resume's publication fails: the rollback reopens what the new
	// grant superseded.
	*f.publishOK = false
	if err := f.pub.SubmitResume(ctx, runview.ResumeSpec{
		RunID: "run-rollback", FilePath: "wf.bot", Source: "workflow wf:\n  start -> done\n",
	}, f.wf(), &runview.CompiledSource{Hash: "hash"}); err == nil {
		t.Fatal("SubmitResume succeeded despite the publish failure — the fault seam broke")
	}
	after, _ := f.leases.ListOpenByRun(context.Background(), "run-rollback")
	if len(after) == 0 {
		t.Fatalf("every lease of the run is closed after a failed resume — the rollback did not reopen the superseded lease (a nil ReopenSuperseded is a silent no-op)")
	}
}

// TestFallbackDoorPreview_parity: the preview's door fields answer what
// the live consult serves on the same input.
func TestFallbackDoorPreview_parity(t *testing.T) {
	f := newDoorPublishFixture(t, true)
	preview, err := f.pub.PreviewCredentials(context.Background(), runview.CredentialPreviewSpec{
		Context:        runview.CredentialPreviewContext{TeamID: poolTeam, BotID: "docs-refresh"},
		OwnerID:        "requester",
		Launch:         runview.LaunchSpec{BotID: "docs-refresh"},
		LLMRoutePolicy: f.policy(),
	}, f.wf())
	if err != nil {
		t.Fatalf("PreviewCredentials: %v", err)
	}
	found := false
	for _, w := range preview.Pool.DoorWants {
		if w == "api_key:zai" {
			found = true
		}
	}
	if !found {
		t.Fatalf("door_wants = %v, want the missing zai kind (door_reason = %q)", preview.Pool.DoorWants, preview.Pool.DoorReason)
	}
}

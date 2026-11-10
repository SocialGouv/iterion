package cloudpublisher

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/credpool"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// rvaFixture is newAttachedFixture with the ledger kept, and the gateway key
// optionally a legacy row without its base URL.
type rvaFixture struct {
	*attachedFixture
	ledger *credpool.MemoryLedger
}

func newRVAFixture(t *testing.T, gatewayURL string) *rvaFixture {
	t.Helper()
	sealer, err := secrets.NewKeyRingSealer(map[string][]byte{"default": make([]byte, 32)}, "default")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	oauth := secrets.NewMemoryOAuthStore()
	seedOAuth(t, oauth, sealer, "donor", "sk-ant-donated")
	pools := credpool.NewMemoryPoolStore()
	if err := pools.Upsert(ctx, credpool.Pool{ID: "pool-att", OrgID: poolOrg, Enabled: true,
		OwnerType: credpool.PoolOwnerUser, OwnerID: "ops", Visibility: credpool.PoolVisibilityPrivate}); err != nil {
		t.Fatal(err)
	}
	pledges := credpool.NewMemoryPledgeStore()
	if err := pledges.Upsert(ctx, credpool.Pledge{
		ID: credpool.PledgeID("donor", credpool.SourceOAuth, "claude_code"), PoolID: "pool-att",
		UserID: "donor", Credential: credpool.Credential{Source: credpool.SourceOAuth, Ref: "claude_code"},
		Enabled: true, Health: credpool.HealthOK, Priority: 0,
		Limits: credpool.Limits{MaxUSDPerDay: 50},
	}); err != nil {
		t.Fatal(err)
	}
	apiKeys := secrets.NewMemoryApiKeyStore()
	gw := secrets.ApiKey{ID: "key-gw", TenantID: poolTeam, ScopeTeamID: poolTeam, ScopeUserID: "gw-lender",
		Provider: secrets.ProviderOpenAICompatible, Name: "pliage-deepseek", BaseURL: gatewayURL}
	if gatewayURL != "" {
		gw.Fingerprint = secrets.APIKeyFingerprint(secrets.ProviderOpenAICompatible, "sk-gw-0000", gatewayURL)
		gw.SealedSecret, err = secrets.SealAPIKeyFor(sealer, gw, []byte("sk-gw-0000"))
	} else {
		// A row written before the gateway URL requirement: sealed under the
		// AAD a URL-less gateway row opens with.
		gw.SealedSecret, err = sealer.Seal([]byte("sk-gw-0000"), []byte("api_key:key-gw\x00"))
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := apiKeys.Create(ctx, gw); err != nil {
		t.Fatal(err)
	}
	if err := pledges.Upsert(ctx, credpool.Pledge{
		ID: credpool.PledgeID("gw-lender", credpool.SourceAPIKey, string(secrets.ProviderOpenAICompatible)), PoolID: "pool-att",
		UserID:     "gw-lender",
		Credential: credpool.Credential{Source: credpool.SourceAPIKey, Ref: string(secrets.ProviderOpenAICompatible), KeyID: "key-gw"},
		Enabled:    true, Health: credpool.HealthOK,
		Model: "openai_compatible/scaleway/deepseek-v4.1-flash", Priority: 5,
		Limits: credpool.Limits{MaxUSDPerDay: 50},
	}); err != nil {
		t.Fatal(err)
	}
	leases := credpool.NewMemoryLeaseStore()
	ledger := credpool.NewMemoryLedger()
	broker := credpool.NewBroker(credpool.BrokerConfig{Pools: pools, Pledges: pledges, Leases: leases,
		Ledger: ledger, OAuth: oauth, APIKeys: apiKeys, Sealer: sealer, Logger: testLogger()})
	rs := secrets.NewMemoryRunSecretsStore()
	return &rvaFixture{attachedFixture: &attachedFixture{
		pub: &Publisher{runSecrets: rs, sealer: sealer, credPool: broker,
			logger: iterlog.New(iterlog.LevelError, nil), rotatedOAuthKinds: rotatedClaude},
		rs: rs, sealer: sealer, pools: pools, pledges: pledges, leases: leases, oauth: oauth,
	}, ledger: ledger}
}

func (f *rvaFixture) dump(t *testing.T, runID, label string) {
	t.Helper()
	all, _ := f.leases.ListByRun(context.Background(), runID)
	for _, l := range all {
		t.Logf("%s: %.8s %-36s acq=%s closed=%-5v outcome=%-20q cost=%.2f", label, l.ID, l.PledgeID,
			l.AcquiredAt.Format("15:04:05.000000"), l.Closed, l.Outcome, l.CostUSD)
	}
}

func (f *rvaFixture) spent(t *testing.T, pledgeID string) float64 {
	t.Helper()
	day, _, err := f.ledger.Usage(context.Background(), pledgeID, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	return day.CostUSD
}

// F2 through the REAL attached walk: the gateway entry (a URL-less legacy
// row) is acquired LAST and released; the attempt's report never reaches
// the sealed forfait lease.
func TestAttachedReport_F2_publisher_releasedGatewaySwallowsTheForfaitReport(t *testing.T) {
	f := newRVAFixture(t, "")
	ctx := context.Background()
	_, creds := f.resolveAttached(t, "run-f2", nil, frozenAttachment("pool-att"))
	t.Logf("sealed grants=%d released=%d", len(creds.grants), len(creds.releasedGrants))
	if len(creds.grants) != 1 || len(creds.releasedGrants) != 1 {
		t.Fatalf("setup: want the forfait sealed and the URL-less gateway released")
	}
	time.Sleep(3 * time.Millisecond) // the bundle seal, the doc save, the publish
	pub := publishInstant(time.Now().UTC(), time.Time{}, creds.poolLease())
	f.dump(t, "run-f2", "before report")
	if err := f.pub.credPool.ReportAttempt(ctx, "run-f2", pub, credpool.Outcome{CostUSD: 2}); err != nil {
		t.Fatalf("report: %v", err)
	}
	f.dump(t, "run-f2", "after the run's final report")
	forfait := credpool.PledgeID("donor", credpool.SourceOAuth, "claude_code")
	open := openLeases(t, f.attachedFixture, "run-f2")
	t.Logf("open leases after the final report = %d (want 0); forfait donor charged $%.2f (want 2)", len(open), f.spent(t, forfait))
	if len(open) != 0 || f.spent(t, forfait) != 2 {
		t.Errorf("the forfait lease leaked past the run's report")
	}
}

// F1 through the REAL attached walk: A1 seals forfait+gateway, a resume
// (A2) takes over inside A1's teardown, A1's late report lands.
func TestAttachedReport_F1_publisher_lateReportClosesTheResumesLeases(t *testing.T) {
	f := newRVAFixture(t, "https://gw.example")
	ctx := context.Background()
	_, c1 := f.resolveAttached(t, "run-f1", nil, frozenAttachment("pool-att"))
	if len(c1.grants) != 2 {
		t.Fatalf("A1 sealed %d grants, want 2", len(c1.grants))
	}
	time.Sleep(3 * time.Millisecond)
	p1 := publishInstant(time.Now().UTC(), time.Time{}, c1.poolLease())
	time.Sleep(3 * time.Millisecond)
	_, c2 := f.resolveAttached(t, "run-f1", nil, frozenAttachment("pool-att"))
	if len(c2.grants) != 2 {
		t.Fatalf("A2 sealed %d grants, want 2", len(c2.grants))
	}
	time.Sleep(3 * time.Millisecond)
	p2 := publishInstant(time.Now().UTC(), time.Time{}, c2.poolLease())
	f.dump(t, "run-f1", "after the resume")

	forfaitFP := seededFP("donor")
	gwFP := secrets.APIKeyFingerprint(secrets.ProviderOpenAICompatible, "sk-gw-0000", "https://gw.example")
	if err := f.pub.credPool.ReportAttempt(ctx, "run-f1", p1, credpool.Outcome{CostUSD: 3, ByFingerprint: []credpool.FingerprintSpend{
		{Fingerprint: forfaitFP, CostUSD: 1}, {Fingerprint: gwFP, CostUSD: 2}}}); err != nil {
		t.Fatalf("A1 report: %v", err)
	}
	f.dump(t, "run-f1", "after A1's late report")
	openWhileA2Runs := len(openLeases(t, f.attachedFixture, "run-f1"))
	if err := f.pub.credPool.ReportAttempt(ctx, "run-f1", p2, credpool.Outcome{CostUSD: 10, ByFingerprint: []credpool.FingerprintSpend{
		{Fingerprint: forfaitFP, CostUSD: 4}, {Fingerprint: gwFP, CostUSD: 6}}}); err != nil {
		t.Fatalf("A2 report: %v", err)
	}
	f.dump(t, "run-f1", "after A2's report")
	forfait := credpool.PledgeID("donor", credpool.SourceOAuth, "claude_code")
	gw := credpool.PledgeID("gw-lender", credpool.SourceAPIKey, string(secrets.ProviderOpenAICompatible))
	t.Logf("A2's open leases while it still runs = %d (want 2); ledgers forfait=$%.2f (want 5) gateway=$%.2f (want 8)",
		openWhileA2Runs, f.spent(t, forfait), f.spent(t, gw))
	if openWhileA2Runs != 2 || f.spent(t, forfait) != 5 || f.spent(t, gw) != 8 {
		t.Errorf("A1's late report corrupted A2's leases / the ledgers")
	}
	_ = store.PublishAt
}

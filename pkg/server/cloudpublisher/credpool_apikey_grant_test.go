package cloudpublisher

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// #659 pt 1: a METERED donor key granted by the pool used to reach the
// GRANTED line as `byok(api_key:xai fp=<unstamped>)` — the api-key fill
// site never stamped the fingerprint the OAuth site does — and the same
// omission kept it off the run document's fingerprints, so the per-key
// concurrency meter could not count the run and the metering-time
// last_used_at bump had nothing to key on. The oracle is the real
// broker over a real sealed key: the line names the tier and the hash,
// the run-doc fingerprints carry it, the bundle carries the key.
func TestPoolTier_apiKeyGrantIsStampedAndLabelled(t *testing.T) {
	ctx := context.Background()
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	// The donor's PERSONAL xai key, sealed the way the BYOK route seals it.
	apiKeys := secrets.NewMemoryApiKeyStore()
	keyID := secrets.NewApiKeyID()
	sealed, err := secrets.SealAPIKey(sealer, keyID, []byte("xai-donated-key"))
	if err != nil {
		t.Fatal(err)
	}
	wantFP := secrets.FingerprintSHA256("xai-donated-key")
	if err := apiKeys.Create(store.WithTenant(ctx, "donor-team"), secrets.ApiKey{
		ID: keyID, TenantID: "donor-team", ScopeTeamID: "donor-team", ScopeUserID: "donor",
		Provider: secrets.ProviderXAI, Name: "lent", SealedSecret: sealed, Fingerprint: wantFP,
	}); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	pools := credpool.NewMemoryPoolStore()
	if err := pools.Upsert(ctx, credpool.Pool{ID: "pool-1", OrgID: poolOrg, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	pledges := credpool.NewMemoryPledgeStore()
	if err := pledges.Upsert(ctx, credpool.Pledge{
		ID: credpool.PledgeID("donor", credpool.SourceAPIKey, "xai"), PoolID: "pool-1", UserID: "donor",
		Credential: credpool.Credential{Source: credpool.SourceAPIKey, Ref: "xai", KeyID: keyID},
		Enabled:    true, Health: credpool.HealthOK, Limits: credpool.Limits{MaxUSDPerDay: 5},
	}); err != nil {
		t.Fatalf("seed pledge: %v", err)
	}
	broker := credpool.NewBroker(credpool.BrokerConfig{
		Pools: pools, Pledges: pledges, Leases: credpool.NewMemoryLeaseStore(), Ledger: credpool.NewMemoryLedger(),
		OAuth: secrets.NewMemoryOAuthStore(), APIKeys: apiKeys, Sealer: sealer, Logger: testLogger(),
	})
	if broker == nil {
		t.Fatal("broker is nil with every dependency wired")
	}
	var buf bytes.Buffer
	rs := secrets.NewMemoryRunSecretsStore()
	p := &Publisher{runSecrets: rs, sealer: sealer, credPool: broker, logger: iterlog.New(iterlog.LevelInfo, &buf)}

	wf := &ir.Workflow{Nodes: map[string]ir.Node{"a": &ir.AgentNode{
		BaseNode: ir.BaseNode{ID: "a"}, LLMFields: ir.LLMFields{Backend: "claw", Provider: "xai", Model: "xai/grok-4"},
	}}}
	creds, err := p.resolveAndSealCredentials(store.WithTenant(ctx, poolTeam), "run-xai", poolOrg, poolTeam, "requester", "bot", wf, nil, nil, model.ModelOverrides{}, nil)
	if err != nil {
		t.Fatalf("resolveAndSealCredentials: %v", err)
	}
	if creds.grant == nil || creds.grant.Source != credpool.SourceAPIKey {
		t.Fatalf("want an api_key grant, got %+v; log:\n%s", creds.grant, buf.String())
	}
	log := buf.String()
	if !strings.Contains(log, "pool(api_key:xai fp="+wantFP+")") {
		t.Fatalf("GRANTED line must name the pool tier and the donor key's fingerprint; got:\n%s", log)
	}
	if strings.Contains(log, "<unstamped>") || strings.Contains(log, "xai-donated-key") {
		t.Fatalf("GRANTED line is unstamped or leaks the key:\n%s", log)
	}
	found := false
	for _, fp := range creds.fingerprints {
		if fp == wantFP {
			found = true
		}
	}
	if !found {
		t.Fatalf("run-doc fingerprints = %v, want the lent key's %s", creds.fingerprints, wantFP)
	}
	rec, err := rs.Get(ctx, creds.secretsRef)
	if err != nil {
		t.Fatalf("RunSecrets.Get: %v", err)
	}
	bundle, err := secrets.OpenRunBundle(sealer, "run-xai", rec.SealedBundle)
	if err != nil {
		t.Fatalf("OpenRunBundle: %v", err)
	}
	if bundle.APIKeys[secrets.ProviderXAI] != "xai-donated-key" {
		t.Fatalf("bundle xai slot = %q, want the lent key", bundle.APIKeys[secrets.ProviderXAI])
	}
}

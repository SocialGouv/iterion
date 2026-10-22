package cloudpublisher

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/llmroute"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Context for (b): the PRE-EXISTING whitelist refusal the gateway refusal
// copies — does an unsealed grant keep its lease?
func TestRVA_WhitelistRefusedPoolGrantIsReleased(t *testing.T) {
	ctx := context.Background()
	sealer, _ := secrets.NewKeyRingSealer(map[string][]byte{"default": make([]byte, 32)}, "default")
	apiKeys := secrets.NewMemoryApiKeyStore()
	keyID := secrets.NewApiKeyID()
	sealed, _ := secrets.SealAPIKey(sealer, keyID, []byte("xai-donated-key"))
	_ = apiKeys.Create(store.WithTenant(ctx, "donor-team"), secrets.ApiKey{
		ID: keyID, TenantID: "donor-team", ScopeTeamID: "donor-team", ScopeUserID: "donor",
		Provider: secrets.ProviderXAI, Name: "lent", SealedSecret: sealed, Fingerprint: secrets.FingerprintSHA256("xai-donated-key"),
	})
	pools := credpool.NewMemoryPoolStore()
	_ = pools.Upsert(ctx, credpool.Pool{ID: "pool-1", OrgID: poolOrg, Enabled: true})
	pledges := credpool.NewMemoryPledgeStore()
	_ = pledges.Upsert(ctx, credpool.Pledge{
		ID: credpool.PledgeID("donor", credpool.SourceAPIKey, "xai"), PoolID: "pool-1", UserID: "donor",
		Credential: credpool.Credential{Source: credpool.SourceAPIKey, Ref: "xai", KeyID: keyID},
		Enabled:    true, Health: credpool.HealthOK, Limits: credpool.Limits{MaxUSDPerDay: 5},
	})
	leases := credpool.NewMemoryLeaseStore()
	broker := credpool.NewBroker(credpool.BrokerConfig{
		Pools: pools, Pledges: pledges, Leases: leases, Ledger: credpool.NewMemoryLedger(),
		OAuth: secrets.NewMemoryOAuthStore(), APIKeys: apiKeys, Sealer: sealer, Logger: testLogger(),
	})
	rs := secrets.NewMemoryRunSecretsStore()
	p := &Publisher{runSecrets: rs, sealer: sealer, credPool: broker, logger: testLogger()}
	wf := &ir.Workflow{Nodes: map[string]ir.Node{"a": &ir.AgentNode{
		BaseNode: ir.BaseNode{ID: "a"}, LLMFields: ir.LLMFields{Backend: "claw", Provider: "xai", Model: "xai/grok-4"},
	}}}
	policy := &store.RunLLMRoutePolicy{
		PairOrder: []string{llmroute.Pair(llmroute.HarnessClaudeCode, llmroute.CredClaudeForfait)},
		Triggers:  []string{llmroute.TriggerUsageWindow},
	}
	creds, err := p.resolveAndSealCredentials(store.WithTenant(ctx, poolTeam), "run-pw", poolOrg, poolTeam, "requester", "bot", "", wf, nil, nil, model.RouteView{Workflow: wf}, nil, store.RunTrustDefault, nil, policy)
	if err != nil {
		t.Fatal(err)
	}
	open, _ := leases.ListOpenByRun(ctx, "run-pw")
	t.Logf("WHITELIST-REFUSED: grant!=nil=%v poolLease!=nil=%v open_leases=%d secretsRef=%q stampFPs=%v tiers=%v",
		creds.grant != nil, creds.poolLease() != nil, len(open), creds.secretsRef, creds.fingerprints, creds.tiers)
	// The refused grant is RELEASED, not merely unsealed: no lease, no
	// sealed bundle, nothing of the donor's identity on the run.
	if creds.grant != nil || creds.poolLease() != nil {
		t.Fatalf("a whitelist-refused grant was retained: grant=%v lease=%v", creds.grant != nil, creds.poolLease() != nil)
	}
	if len(open) != 0 {
		t.Fatalf("a whitelist-refused grant still holds %d lease(s)", len(open))
	}
	if creds.secretsRef != "" {
		t.Fatalf("a whitelist-refused run sealed a bundle: %q", creds.secretsRef)
	}
	donorFP := secrets.FingerprintSHA256("xai-donated-key")
	for prov, fp := range creds.fingerprints {
		if fp == donorFP {
			t.Fatalf("the unsealed donor's fingerprint %q (%v) is stamped on the run", fp, prov)
		}
	}
}

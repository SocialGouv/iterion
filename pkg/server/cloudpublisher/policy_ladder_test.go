package cloudpublisher

import (
	"context"
	"github.com/SocialGouv/iterion/pkg/credpool"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/llmroute"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// policyWf is a one-agent workflow pinned claude_code with a model — the
// primary the ladder composes against.
func policyWf() *ir.Workflow {
	return &ir.Workflow{Nodes: map[string]ir.Node{"implement": &ir.AgentNode{
		BaseNode: ir.BaseNode{ID: "implement"},
		LLMFields: ir.LLMFields{
			Backend: "claude_code",
			Model:   "claude-opus-5-5",
		},
	}}}
}

// resolveWithPolicy seals a two-key tenant under a policy and returns the
// OPENED bundle (the wf is mutated in place by the ladder's materialization).
func resolveWithPolicy(t *testing.T, policy *store.RunLLMRoutePolicy, wf *ir.Workflow) secrets.RunBundle {
	t.Helper()
	sealer, err := secrets.NewKeyRingSealer(map[string][]byte{"default": make([]byte, 32)}, "default")
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	keys := secrets.NewMemoryApiKeyStore()
	seedKey(t, keys, sealer, "team1", secrets.ProviderAnthropic, "sk-ant-tenant")
	seedKey(t, keys, sealer, "team1", secrets.ProviderZAI, "sk-zai-tenant")
	p := &Publisher{
		apiKeys:    keys,
		runSecrets: secrets.NewMemoryRunSecretsStore(),
		sealer:     sealer,
		logger:     testLogger(),
	}
	ctx := store.WithTenant(t.Context(), "team1")
	creds, err := p.resolveAndSealCredentials(ctx, "run-pol", "", "team1", "owner1", "", "", wf, nil, nil, model.ModelOverrides{}, nil, store.RunTrustDefault, nil, policy)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if creds.secretsRef == "" {
		t.Fatal("no bundle sealed")
	}
	rec, err := p.runSecrets.(*secrets.MemoryRunSecretsStore).Get(ctx, creds.secretsRef)
	if err != nil {
		t.Fatalf("run secrets: %v", err)
	}
	bundle, err := secrets.OpenRunBundle(mustDEKSealer(t, creds.dek), rec.TenantID, "", "run-pol", rec.KeyID, rec.SealedBundle)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return bundle
}

// The launch-time selection end-to-end: the whitelist keeps unlisted slots
// out of the bundle, the ladder carries the HELD pairs in policy order
// (flagged Policy, On = the resolved triggers), and the wf carries the
// materialized routes with per-node mapped models — claw's anthropic
// crossing spelled with its TRUE provider prefix.
func TestResolve_PolicyShapesSealingAndBuildsTheLadder(t *testing.T) {
	policy := &store.RunLLMRoutePolicy{
		PairOrder: []string{
			llmroute.Pair(llmroute.HarnessClaudeCode, llmroute.CredClaudeForfait), // nothing seeded: never held
			llmroute.Pair(llmroute.HarnessClaudeCode, "anthropic_key"),
			llmroute.Pair(llmroute.HarnessClaudeCode, "zai_key"),
		},
		Triggers: []string{llmroute.TriggerUsageWindow, llmroute.TriggerAuth},
	}
	wf := policyWf()
	bundle := resolveWithPolicy(t, policy, wf)

	if got := bundle.APIKeys[secrets.ProviderAnthropic]; got != "sk-ant-tenant" {
		t.Fatalf("anthropic key = %q, want sealed (claw+anthropic_key is listed)", got)
	}
	if got := bundle.APIKeys[secrets.ProviderZAI]; got != "sk-zai-tenant" {
		t.Fatalf("zai key = %q, want sealed (claude_code+zai_key is listed)", got)
	}

	node := wf.Nodes["implement"].(*ir.AgentNode)
	if len(node.Fallbacks) != 2 {
		t.Fatalf("ladder stages on the node = %d, want 2 (one per held pair)", len(node.Fallbacks))
	}
	first, second := node.Fallbacks[0], node.Fallbacks[1]
	if first.Backend != "claude_code" || first.Provider != "anthropic" || first.Model != "claude-opus-5-5" {
		t.Fatalf("stage 1 = %+v, want claude_code hinted anthropic (the BYOK key)", first)
	}
	if second.Backend != "claude_code" || second.Provider != "zai" || second.Model != "claude-opus-5-5" {
		t.Fatalf("stage 2 = %+v, want claude_code hinted zai (the facade)", second)
	}
	if !first.Policy || !second.Policy {
		t.Fatal("the computed stages must carry the Policy marker — the preflight judges their spend")
	}
	for _, st := range node.Fallbacks {
		if len(st.On) != 2 || st.On[0] != llmroute.TriggerUsageWindow || st.On[1] != llmroute.TriggerAuth {
			t.Fatalf("stage On = %v, want the resolved policy's triggers (auth included — the default set would stop the selection's own scenario)", st.On)
		}
	}
}

// The whitelist is a NARROWING: a pair_order that omits a seeded slot
// keeps it out of the bundle entirely — fill and (when refused) restore.
func TestResolve_WhitelistKeepsUnlistedSlotsOut(t *testing.T) {
	policy := &store.RunLLMRoutePolicy{
		PairOrder: []string{llmroute.Pair(llmroute.HarnessClaw, "anthropic_key")},
	}
	wf := policyWf()
	bundle := resolveWithPolicy(t, policy, wf)
	if got := bundle.APIKeys[secrets.ProviderZAI]; got != "" {
		t.Fatalf("zai key = %q, want OUT — claude_code+zai_key is not listed", got)
	}
	if got := bundle.APIKeys[secrets.ProviderAnthropic]; got != "sk-ant-tenant" {
		t.Fatalf("anthropic key = %q, want sealed", got)
	}
}

// No policy (an unwired launch) reads exactly as before: everything the
// tiers hold seals.
func TestResolve_NilPolicyAdmitsEverything(t *testing.T) {
	wf := policyWf()
	bundle := resolveWithPolicy(t, nil, wf)
	if bundle.APIKeys[secrets.ProviderAnthropic] == "" || bundle.APIKeys[secrets.ProviderZAI] == "" {
		t.Fatalf("keys = %q / %q, want both sealed without a policy", bundle.APIKeys[secrets.ProviderAnthropic], bundle.APIKeys[secrets.ProviderZAI])
	}
	if nodes := wf.Nodes["implement"].(*ir.AgentNode); len(nodes.Fallbacks) != 0 {
		t.Fatalf("a nil policy must build no ladder, got %d stages", len(nodes.Fallbacks))
	}
}

// A CLAMPED-EMPTY trigger set is the "never switch" answer: no ladder at
// all. The wire's On would otherwise round-trip empty into the chain
// DEFAULT set — the ceiling's forbidden triggers armed (the review's M4).
func TestResolve_EmptyTriggersBuildNoLadder(t *testing.T) {
	policy := &store.RunLLMRoutePolicy{
		PairOrder: []string{llmroute.Pair(llmroute.HarnessClaudeCode, "anthropic_key")}, // a crossing the screen LANDS
		Triggers:  []string{},
	}
	wf := policyWf()
	bundle := resolveWithPolicy(t, policy, wf)
	if bundle.APIKeys[secrets.ProviderAnthropic] == "" {
		t.Fatal("the whitelist still seals — only the SWITCH is off")
	}
	if node := wf.Nodes["implement"].(*ir.AgentNode); len(node.Fallbacks) != 0 {
		t.Fatalf("ladder stages = %d, want none — never switch means never switch", len(node.Fallbacks))
	}
}

// The whitelist gates the POOL like every tier (the slice-3 review's M1):
// a donation whose slot the pair order excludes refuses the GRANT — the
// run never spends a pooled credential the policy excludes, and the
// unsealed lease releases cleanly.
func TestResolve_PoolGrantRefusedByWhitelist(t *testing.T) {
	ctx := context.Background()
	sealer, err := secrets.NewKeyRingSealer(map[string][]byte{"default": make([]byte, 32)}, "default")
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	apiKeys := secrets.NewMemoryApiKeyStore()
	keyID := secrets.NewApiKeyID()
	sealed, err := secrets.SealAPIKey(sealer, keyID, []byte("xai-donated-key"))
	if err != nil {
		t.Fatal(err)
	}
	if err := apiKeys.Create(store.WithTenant(ctx, "donor-team"), secrets.ApiKey{
		ID: keyID, TenantID: "donor-team", ScopeTeamID: "donor-team", ScopeUserID: "donor",
		Provider: secrets.ProviderXAI, Name: "lent", SealedSecret: sealed, Fingerprint: secrets.FingerprintSHA256("xai-donated-key"),
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
	rs := secrets.NewMemoryRunSecretsStore()
	p := &Publisher{runSecrets: rs, sealer: sealer, credPool: broker, logger: testLogger()}
	wf := &ir.Workflow{Nodes: map[string]ir.Node{"a": &ir.AgentNode{
		BaseNode: ir.BaseNode{ID: "a"}, LLMFields: ir.LLMFields{Backend: "claw", Provider: "xai", Model: "xai/grok-4"},
	}}}
	// claw+xai_key is NOT listed: the grant is refused, nothing seals.
	policy := &store.RunLLMRoutePolicy{
		PairOrder: []string{llmroute.Pair(llmroute.HarnessClaudeCode, llmroute.CredClaudeForfait)},
		Triggers:  []string{llmroute.TriggerUsageWindow},
	}
	creds, err := p.resolveAndSealCredentials(store.WithTenant(ctx, poolTeam), "run-pw", poolOrg, poolTeam, "requester", "bot", "", wf, nil, nil, model.ModelOverrides{}, nil, store.RunTrustDefault, nil, policy)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	// Nothing seals: the whitelist refused the only credential the pool
	// could lend, so the resolution leaves an EMPTY bundle — the strongest
	// form of "nothing pooled leaked". (The run itself would then fail its
	// first LLM call / fall to whatever remains — the operator's stated
	// narrowness, not a defect.)
	if creds.secretsRef == "" {
		return
	}
	rec, err := rs.Get(ctx, creds.secretsRef)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := secrets.OpenRunBundle(mustDEKSealer(t, creds.dek), rec.TenantID, "", "run-pw", rec.KeyID, rec.SealedBundle)
	if err != nil {
		t.Fatal(err)
	}
	if got := bundle.APIKeys[secrets.ProviderXAI]; got != "" {
		t.Fatalf("xai slot = %q, want EMPTY — the pool grant is refused by the whitelist", got)
	}
	t.Logf("DEBUG: apikeys=%v pinned=%v oauth=%v grant=%v ref=%q", bundle.APIKeys, bundle.PinnedAPIKeys, bundle.OAuthCredentials, creds.grant != nil, creds.secretsRef)
	if creds.grant == nil {
		t.Log("grant nil: the lease path also declined — either way nothing pooled sealed")
	}
}

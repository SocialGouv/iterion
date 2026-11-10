package cloudpublisher

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/llmroute"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// varsGatewayWorkflow is mixedGatewayWorkflow with the gateway node's model
// written `{{vars.model}}`, the var defaulting to the gateway route — the
// documented spelling (bots/feed-watch, examples/clarify). review carries an
// authored route when reviewRoute is set, so the gateway node is the only
// one the ladder may reach.
func varsGatewayWorkflow(reviewRoute bool) *ir.Workflow {
	review := &ir.AgentNode{BaseNode: ir.BaseNode{ID: "review"}, LLMFields: ir.LLMFields{Backend: "claw", Model: "anthropic/claude-opus-5"}, Tools: []string{}}
	if reviewRoute {
		review.Fallbacks = []ir.Fallback{{Name: "haiku", Backend: "claw", Model: "anthropic/claude-haiku-4-5"}}
	}
	return &ir.Workflow{
		Vars: map[string]*ir.Var{"model": {Name: "model", Type: ir.VarString, HasDefault: true, Default: "openai_compatible/team/m"}},
		Nodes: map[string]ir.Node{
			"gw":     &ir.AgentNode{BaseNode: ir.BaseNode{ID: "gw"}, LLMFields: ir.LLMFields{Backend: "claw", Model: "{{vars.model}}"}, Tools: []string{}},
			"review": review,
		},
	}
}

// ADR-131 §4 (§6-ter n°13): the publisher's ladder composer reads a
// `{{vars.model}}` node by the route the run's vars decide — the walk's own
// reading, which attributes it and stamps team_gateway — so the gateway node
// takes no vendor crossing, the refusal is named, and the vendor node of the
// mixed run keeps its rung.
func TestContentBoundary_TheLadderReadsAVarsTemplatedGatewayNodeByItsRoute(t *testing.T) {
	p, buf := gatewayKeyPublisher(t)
	seedGatewayKey(t, p.apiKeys, p.sealer, "team1", nil)
	seedKey(t, p.apiKeys, p.sealer, "team1", secrets.ProviderOpenAI, "sk-openai-team")
	wf := varsGatewayWorkflow(false)
	view := model.RouteViewForLaunch(wf, model.ModelOverrides{}, nil, map[string]string(nil), nil)
	creds, err := p.resolveAndSealCredentials(gatewayLaunchCtx(), "run-cb-vars", "", "team1", "owner1", gwBot, "", wf, nil, nil, view, nil, store.RunTrustDefault, nil, ladderPolicy(), nil, boundaryDerived)
	if err != nil {
		t.Fatalf("resolveAndSealCredentials: %v", err)
	}
	if !creds.contentBoundary.TeamGateway() {
		t.Fatalf("fixture: content boundary = %+v, want team_gateway — the walk attributes the var's gateway route", creds.contentBoundary)
	}
	if !policyCrossing(wf, "review") {
		t.Fatalf("the vendor node lost its crossing (log:\n%s)", buf.String())
	}
	if policyCrossing(wf, "gw") {
		t.Fatalf("gateway node rungs = %+v, want none: the run's vars route it to the team's gateway (log:\n%s)", wf.Nodes["gw"].(*ir.AgentNode).Fallbacks, buf.String())
	}
	if !strings.Contains(buf.String(), llmroute.ReasonContentBoundary) {
		t.Errorf("the refusal is not named:\n%s", buf.String())
	}
}

// The door's probe screens through the same reading: under team_gateway no
// donor is asked — on the launch nor in the preview — for a kind only the
// `{{vars.model}}` gateway node's crossing would carry.
func TestContentBoundary_TheFallbackDoorReadsAVarsTemplatedGatewayNodeByItsRoute(t *testing.T) {
	sealer, err := secrets.NewKeyRingSealer(map[string][]byte{"default": make([]byte, 32)}, "default")
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	ctx := context.Background()
	keys := secrets.NewMemoryApiKeyStore()
	seedGatewayKey(t, keys, sealer, poolTeam, nil)
	seedKey(t, keys, sealer, poolTeam, secrets.ProviderAnthropic, "sk-ant-team")
	donorKeys := secrets.NewMemoryApiKeyStore()
	keyID := secrets.NewApiKeyID()
	sealed, err := secrets.SealAPIKey(sealer, keyID, []byte("sk-openai-donated"))
	if err != nil {
		t.Fatal(err)
	}
	if err := donorKeys.Create(store.WithTenant(ctx, "donor-team"), secrets.ApiKey{
		ID: keyID, TenantID: "donor-team", ScopeTeamID: "donor-team", ScopeUserID: "donor",
		Provider: secrets.ProviderOpenAI, Name: "lent", SealedSecret: sealed, Fingerprint: secrets.FingerprintSHA256("sk-openai-donated"),
	}); err != nil {
		t.Fatalf("seed donor key: %v", err)
	}
	pools := credpool.NewMemoryPoolStore()
	if err := pools.Upsert(ctx, credpool.Pool{ID: "pool-1", OrgID: poolOrg, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	pledges := credpool.NewMemoryPledgeStore()
	if err := pledges.Upsert(ctx, credpool.Pledge{
		ID: credpool.PledgeID("donor", credpool.SourceAPIKey, "openai"), PoolID: "pool-1", UserID: "donor",
		Credential: credpool.Credential{Source: credpool.SourceAPIKey, Ref: "openai", KeyID: keyID},
		Enabled:    true, Health: credpool.HealthOK, Limits: credpool.Limits{MaxUSDPerDay: 5}, FallbackUse: true,
	}); err != nil {
		t.Fatalf("seed pledge: %v", err)
	}
	broker := credpool.NewBroker(credpool.BrokerConfig{
		Pools: pools, Pledges: pledges, Leases: credpool.NewMemoryLeaseStore(), Ledger: credpool.NewMemoryLedger(),
		OAuth: secrets.NewMemoryOAuthStore(), APIKeys: donorKeys, Sealer: sealer, Logger: testLogger(),
	})
	var buf bytes.Buffer
	p := &Publisher{apiKeys: keys, runSecrets: secrets.NewMemoryRunSecretsStore(), sealer: sealer, credPool: broker, logger: iterlog.New(iterlog.LevelInfo, &buf),
		identity: &fakeTeamResolver{orgs: map[string]string{poolTeam: poolOrg}, orgDocs: map[string]identity.Org{poolOrg: {ID: poolOrg}}}}
	tctx := secrets.WithLaunchRepo(store.WithTenant(ctx, poolTeam), gwRepo)
	launchWF := varsGatewayWorkflow(true)
	view := model.RouteViewForLaunch(launchWF, model.ModelOverrides{}, nil, map[string]string(nil), nil)
	creds, err := p.resolveAndSealCredentials(tctx, "run-cb-vars-door", poolOrg, poolTeam, "owner1", gwBot, "", launchWF, nil, nil, view, nil, store.RunTrustDefault, nil, ladderPolicy(), nil, boundaryDerived)
	if err != nil {
		t.Fatalf("resolveAndSealCredentials: %v", err)
	}
	if !creds.contentBoundary.TeamGateway() {
		t.Fatalf("fixture: content boundary = %+v, want team_gateway", creds.contentBoundary)
	}
	b := bundleFrom(t, p, tctx, creds, "run-cb-vars-door")
	if creds.doorGrant != nil || b.APIKeys[secrets.ProviderOpenAI] != "" {
		t.Fatalf("door grant = %v, openai slot sealed = %v — a donor's key lent to a team_gateway run for its gateway node (log:\n%s)", creds.doorGrant, b.APIKeys[secrets.ProviderOpenAI] != "", buf.String())
	}
	spec := previewSpec(poolTeam, "owner1")
	spec.LLMRoutePolicy = ladderPolicy()
	spec.Context.BotID = gwBot
	preview, err := previewReadOnly(p).PreviewCredentials(tctx, spec, varsGatewayWorkflow(true))
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if slices.Contains(preview.Pool.DoorWants, "api_key:openai") {
		t.Fatalf("preview door wants = %v (reason %q) — the preview asks a donor the launch must not", preview.Pool.DoorWants, preview.Pool.DoorReason)
	}
}

// ADR-131 §4, the stamp producer (#2005 PR-C'): a launch's walk decides the
// run's content boundary — team_gateway when it ATTRIBUTES a gateway route to
// a key and the gateway key sealed (a mixed run included), an explicit
// own_keys otherwise — and the decision reaches its three carriages: the run
// document (applyTo), the bundle's AmbientLLMRefused (ORed with ADR-162's
// untrusted arm), and the policy ladder's screen with the fallback door's
// probe (a node routed to the team's gateway takes no crossing to a vendor
// credential; the vendor nodes of a mixed run keep theirs). A resume replays
// the frozen stamp (apikey_skip_test.go).

package cloudpublisher

import (
	"bytes"
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/llmroute"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
)

// gwHost is gwURL's host, the one the runner pins on the run's proxy.
const gwHost = "gw.example"

// gatewayOnlyWorkflow is a run whose one route is a decided gateway route —
// envFundedWorkflow's shape, built fresh: the resolution lands rungs on the
// program it reads (the ladder, an attached pool's entry), and a package
// fixture would carry them into every later test.
func gatewayOnlyWorkflow() *ir.Workflow {
	return &ir.Workflow{Nodes: map[string]ir.Node{
		"implement": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "implement"}, LLMFields: ir.LLMFields{Backend: "claw", Model: "openai_compatible/team/m"}},
	}}
}

// mixedGatewayWorkflow is a mixed run: one node routed to the gateway, one to
// a vendor, neither with routes of its own (both eligible for the ladder).
func mixedGatewayWorkflow() *ir.Workflow {
	return &ir.Workflow{Nodes: map[string]ir.Node{
		"gw":     &ir.AgentNode{BaseNode: ir.BaseNode{ID: "gw"}, LLMFields: ir.LLMFields{Backend: "claw", Model: "openai_compatible/team/m"}, Tools: []string{}},
		"review": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "review"}, LLMFields: ir.LLMFields{Backend: "claw", Model: "anthropic/claude-opus-5"}, Tools: []string{}},
	}}
}

// resolveBoundaryRun is resolveGatewayRun with the org and the trust the row
// needs.
func resolveBoundaryRun(t *testing.T, p *Publisher, runID, orgID string, wf *ir.Workflow, trust store.RunTrust, policy *store.RunLLMRoutePolicy) credResolution {
	t.Helper()
	creds, err := p.resolveAndSealCredentials(gatewayLaunchCtx(), runID, orgID, "team1", "owner1", gwBot, "", wf, nil, nil, model.RouteView{Workflow: wf}, nil, trust, nil, policy, nil, boundaryDerived)
	if err != nil {
		t.Fatalf("resolveAndSealCredentials: %v", err)
	}
	return creds
}

func ownKeys() *store.RunContentBoundary {
	return &store.RunContentBoundary{Kind: store.ContentBoundaryOwnKeys}
}

// B1: a decided gateway route the team's own key funds stamps team_gateway,
// naming the key row that filled the slot and the host its sealed URL dials;
// applyTo writes it on the launch's document; and the bundle carries the
// ambient refusal on a TRUSTED run — the same decision, its second carriage.
func TestContentBoundary_ADecidedGatewayRouteTheTeamFundsIsTeamGateway(t *testing.T) {
	p, buf := gatewayKeyPublisher(t)
	var keyID string
	seedGatewayKey(t, p.apiKeys, p.sealer, "team1", func(k *secrets.ApiKey) { keyID = k.ID })
	creds := resolveBoundaryRun(t, p, "run-cb-1", "", gatewayOnlyWorkflow(), store.RunTrustDefault, nil)
	b := openGatewayBundle(t, p, "run-cb-1", creds)
	if b.APIKeys[secrets.ProviderOpenAICompatible] != gwTeamKey {
		t.Fatalf("the fixture sealed no gateway key, so the stamp below proves nothing (log:\n%s)", buf.String())
	}
	want := &store.RunContentBoundary{Kind: store.ContentBoundaryTeamGateway, KeyID: keyID, GatewayHost: gwHost}
	if !reflect.DeepEqual(creds.contentBoundary, want) {
		t.Fatalf("content boundary = %+v, want %+v", creds.contentBoundary, want)
	}
	if !b.AmbientLLMRefused {
		t.Errorf("a trusted run held to its team's gateway seals no ambient refusal: its env fallbacks would serve the operator's gateway")
	}
	var r store.Run
	creds.applyTo(&r)
	if !reflect.DeepEqual(r.ContentBoundary, want) {
		t.Errorf("applyTo wrote %+v on the launch's document, want %+v", r.ContentBoundary, want)
	}
}

// B2: every other launch the walk resolves stamps own_keys EXPLICITLY, never
// nil (arbitration 2) — a key sealed for an env-templated dial (attributed to
// nothing, ADR-129 §4), a gateway route attributed to a key the run's frozen
// pair order holds out (attributed, never sealed: both facts are owed), a
// vendor-only run, and a run whose gateway routes ride the pod's environment
// — and none of them carries the ambient refusal.
func TestContentBoundary_EveryOtherWalkedLaunchStampsOwnKeys(t *testing.T) {
	dial := &ir.Workflow{Nodes: map[string]ir.Node{
		"review": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "review"}, LLMFields: ir.LLMFields{Backend: "claw", Model: "${C4_UNSET_ROUTE_DIAL:-openai_compatible/scaleway/deepseek-v4.1-flash}"}},
	}}
	vendor := &ir.Workflow{Nodes: map[string]ir.Node{
		"review": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "review"}, LLMFields: ir.LLMFields{Backend: "claw", Model: "anthropic/claude-sonnet-4-5"}},
	}}
	for _, row := range []struct {
		name      string
		wf        *ir.Workflow
		policy    *store.RunLLMRoutePolicy
		seed      func(*Publisher)
		gwSealed  bool
		anySealed bool
	}{
		{"a key sealed for an env-templated gateway dial", dial, nil, func(p *Publisher) { seedGatewayKey(t, p.apiKeys, p.sealer, "team1", nil) }, true, true},
		{"a gateway route attributed to a key the frozen pair order holds out", gatewayOnlyWorkflow(), preUpgradePolicy(), func(p *Publisher) { seedGatewayKey(t, p.apiKeys, p.sealer, "team1", nil) }, false, false},
		{"a vendor-only run", vendor, nil, func(p *Publisher) {
			seedGatewayKey(t, p.apiKeys, p.sealer, "team1", nil)
			seedKey(t, p.apiKeys, p.sealer, "team1", secrets.ProviderAnthropic, "sk-ant-team")
		}, false, true},
		{"a gateway run the pod's environment funds", gatewayOnlyWorkflow(), nil, func(*Publisher) {}, false, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			p, buf := gatewayKeyPublisher(t)
			row.seed(p)
			creds := resolveBoundaryRun(t, p, "run-cb-2", "", row.wf, store.RunTrustDefault, row.policy)
			b := openGatewayBundle(t, p, "run-cb-2", creds)
			if got := b.APIKeys[secrets.ProviderOpenAICompatible] != ""; got != row.gwSealed {
				t.Fatalf("fixture: gateway sealed = %v, want %v — the row tests another shape (log:\n%s)", got, row.gwSealed, buf.String())
			}
			if !reflect.DeepEqual(creds.contentBoundary, ownKeys()) {
				t.Fatalf("content boundary = %+v, want an explicit own_keys stamp", creds.contentBoundary)
			}
			if sealed := creds.secretsRef != ""; sealed != row.anySealed {
				t.Fatalf("fixture: bundle sealed = %v, want %v — the row tests another shape (log:\n%s)", sealed, row.anySealed, buf.String())
			}
			if b.AmbientLLMRefused {
				t.Errorf("a trusted own_keys run carries the ambient refusal")
			}
		})
	}
}

// B3: a MIXED run — a gateway route attributed to the team's key beside a
// vendor route its own key funds — is held to the team's gateway all the same
// (arbitration 3): its vendor-funded auxiliaries are refused with the rest.
func TestContentBoundary_AMixedRunIsTeamGateway(t *testing.T) {
	p, _ := gatewayKeyPublisher(t)
	seedGatewayKey(t, p.apiKeys, p.sealer, "team1", nil)
	seedKey(t, p.apiKeys, p.sealer, "team1", secrets.ProviderAnthropic, "sk-ant-team")
	creds := resolveBoundaryRun(t, p, "run-cb-3", "", mixedGatewayWorkflow(), store.RunTrustDefault, nil)
	b := openGatewayBundle(t, p, "run-cb-3", creds)
	if b.APIKeys[secrets.ProviderOpenAICompatible] == "" || b.APIKeys[secrets.ProviderAnthropic] == "" {
		t.Fatalf("fixture: both keys must seal for a mixed run, got %v", b.APIKeys)
	}
	if !creds.contentBoundary.TeamGateway() {
		t.Fatalf("content boundary = %+v, want team_gateway for a mixed run", creds.contentBoundary)
	}
	if !b.AmbientLLMRefused {
		t.Errorf("a mixed team_gateway run seals no ambient refusal")
	}
}

// B4: the ambient refusal ORs the two facts and never clears one: an
// untrusted vendor run keeps ADR-162's stamp under an own_keys boundary, a
// trusted team_gateway run carries it, and a trusted own_keys run does not.
func TestContentBoundary_TheAmbientRefusalOrsTrustAndTheBoundary(t *testing.T) {
	vendor := &ir.Workflow{Nodes: map[string]ir.Node{
		"review": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "review"}, LLMFields: ir.LLMFields{Backend: "claw", Model: "anthropic/claude-sonnet-4-5"}},
	}}
	for _, row := range []struct {
		name     string
		wf       *ir.Workflow
		trust    store.RunTrust
		gateway  bool
		refused  bool
		boundary string
	}{
		{"an untrusted vendor run", vendor, store.RunTrustFork, false, true, store.ContentBoundaryOwnKeys},
		{"a trusted team_gateway run", gatewayOnlyWorkflow(), store.RunTrustDefault, true, true, store.ContentBoundaryTeamGateway},
		{"a trusted vendor run (the control)", vendor, store.RunTrustDefault, false, false, store.ContentBoundaryOwnKeys},
	} {
		t.Run(row.name, func(t *testing.T) {
			p, _ := gatewayKeyPublisher(t)
			if row.gateway {
				seedGatewayKey(t, p.apiKeys, p.sealer, "team1", nil)
			} else {
				seedKey(t, p.apiKeys, p.sealer, "team1", secrets.ProviderAnthropic, "sk-ant-team")
			}
			creds := resolveBoundaryRun(t, p, "run-cb-4", "", row.wf, row.trust, nil)
			if creds.contentBoundary == nil || creds.contentBoundary.Kind != row.boundary {
				t.Fatalf("content boundary = %+v, want kind %q", creds.contentBoundary, row.boundary)
			}
			if got := openGatewayBundle(t, p, "run-cb-4", creds).AmbientLLMRefused; got != row.refused {
				t.Errorf("bundle.AmbientLLMRefused = %v, want %v", got, row.refused)
			}
		})
	}
}

// B5: KeyID names the key row that filled the gateway slot, at every site
// that fills it: the team's BYOK tier, the restore of a capped team key, the
// org tier, the platform tier, and an attached pool's entry (the pledge's
// key) when the team's own capped key left the slot to it.
func TestContentBoundary_KeyIDNamesTheRowThatFilledTheGatewaySlot(t *testing.T) {
	capKey := func(t *testing.T, p *Publisher, team string) string {
		var id string
		seedGatewayKey(t, p.apiKeys, p.sealer, team, func(k *secrets.ApiKey) { id = k.ID; k.MaxConcurrentRuns = 1 })
		rs, err := store.New(t.TempDir())
		if err != nil {
			t.Fatalf("store: %v", err)
		}
		ctx := context.Background()
		if _, err := rs.CreateRun(ctx, "alive-1", "demo", nil); err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
		r, err := rs.LoadRun(ctx, "alive-1")
		if err != nil {
			t.Fatalf("LoadRun: %v", err)
		}
		r.Status = store.RunStatusRunning
		if err := rs.SaveRun(ctx, r); err != nil {
			t.Fatalf("SaveRun: %v", err)
		}
		if err := rs.SetRunCredStamp(ctx, "alive-1", store.RunCredStamp{Fingerprints: []string{gwFingerprint}}); err != nil {
			t.Fatalf("stamp: %v", err)
		}
		p.store = rs
		return id
	}
	for _, row := range []struct {
		name  string
		orgID string
		// seed returns the id of the row the slot must name.
		seed func(t *testing.T, p *Publisher) string
	}{
		{"the team's BYOK tier", "", func(t *testing.T, p *Publisher) string {
			var id string
			seedGatewayKey(t, p.apiKeys, p.sealer, "team1", func(k *secrets.ApiKey) { id = k.ID })
			return id
		}},
		{"the restore of a capped team key", "", func(t *testing.T, p *Publisher) string { return capKey(t, p, "team1") }},
		{"the org tier", "org-1", func(t *testing.T, p *Publisher) string {
			p.identity = &fakeTeamResolver{orgs: map[string]string{"team1": "org-1"},
				orgDocs: map[string]identity.Org{"org-1": {ID: "org-1", CredentialAudience: identity.CredentialAudience{Teams: []string{"team1"}}}}}
			var id string
			seedGatewayKey(t, p.apiKeys, p.sealer, secrets.OrgTierTenantID("org-1"), func(k *secrets.ApiKey) { id = k.ID })
			return id
		}},
		{"the platform tier", "", func(t *testing.T, p *Publisher) string {
			var id string
			seedGatewayKey(t, p.apiKeys, p.sealer, secrets.PlatformTenantID, func(k *secrets.ApiKey) { id = k.ID })
			return id
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			p, buf := gatewayKeyPublisher(t)
			want := row.seed(t, p)
			creds := resolveBoundaryRun(t, p, "run-cb-5", row.orgID, gatewayOnlyWorkflow(), store.RunTrustDefault, nil)
			if got := openGatewayBundle(t, p, "run-cb-5", creds).APIKeys[secrets.ProviderOpenAICompatible]; got != gwTeamKey {
				t.Fatalf("fixture: gateway slot = %q, the row's tier must fill it (log:\n%s)", got, buf.String())
			}
			if creds.contentBoundary == nil || creds.contentBoundary.KeyID != want || creds.contentBoundary.GatewayHost != gwHost {
				t.Fatalf("content boundary = %+v, want key_id %q (the row that filled the slot) and host %q", creds.contentBoundary, want, gwHost)
			}
		})
	}
	t.Run("an attached pool's entry", func(t *testing.T) {
		f := newAttachedFixture(t)
		p, _ := gatewayKeyPublisher(t)
		f.pub.apiKeys, f.pub.usageCaps = p.apiKeys, p.usageCaps
		// The team's own key is held (the routes are the team's) but capped,
		// so the attached pool's entry fills the slot before any restore.
		p.sealer = f.sealer
		capKey(t, p, poolTeam)
		f.pub.store = p.store
		var buf bytes.Buffer
		f.pub.logger = iterlog.New(iterlog.LevelInfo, &buf)
		b, creds := f.resolveAttached(t, "run-cb-5b", gatewayOnlyWorkflow(), frozenAttachment("pool-att"))
		if got := b.APIKeys[secrets.ProviderOpenAICompatible]; got != "sk-gw-0000" {
			t.Fatalf("fixture: gateway slot = %q, the attached pool's entry must fill it (log:\n%s)", got, buf.String())
		}
		if creds.contentBoundary == nil || creds.contentBoundary.KeyID != "key-gw" || creds.contentBoundary.GatewayHost != gwHost {
			t.Fatalf("content boundary = %+v, want key_id %q (the pledge's key) and host %q", creds.contentBoundary, "key-gw", gwHost)
		}
	})
}

// ladderPolicy orders the anthropic key, the gateway slot and the openai key:
// the openai pair is the crossing §6-ter n°13 is about — claw+openai_key
// renders the class's openai model on a node written for the gateway.
func ladderPolicy() *store.RunLLMRoutePolicy {
	return &store.RunLLMRoutePolicy{
		PairOrder: []string{llmroute.Pair(llmroute.HarnessClaw, "anthropic_key"), llmroute.Pair(llmroute.HarnessClaw, "openai_compatible_key"), llmroute.Pair(llmroute.HarnessClaw, "openai_key")},
		Triggers:  []string{llmroute.TriggerUsageWindow},
		Sources:   map[string]string{llmroute.FieldPairOrder: llmroute.SourceDefault},
	}
}

// policyCrossing reports whether the node carries a policy-ladder rung.
func policyCrossing(wf *ir.Workflow, node string) bool {
	agent, ok := wf.Nodes[node].(*ir.AgentNode)
	if !ok {
		return false
	}
	return slices.ContainsFunc(agent.Fallbacks, func(fb ir.Fallback) bool { return fb.Policy })
}

// B6 (§6-ter n°13, the ladder): under team_gateway the ladder lands no vendor
// crossing on the node routed to the gateway, by name, while the mixed run's
// vendor node keeps its rung and the stage still rides the message for it.
// Without the boundary — the team holds no gateway key, the gateway route rides
// the operator's environment — the crossing lands on both, as llmroute's
// unstamped rows pin.
func TestContentBoundary_TheLadderLandsNoVendorCrossingOnAGatewayNode(t *testing.T) {
	for _, row := range []struct {
		name        string
		teamGateway bool
	}{
		{"team_gateway", true},
		{"unstamped gateway (the control)", false},
	} {
		t.Run(row.name, func(t *testing.T) {
			p, buf := gatewayKeyPublisher(t)
			if row.teamGateway {
				seedGatewayKey(t, p.apiKeys, p.sealer, "team1", nil)
			}
			seedKey(t, p.apiKeys, p.sealer, "team1", secrets.ProviderOpenAI, "sk-openai-team")
			wf := mixedGatewayWorkflow()
			creds := resolveBoundaryRun(t, p, "run-cb-6", "", wf, store.RunTrustDefault, ladderPolicy())
			if creds.contentBoundary.TeamGateway() != row.teamGateway {
				t.Fatalf("fixture: content boundary = %+v, want team_gateway %v", creds.contentBoundary, row.teamGateway)
			}
			if !slices.ContainsFunc(creds.Ladder, func(e queue.RunFallbackEntry) bool { return e.Provider == "openai_key" }) {
				t.Fatalf("ladder = %+v: the openai stage must ride the message for the vendor node", creds.Ladder)
			}
			if !policyCrossing(wf, "review") {
				t.Fatalf("the vendor node lost its crossing (log:\n%s)", buf.String())
			}
			if got := policyCrossing(wf, "gw"); got == row.teamGateway {
				t.Fatalf("gateway node crossing = %v, want %v (log:\n%s)", got, !row.teamGateway, buf.String())
			}
			if row.teamGateway && !strings.Contains(buf.String(), llmroute.ReasonContentBoundary) {
				t.Errorf("the refusal is not named:\n%s", buf.String())
			}
		})
	}
}

// B7 (§6-ter n°13, the door): under team_gateway the fallback door asks no
// donor for a kind only the gateway node's crossing would carry — on the
// launch and in the preview — while without the boundary a consenting
// donor's openai key is lent for that crossing. The vendor node carries an
// authored route, so the gateway node is the only one the ladder may reach.
func TestContentBoundary_TheFallbackDoorAsksNoDonorForAGatewayNode(t *testing.T) {
	for _, row := range []struct {
		name        string
		teamGateway bool
	}{
		{"team_gateway", true},
		{"unstamped gateway (the control)", false},
	} {
		t.Run(row.name, func(t *testing.T) {
			sealer, err := secrets.NewKeyRingSealer(map[string][]byte{"default": make([]byte, 32)}, "default")
			if err != nil {
				t.Fatalf("sealer: %v", err)
			}
			ctx := context.Background()
			keys := secrets.NewMemoryApiKeyStore()
			if row.teamGateway {
				seedGatewayKey(t, keys, sealer, poolTeam, nil)
			}
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
			wf := func() *ir.Workflow {
				return &ir.Workflow{Nodes: map[string]ir.Node{
					"gw": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "gw"}, LLMFields: ir.LLMFields{Backend: "claw", Model: "openai_compatible/team/m"}, Tools: []string{}},
					"review": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "review"}, LLMFields: ir.LLMFields{Backend: "claw", Model: "anthropic/claude-opus-5"}, Tools: []string{},
						Fallbacks: []ir.Fallback{{Name: "haiku", Backend: "claw", Model: "anthropic/claude-haiku-4-5"}}},
				}}
			}
			tctx := secrets.WithLaunchRepo(store.WithTenant(ctx, poolTeam), gwRepo)
			launchWF := wf()
			creds, err := p.resolveAndSealCredentials(tctx, "run-cb-7", poolOrg, poolTeam, "owner1", gwBot, "", launchWF, nil, nil, model.RouteView{Workflow: launchWF}, nil, store.RunTrustDefault, nil, ladderPolicy(), nil, boundaryDerived)
			if err != nil {
				t.Fatalf("resolveAndSealCredentials: %v", err)
			}
			if creds.contentBoundary.TeamGateway() != row.teamGateway {
				t.Fatalf("fixture: content boundary = %+v, want team_gateway %v", creds.contentBoundary, row.teamGateway)
			}
			if creds.secretsRef == "" {
				t.Fatalf("fixture: the run sealed nothing (log:\n%s)", buf.String())
			}
			b := bundleFrom(t, p, tctx, creds, "run-cb-7")
			if lent := creds.doorGrant != nil && b.APIKeys[secrets.ProviderOpenAI] != ""; lent == row.teamGateway {
				t.Fatalf("door grant = %v, openai slot sealed = %v — want lent %v (log:\n%s)", creds.doorGrant, b.APIKeys[secrets.ProviderOpenAI] != "", !row.teamGateway, buf.String())
			}
			spec := previewSpec(poolTeam, "owner1")
			spec.LLMRoutePolicy = ladderPolicy()
			spec.Context.BotID = gwBot
			preview, err := previewReadOnly(p).PreviewCredentials(tctx, spec, wf())
			if err != nil {
				t.Fatalf("preview: %v", err)
			}
			if asked := slices.Contains(preview.Pool.DoorWants, "api_key:openai"); asked == row.teamGateway {
				t.Fatalf("preview door wants = %v (reason %q) — want the openai ask %v, as the launch", preview.Pool.DoorWants, preview.Pool.DoorReason, !row.teamGateway)
			}
		})
	}
}

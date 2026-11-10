// ADR-131 §5, the walk half (#2005 PR-C'): a team's own openai_compatible
// key funds its gateway routes. When a credential tier the walk consults
// holds a gateway key serving the launch (its bot and repository audiences),
// the gateway routes are key-funded — the walk seals the PAIR (key, canonical
// URL, endpoint-aware fingerprint) and the run is no longer env-funded. When
// no tier holds one, the run is env-funded exactly as before.
//
// Mutants these witnesses red on, each run and restored: the walk's
// vocabulary without the gateway (allKnownProviders = the pool's) → W1, W7;
// the envFunded predicate read without the probe → W1, W3, W4, W5; the probe
// ignoring the audiences → W2; the probe applying the usability predicate →
// W3; the preview keeping its own predicate → W4; the SealOnly filter dropped
// from the ladder composer → W5; the probe consulting the frozen pair order →
// W6; the wants guard dropped → W8 (and W6's legacy pool row); the sovereign
// arm dropped from gatewayRouting → W9's refused row, or made blind to the
// runner pool → its pool-stamped row; the attributed set read under the
// pool's vocabulary → W9's refused row; the launch's or the preview's
// holds-nothing counting a gateway key → W10, each surface alone. The
// gateway slot always sealable → W11 and W12's vendor row; its undecided arm
// read without the runner pool → W7's default-runner rows, W7b, W7c and W12;
// without the undecided arm → W7's pool rows and the content boundary's
// dial row; mayRide read without the
// runner pool → W7 and W7c; without its
// attributed arm → W1-W6; the frozen list noted before the gate → W11's
// frozen arm; the preview without the gate or its reason → W11's preview
// asserts; the attached pool on its former want condition → W12's dial row;
// the walk without the gate → W11, W12. holdsDefaultLLMCredential counting a
// gateway key → W10, W13, W14; the legacy pool's, the door's, the preview's
// admission, door and sovereign twins on their former readings → W13, W14,
// W13's preview, W14's preview, W10, W10's preview, each alone.

package cloudpublisher

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/llmroute"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

const (
	gwTeamKey = "sk-gw-team"
	gwURL     = "https://gw.example"
	gwBot     = "docs-refresh"
	gwRepo    = "acme/app"
	// envFundedLine is the walk's own statement that a run rides the runner's
	// gateway and acquires nothing: present exactly when the walk's envFunded
	// predicate held.
	envFundedLine = "is env-funded"
)

var gwFingerprint = secrets.APIKeyFingerprint(secrets.ProviderOpenAICompatible, gwTeamKey, gwURL)

// seedGatewayKey seals a team gateway key the way the key routes store one:
// the canonical base URL bound into the seal, the endpoint-aware fingerprint
// on the row. shape adjusts the row (audiences, ceiling) before it is sealed.
func seedGatewayKey(t *testing.T, st secrets.ApiKeyStore, sealer secrets.Sealer, teamID string, shape func(*secrets.ApiKey)) {
	t.Helper()
	k := secrets.ApiKey{
		ID: secrets.NewApiKeyID(), TenantID: teamID, ScopeTeamID: teamID,
		Provider: secrets.ProviderOpenAICompatible, Name: "team gateway",
		BaseURL: gwURL, Fingerprint: gwFingerprint, CreatedAt: time.Now().UTC(),
	}
	if shape != nil {
		shape(&k)
	}
	sealed, err := secrets.SealAPIKeyFor(sealer, k, []byte(gwTeamKey))
	if err != nil {
		t.Fatalf("seal gateway key: %v", err)
	}
	k.SealedSecret = sealed
	if err := st.Create(context.Background(), k); err != nil {
		t.Fatalf("create gateway key: %v", err)
	}
}

// gatewayKeyPublisher is the sealing stack with an empty key store and a
// capturing logger at Info.
func gatewayKeyPublisher(t *testing.T) (*Publisher, *bytes.Buffer) {
	t.Helper()
	sealer, err := secrets.NewKeyRingSealer(map[string][]byte{"default": make([]byte, 32)}, "default")
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	var buf bytes.Buffer
	return &Publisher{apiKeys: secrets.NewMemoryApiKeyStore(), oauthForfait: secrets.NewMemoryOAuthStore(),
		usageCaps: usagecap.NewMemStore(), runSecrets: secrets.NewMemoryRunSecretsStore(), sealer: sealer,
		logger: iterlog.New(iterlog.LevelInfo, &buf)}, &buf
}

// gatewayLaunchCtx is the launch's context as SubmitLaunch builds it: the
// tenant, and the repository the launch serves.
func gatewayLaunchCtx() context.Context {
	return secrets.WithLaunchRepo(store.WithTenant(context.Background(), "team1"), gwRepo)
}

func resolveGatewayRun(t *testing.T, p *Publisher, runID string, wf *ir.Workflow, policy *store.RunLLMRoutePolicy) (credResolution, error) {
	t.Helper()
	return p.resolveAndSealCredentials(gatewayLaunchCtx(), runID, "", "team1", "owner1", gwBot, "", wf, nil, nil, model.RouteView{Workflow: wf}, nil, store.RunTrustDefault, nil, policy, nil, boundaryDerived)
}

// openGatewayBundle opens what a resolution sealed; an empty ref is the empty
// bundle (nothing sealed at all).
func openGatewayBundle(t *testing.T, p *Publisher, runID string, creds credResolution) secrets.RunBundle {
	t.Helper()
	return openGatewayBundleOn(t, p, runID, "", creds)
}

// openGatewayBundleOn opens what a resolution sealed for a launch mapped to
// runnerPool: the seal binds the pool, so only its own runner opens it.
func openGatewayBundleOn(t *testing.T, p *Publisher, runID, runnerPool string, creds credResolution) secrets.RunBundle {
	t.Helper()
	if creds.secretsRef == "" {
		return secrets.RunBundle{}
	}
	rec, err := p.runSecrets.Get(store.WithTenant(context.Background(), "team1"), creds.secretsRef)
	if err != nil {
		t.Fatalf("RunSecrets.Get: %v", err)
	}
	b, err := secrets.OpenRunBundle(mustDEKSealer(t, creds.dek), rec.TenantID, runnerPool, runID, rec.KeyID, rec.SealedBundle)
	if err != nil {
		t.Fatalf("OpenRunBundle: %v", err)
	}
	return b
}

// W1: the team holds a gateway key serving the launch → the walk seals the
// PAIR and the run is no longer env-funded; the run-doc stamp names the
// key's endpoint-aware fingerprint, the identity the runner books it under.
func TestGatewayKey_TheWalkSealsThePairAndTheRunIsNotEnvFunded(t *testing.T) {
	p, buf := gatewayKeyPublisher(t)
	seedGatewayKey(t, p.apiKeys, p.sealer, "team1", nil)
	creds, err := resolveGatewayRun(t, p, "run-gw-1", envFundedWorkflow, nil)
	if err != nil {
		t.Fatalf("resolveAndSealCredentials: %v", err)
	}
	b := openGatewayBundle(t, p, "run-gw-1", creds)
	gw := secrets.ProviderOpenAICompatible
	if b.APIKeys[gw] != gwTeamKey {
		t.Fatalf("gateway slot = %q, want the team's own gateway key sealed (log:\n%s)", b.APIKeys[gw], buf.String())
	}
	if b.APIBaseURLs[gw] != gwURL {
		t.Errorf("gateway URL = %q, want the key's canonical endpoint — the PAIR travels", b.APIBaseURLs[gw])
	}
	if b.APIKeyFingerprints[gw] != gwFingerprint {
		t.Errorf("gateway fingerprint = %q, want the server's endpoint-aware stamp %q", b.APIKeyFingerprints[gw], gwFingerprint)
	}
	if !slices.Equal(creds.fingerprints, []string{gwFingerprint}) {
		t.Errorf("run-doc stamp = %v, want exactly the gateway key's fingerprint", creds.fingerprints)
	}
	if strings.Contains(buf.String(), envFundedLine) {
		t.Errorf("the walk still calls the run env-funded:\n%s", buf.String())
	}
}

// W2: the probe reads the audiences the walk applies — a key whose bot or
// repository list excludes the launch funds nothing, and the run stays
// env-funded exactly as before; a key naming the launch's bot and repository
// funds it.
func TestGatewayKey_AnAudienceThatExcludesTheLaunchKeepsTheRunEnvFunded(t *testing.T) {
	for _, row := range []struct {
		name   string
		shape  func(*secrets.ApiKey)
		sealed bool
	}{
		{"bots exclude the launch", func(k *secrets.ApiKey) { k.Bots = []string{"other-bot"} }, false},
		{"repos exclude the launch", func(k *secrets.ApiKey) { k.Repos = []string{"acme/other"} }, false},
		{"bots and repos name the launch", func(k *secrets.ApiKey) { k.Bots = []string{gwBot}; k.Repos = []string{gwRepo} }, true},
	} {
		t.Run(row.name, func(t *testing.T) {
			p, buf := gatewayKeyPublisher(t)
			seedGatewayKey(t, p.apiKeys, p.sealer, "team1", row.shape)
			creds, err := resolveGatewayRun(t, p, "run-gw-2", envFundedWorkflow, nil)
			if err != nil {
				t.Fatalf("resolveAndSealCredentials: %v", err)
			}
			got := openGatewayBundle(t, p, "run-gw-2", creds).APIKeys[secrets.ProviderOpenAICompatible] != ""
			if got != row.sealed {
				t.Fatalf("gateway key sealed = %v, want %v (log:\n%s)", got, row.sealed, buf.String())
			}
			if envFunded := strings.Contains(buf.String(), envFundedLine); envFunded == row.sealed {
				t.Fatalf("env-funded line present = %v, want %v — a key the launch's audience excludes is not held for it:\n%s", envFunded, !row.sealed, buf.String())
			}
		})
	}
}

// W3: the probe has NO ceiling predicate. A key at its concurrency ceiling is
// still the team's key: the routes are the team's to fund, the walk's restore
// hands the key back (a parked run beats a run spending the pod's gateway),
// and the admission step after the walk is what parks the run on it.
func TestGatewayKey_ACappedKeyStillFundsTheGatewayRoutes(t *testing.T) {
	p, buf := gatewayKeyPublisher(t)
	seedGatewayKey(t, p.apiKeys, p.sealer, "team1", func(k *secrets.ApiKey) { k.MaxConcurrentRuns = 1 })
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
	creds, err := resolveGatewayRun(t, p, "run-gw-3", envFundedWorkflow, nil)
	if err != nil {
		t.Fatalf("resolveAndSealCredentials: %v", err)
	}
	if got := openGatewayBundle(t, p, "run-gw-3", creds).APIKeys[secrets.ProviderOpenAICompatible]; got != gwTeamKey {
		t.Fatalf("gateway slot = %q, want the capped team key restored — a capped key must never hand the routes to the pod's gateway:\n%s", got, buf.String())
	}
	// The key came back through the restore, not a walk that never asked the
	// ceiling: otherwise the assertion above proves nothing about the probe.
	if !strings.Contains(buf.String(), "AT ITS CEILING") || !strings.Contains(buf.String(), "refused api-key RESTORED") {
		t.Fatalf("the fixture never put the key at its ceiling and restored it, so this proves nothing:\n%s", buf.String())
	}
}

// W4: the preview answers through the walk's own helper — a team gateway key
// makes the run key-funded on both surfaces: the preview selects the key and
// drops the env-funded warning, the live walk seals it. Without a key the
// sibling witness TestCredentialPreviewMatchesTheEnvFundedRun still holds.
func TestGatewayKey_ThePreviewAnswersTheWalksPredicate(t *testing.T) {
	p, _ := gatewayKeyPublisher(t)
	seedGatewayKey(t, p.apiKeys, p.sealer, "team1", nil)
	spec := previewSpec("team1", "owner1")
	preview, err := previewReadOnly(p).PreviewCredentials(context.Background(), spec, envFundedWorkflow)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	for _, w := range preview.Warnings {
		if strings.Contains(w, "rides the runner's openai_compatible gateway") {
			t.Fatalf("the preview calls a run whose team holds a gateway key env-funded: %q", w)
		}
	}
	if got := selectedDescriptions(preview); !slices.Contains(got, "team:api_key:openai_compatible") {
		t.Fatalf("preview selected %v, want the team's gateway key", got)
	}
	creds, err := p.resolveAndSealCredentials(store.WithTenant(context.Background(), "team1"), "run-gw-4", "", "team1", spec.OwnerID, spec.Context.BotID, "", envFundedWorkflow, nil, nil, model.RouteView{Workflow: envFundedWorkflow}, nil, store.RunTrustDefault, nil, nil, nil, boundaryDerived)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got := openGatewayBundle(t, p, "run-gw-4", creds).APIKeys[secrets.ProviderOpenAICompatible]; got != gwTeamKey {
		t.Fatalf("the live walk did not seal what the preview selected: %q", got)
	}
}

// W5: once the walk seals the gateway key, claw+openai_compatible_key is a
// HELD pair of the order — a seal-whitelist entry, never a ladder rung: the
// run message carries no fallback entry naming the slot, while the held
// vendor pairs still compose theirs.
func TestGatewayKey_TheSealedGatewayPairNeverOrdersALadderRung(t *testing.T) {
	p, _ := gatewayKeyPublisher(t)
	seedGatewayKey(t, p.apiKeys, p.sealer, "team1", nil)
	seedKey(t, p.apiKeys, p.sealer, "team1", secrets.ProviderAnthropic, "sk-ant-team")
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"draft": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "draft"}, LLMFields: ir.LLMFields{Backend: "claw", Model: "anthropic/claude-opus-5"}},
		"gw":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "gw"}, LLMFields: ir.LLMFields{Backend: "claw", Model: "openai_compatible/team/m"}},
	}}
	policy := &store.RunLLMRoutePolicy{
		PairOrder: append([]string(nil), llmroute.DefaultPairOrder...),
		Triggers:  []string{llmroute.TriggerUsageWindow},
		Sources:   map[string]string{llmroute.FieldPairOrder: llmroute.SourceDefault},
	}
	creds, err := resolveGatewayRun(t, p, "run-gw-5", wf, policy)
	if err != nil {
		t.Fatalf("resolveAndSealCredentials: %v", err)
	}
	if got := openGatewayBundle(t, p, "run-gw-5", creds).APIKeys[secrets.ProviderOpenAICompatible]; got != gwTeamKey {
		t.Fatalf("the gateway key is not sealed, so the pair is not held and this proves nothing: %q", got)
	}
	if len(creds.Ladder) == 0 {
		t.Fatal("no ladder composed at all — the held anthropic pairs must still order rungs, or the absence below proves nothing")
	}
	for _, e := range creds.Ladder {
		if e.Provider == "openai_compatible_key" {
			t.Fatalf("ladder = %+v: the gateway pair became a rung", creds.Ladder)
		}
	}
}

// preUpgradePolicy is a pair order frozen before the gateway entry existed:
// the shipped default of its launch day, which named no gateway pair.
func preUpgradePolicy() *store.RunLLMRoutePolicy {
	var order []string
	for _, pair := range llmroute.DefaultPairOrder {
		if _, c, err := llmroute.ParsePair(pair); err == nil && llmroute.SealOnly(c) {
			continue
		}
		order = append(order, pair)
	}
	return &store.RunLLMRoutePolicy{
		PairOrder: order,
		Triggers:  []string{llmroute.TriggerUsageWindow},
		Sources:   map[string]string{llmroute.FieldPairOrder: llmroute.SourceDefault},
	}
}

// W6, the obligation pkg/llmroute's former tripwire handed to this site
// (ADR-131 §5): a run whose frozen pair order predates the gateway entry is
// replayed AS STORED — its team's gateway key is not sealed — and the walk
// says so exactly once, naming the slot and the list's provenance. The legacy
// pool, reached because the bundle stayed empty, lends nothing to the gateway
// route (no want of the pool's vocabulary serves it).
func TestGatewayKey_AFrozenPreUpgradeOrderReplaysAsStoredAndSaysSoOnce(t *testing.T) {
	f := newPoolFixture(t, credpool.Limits{})
	var buf bytes.Buffer
	f.pub.logger = iterlog.New(iterlog.LevelInfo, &buf)
	f.pub.apiKeys = secrets.NewMemoryApiKeyStore()
	seedGatewayKey(t, f.pub.apiKeys, f.sealer, poolTeam, nil)
	ctx := store.WithTenant(context.Background(), poolTeam)
	creds, err := f.pub.resolveAndSealCredentials(ctx, "run-gw-6", poolOrg, poolTeam, "owner1", gwBot, "", envFundedWorkflow, nil, nil, model.RouteView{Workflow: envFundedWorkflow}, nil, store.RunTrustDefault, nil, preUpgradePolicy(), nil, boundaryDerived)
	if err != nil {
		t.Fatalf("resolveAndSealCredentials: %v", err)
	}
	if creds.secretsRef != "" {
		b := bundleFrom(t, f.pub, ctx, creds, "run-gw-6")
		if b.APIKeys[secrets.ProviderOpenAICompatible] != "" {
			t.Fatal("the gateway key sealed under a frozen order that names no gateway pair — the list must be replayed as stored")
		}
		if len(b.OAuthCredentials) != 0 || len(b.APIKeys) != 0 {
			t.Fatalf("the legacy pool lent %v/%v to a run whose every route rides the gateway", b.APIKeys, b.OAuthCredentials)
		}
	}
	if creds.grant != nil {
		t.Fatalf("pool grant %v for a gateway-only run: the pool's vocabulary lends nothing a gateway route spends", creds.grant)
	}
	log := buf.String()
	if got := strings.Count(log, heldOutMarker); got != 1 {
		t.Fatalf("the §5 line appears %d time(s), want exactly 1:\n%s", got, log)
	}
	for _, want := range []string{"openai_compatible_key", "SHIPPED DEFAULT", "run=run-gw-6"} {
		if !strings.Contains(log, want) {
			t.Errorf("the §5 line does not carry %q:\n%s", want, log)
		}
	}
}

// W7, the operator's production case and its arbitration (#2005 PR-C'
// round 1): the routes are ENV-TEMPLATED dials the runner pod expands (the
// publisher reads the default only), so the walk cannot attribute them to the
// gateway — ADR-129 §4: an undecided route is never key-funded. Under a
// dedicated runner pool (D13, the pod carries the operator's gateway dials)
// the BYOK tier seals the team's gateway PAIR for the runner's claw dispatch
// to prefer over the pod's environment; on the default runner, whose dials
// never expand to the gateway, it seals nothing. The key's audience is the
// pool arm's control: a key the launch's bot is not named on is not sealed.
func TestGatewayKey_AnEnvTemplatedRouteSealsTheKeyOnlyUnderARunnerPool(t *testing.T) {
	serves := func(k *secrets.ApiKey) { k.Bots = []string{gwBot}; k.Repos = []string{gwRepo} }
	for _, dial := range []string{
		"${C3_UNSET_ROUTE_DIAL:-anthropic/claude-sonnet-4-5}",
		"${C3_UNSET_ROUTE_DIAL:-openai_compatible/scaleway/deepseek-v4.1-flash}",
	} {
		wf := &ir.Workflow{Nodes: map[string]ir.Node{
			"review": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "review"}, LLMFields: ir.LLMFields{Backend: "claw", Model: dial}},
		}}
		for _, row := range []struct {
			name       string
			runnerPool string
			shape      func(*secrets.ApiKey)
			sealed     bool
		}{
			{"runner pool, key serves the launch", "pool-a", serves, true},
			{"runner pool, key's bots exclude the launch", "pool-a", func(k *secrets.ApiKey) { k.Bots = []string{"other-bot"} }, false},
			{"default runner, key serves the launch", "", serves, false},
		} {
			t.Run(dial+"/"+row.name, func(t *testing.T) {
				p, buf := gatewayKeyPublisher(t)
				seedGatewayKey(t, p.apiKeys, p.sealer, "team1", row.shape)
				creds, err := p.resolveAndSealCredentials(gatewayLaunchCtx(), "run-gw-7", "", "team1", "owner1", gwBot, row.runnerPool, wf, nil, nil, model.RouteView{Workflow: wf}, nil, store.RunTrustDefault, nil, nil, nil, boundaryDerived)
				if err != nil {
					t.Fatalf("resolveAndSealCredentials: %v", err)
				}
				b := openGatewayBundleOn(t, p, "run-gw-7", row.runnerPool, creds)
				gw := secrets.ProviderOpenAICompatible
				if got := b.APIKeys[gw] == gwTeamKey; got != row.sealed {
					t.Fatalf("gateway key sealed = %v, want %v (log:\n%s)", got, row.sealed, buf.String())
				}
				if row.sealed && (b.APIBaseURLs[gw] != gwURL || b.APIKeyFingerprints[gw] != gwFingerprint) {
					t.Errorf("the key travelled without its pair: url=%q fp=%q", b.APIBaseURLs[gw], b.APIKeyFingerprints[gw])
				}
				routing := p.gatewayRouting(gatewayLaunchCtx(), model.RouteView{Workflow: wf}, "team1", "", "owner1", gwBot, false, row.runnerPool != "")
				if routing.sealable != (row.runnerPool != "") || routing.mayRide != routing.sealable {
					t.Errorf("sealable=%v mayRide=%v under runner pool %q: an undecided route rides the gateway exactly under a runner pool", routing.sealable, routing.mayRide, row.runnerPool)
				}
				if attributed := attributedRoutes(routing.view).providers; slices.Contains(attributed, string(gw)) {
					t.Errorf("attributed = %v: an env-templated route is the runner's to expand, never attributed to the gateway key", attributed)
				}
				if row.runnerPool != "" {
					return
				}
				// The preview reads the same gate (this publisher maps the team
				// to no runner pool): the key is not selected, and it says why.
				preview, err := previewReadOnly(p).PreviewCredentials(gatewayLaunchCtx(), previewSpec("team1", "owner1"), wf)
				if err != nil {
					t.Fatalf("preview: %v", err)
				}
				if got := selectedDescriptions(preview); slices.Contains(got, "team:api_key:openai_compatible") {
					t.Fatalf("preview selected %v: the default-runner launch seals no gateway key", got)
				}
				named := false
				for _, c := range preview.Candidates {
					if c.Provider == string(gw) && strings.Contains(c.Reason, "only under a dedicated runner pool") {
						named = true
					}
				}
				if !named {
					t.Errorf("the preview does not say why the team's gateway key stays out: %+v", preview.Candidates)
				}
			})
		}
	}
}

// W7d (A2 of the round-1 review): a key that may be spent is counted. Under a
// runner pool the walk seals the team's gateway key for an env-templated dial
// the pod may expand to the gateway — so the run's stamp names the key and the
// admission reserves a slot of its max_concurrent_runs, though the
// publisher's own expansion of the dial reads a vendor route.
func TestGatewayKey_AnEnvDialRunUnderARunnerPoolHoldsTheSealedKeysSlot(t *testing.T) {
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"review": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "review"}, LLMFields: ir.LLMFields{Backend: "claw", Model: "${C3_UNSET_ROUTE_DIAL:-anthropic/claude-sonnet-4-5}"}},
	}}
	p, buf := gatewayKeyPublisher(t)
	seedGatewayKey(t, p.apiKeys, p.sealer, "team1", func(k *secrets.ApiKey) { k.MaxConcurrentRuns = 1 })
	creds, err := p.resolveAndSealCredentials(gatewayLaunchCtx(), "run-gw-7d", "", "team1", "owner1", gwBot, "pool-a", wf, nil, nil, model.RouteView{Workflow: wf}, nil, store.RunTrustDefault, nil, nil, nil, boundaryDerived)
	if err != nil {
		t.Fatalf("resolveAndSealCredentials: %v", err)
	}
	if got := openGatewayBundleOn(t, p, "run-gw-7d", "pool-a", creds).APIKeys[secrets.ProviderOpenAICompatible]; got != gwTeamKey {
		t.Fatalf("gateway slot = %q: the fixture must seal the key, or the stamp below proves nothing (log:\n%s)", got, buf.String())
	}
	if !slices.Contains(creds.fingerprints, gwFingerprint) {
		t.Fatalf("run stamp = %v: the sealed gateway key a pool pod may spend is not counted", creds.fingerprints)
	}
	if len(creds.credSlots) != 1 {
		t.Fatalf("cred slots = %+v, want the gateway key's one capped slot reserved", creds.credSlots)
	}
}

// W7b, the regression the arbitration closes (B1 of the round-1 review):
// a default-runner review whose dials are env-templated holds NO slot of the
// team's capped gateway key — the second concurrent launch queues instead of
// parking on a key its runner never spends, and no gateway fingerprint is
// stamped on either run document.
func TestGatewayKey_ADefaultRunnerEnvDialRunHoldsNoSlotOfTheGatewayKey(t *testing.T) {
	sealer, err := secrets.NewKeyRingSealer(map[string][]byte{"default": make([]byte, 32)}, "default")
	if err != nil {
		t.Fatal(err)
	}
	spy := newLedgerRetrySpy(t)
	keys := secrets.NewMemoryApiKeyStore()
	seedKey(t, keys, sealer, cappedTeam, secrets.ProviderAnthropic, "sk-ant-team")
	seedGatewayKey(t, keys, sealer, cappedTeam, func(k *secrets.ApiKey) {
		k.Bots = []string{"review-pr", "revi-converse"}
		k.Repos = []string{gwRepo}
		k.MaxConcurrentRuns = 1
		k.Name = "pliage-team"
	})
	published := &publishedRuns{}
	p := cappedKeyPublisher(spy, keys, sealer, nil, published)
	ctx := store.WithIdentity(context.Background(), cappedTeam, "webhook:cfg-1")
	// The review-pr shape: an env-templated dial beside an unpinned node, so
	// the walk is neither decided nor narrow-safe.
	wf := &ir.Workflow{Name: "wf", Nodes: map[string]ir.Node{
		"review": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "review"}, LLMFields: ir.LLMFields{Backend: "claude_code", Model: "${C3_UNSET_ROUTE_DIAL:-claude-opus-5-5}", Provider: "anthropic"}, Tools: []string{}},
		"triage": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "triage"}, LLMFields: ir.LLMFields{Backend: "claude_code"}, Tools: []string{}},
	}}
	spec := runview.LaunchSpec{FilePath: "main.bot", Source: "workflow wf:\n  start -> done\n", BotID: "review-pr", ProjectPath: gwRepo}
	for _, id := range []string{"run-rv-1", "run-rv-2"} {
		sub, err := p.SubmitLaunch(ctx, id, spec, wf, &runview.CompiledSource{Hash: "hash"})
		if err != nil {
			t.Fatalf("SubmitLaunch %s: %v", id, err)
		}
		r, err := spy.LoadRun(ctx, id)
		if err != nil {
			t.Fatalf("LoadRun %s: %v", id, err)
		}
		if sub.Status != store.RunStatusQueued {
			t.Fatalf("%s: status=%s failure=%s error=%q — a default-runner env-dial run parked on the gateway key's cap", id, sub.Status, sub.FailureCode, r.Error)
		}
		if slices.Contains(r.CredFingerprints, gwFingerprint) {
			t.Fatalf("%s: run stamp %v names the gateway key a default-runner env dial never spends", id, r.CredFingerprints)
		}
	}
	if got := published.ids(); len(got) != 2 {
		t.Fatalf("published = %v, want both reviews", got)
	}
}

// W7c, the pin's half of the arbitration (gatewayRoutes.mayRide reads the
// seal gate's undecided arm): a gateway pin on a default-runner run whose
// dials are env-templated is idle — the run rides no gateway route there,
// so nothing seals and nothing is refused — while the same pin under a
// runner pool governs the run (its refusal: TestGatewayPin_ABrokenPinRefusesTheLaunchByName).
func TestGatewayKey_AGatewayPinOnADefaultRunnerEnvDialIsIdle(t *testing.T) {
	dial := &ir.Workflow{Nodes: map[string]ir.Node{
		"review": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "review"}, LLMFields: ir.LLMFields{Backend: "claw", Model: "${C3_UNSET_ROUTE_DIAL:-anthropic/claude-sonnet-4-5}"}},
	}}
	p, buf := gatewayKeyPublisher(t)
	seedKey(t, p.apiKeys, p.sealer, "team1", secrets.ProviderAnthropic, "sk-ant-team")
	pin := seedPinKey(t, p, secrets.ProviderOpenAICompatible, "sk-gw-pinned", nil, "").ID
	creds, err := resolvePinnedRunOn(t, p, "run-gw-7c", "", dial, gatewayPin(pin), nil, boundaryDerived)
	if err != nil {
		t.Fatalf("a gateway pin on a default-runner env dial refused the launch: %v (log:\n%s)", err, buf.String())
	}
	if got := openGatewayBundle(t, p, "run-gw-7c", creds).APIKeys[secrets.ProviderOpenAICompatible]; got != "" {
		t.Fatalf("gateway slot = %q: the pinned key sealed for a run that rides no gateway route", got)
	}
}

// W8: under the pool's vocabulary a key-funded gateway route names nothing —
// neither a provider nor an unknown name — and a run whose every route rides
// the gateway asks the pool for NOTHING. Read through the empty provider list
// alone, it would take the full order: every donation, for a run that spends
// none of them.
func TestWantsFor_AKeyFundedGatewayRunAsksThePoolForNothing(t *testing.T) {
	wants, res := wantsFor(model.RouteView{Workflow: envFundedWorkflow, GatewayKeyFunded: true})
	if !res.NarrowSafe || len(res.Unknown) != 0 {
		t.Fatalf("the pool's walk widened on a key-funded gateway route: %+v", res)
	}
	if len(wants) != 0 {
		t.Fatalf("wants = %v, want none: the pool lends nothing a gateway route spends", wants)
	}
}

// W9: the sovereign posture (llm_fallback=none) owes the gateway routes a
// credential of the team's own (ADR-131 §5). Without a gateway key and outside
// a runner pool, the launch is refused BY NAME; a pool-stamped run keeps the
// env-funded behaviour (the pool's pod is the team's own sovereign gateway);
// a team gateway key funds the run.
func TestGatewayKey_TheSovereignPostureOwesTheGatewayRoutesAKey(t *testing.T) {
	const gatewayBot = "agent draft:\n  model: \"openai_compatible/glm-5.2\"\n  backend: \"claw\"\n\nworkflow main:\n  draft -> done\n"
	resolve := func(t *testing.T, p *Publisher, runnerPool string) (credResolution, error) {
		t.Helper()
		wf := compileTestSource(t, gatewayBot)
		ctx := store.WithTenant(context.Background(), "team1")
		return p.resolveAndSealCredentials(ctx, "run-gw-9", "", "team1", "owner1", "", runnerPool, wf, nil, nil, model.RouteView{Workflow: wf}, nil, store.RunTrustDefault, nil, nil, nil, boundaryDerived)
	}
	t.Run("no key, no runner pool: refused by name", func(t *testing.T) {
		p, _ := llmFallbackPublisher(t, map[string]string{"team1": "none"})
		_, err := resolve(t, p, "")
		var refusal *UnfundedProvidersError
		if !errors.As(err, &refusal) || !errors.Is(err, runview.ErrNoLLMCredential) {
			t.Fatalf("resolveAndSealCredentials = %v, want the sovereign unfunded refusal", err)
		}
		if !refusal.Sovereign || !slices.Equal(refusal.Providers, []string{"openai_compatible"}) || !slices.Equal(refusal.Sites["openai_compatible"], []string{"draft"}) {
			t.Fatalf("refusal = %+v, want the sovereign arm naming openai_compatible at draft", refusal)
		}
	})
	t.Run("no key, pool-stamped run: env-funded as before", func(t *testing.T) {
		p, buf := llmFallbackPublisher(t, map[string]string{"team1": "none"})
		if _, err := resolve(t, p, "pool-a"); err != nil {
			t.Fatalf("resolveAndSealCredentials = %v, want nil — a pool-stamped run rides its pool's own gateway", err)
		}
		if !strings.Contains(buf.String(), envFundedLine) {
			t.Fatalf("the pool-stamped run is not env-funded:\n%s", buf.String())
		}
	})
	t.Run("team gateway key: funded", func(t *testing.T) {
		p, _ := llmFallbackPublisher(t, map[string]string{"team1": "none"})
		seedGatewayKey(t, p.apiKeys, p.sealer, "team1", nil)
		creds, err := resolve(t, p, "")
		if err != nil {
			t.Fatalf("resolveAndSealCredentials = %v, want nil — the team's own gateway key funds the run", err)
		}
		if got := openGatewayBundle(t, p, "run-gw-9", creds).APIKeys[secrets.ProviderOpenAICompatible]; got != gwTeamKey {
			t.Fatalf("gateway slot = %q, want the team key", got)
		}
	})
}

// W10, #2485 for this shape (ADR-131 §5: "the gateway slot never pays an
// unattributed route"): under llm_fallback=none a workflow NO route of which
// the walk can attribute refuses when the team holds nothing of its own — and
// a gateway key is not "something" for such a route: it funds the routes the
// walk attributes to it and never an unreadable one, which would ride the
// pod's environment. The launch refuses with the unattributable sentence, and
// the preview answers it.
func TestGatewayKey_AGatewayKeyAloneDoesNotFundAnUnattributedRoute(t *testing.T) {
	p, _ := llmFallbackPublisher(t, map[string]string{"team1": "none"})
	seedGatewayKey(t, p.apiKeys, p.sealer, "team1", nil)
	launchErr := resolvePolicy(t, p, unattributableBot)
	var unfunded *UnfundedProvidersError
	if !errors.As(launchErr, &unfunded) || !unfunded.Unattributable || !unfunded.Sovereign {
		t.Fatalf("resolveAndSealCredentials = %v, want the sovereign unattributable refusal — the team's gateway key funds no unreadable route", launchErr)
	}
	v, err := p.PreviewCredentials(context.Background(), previewSpec("team1", "owner1"), compileTestSource(t, unattributableBot))
	if err != nil {
		t.Fatalf("PreviewCredentials: %v", err)
	}
	if want := "The live launch refuses: " + launchErr.Error(); previewWarning(v, want) == "" {
		t.Fatalf("the preview does not answer the launch's own sentence.\n want: %s\n got warnings: %v", want, v.Warnings)
	}
}

// W11 (R1): the gateway slot seals only for a run that may ride a gateway
// route — one the walk attributes to the key, or a route it cannot decide (not
// narrow-safe, or deferring to the runner's environment: W7). A decided
// vendor-only run never carries the pair, whatever the team holds: the pair
// would pin the gateway host on the run's proxy, make the run always-proxied
// and keep a subbot out of its parent's sandbox, for nothing. The preview
// names why the team's gateway key stays out.
func TestGatewayKey_ADecidedVendorRunNeverCarriesThePair(t *testing.T) {
	p, buf := gatewayKeyPublisher(t)
	seedGatewayKey(t, p.apiKeys, p.sealer, "team1", nil)
	seedKey(t, p.apiKeys, p.sealer, "team1", secrets.ProviderAnthropic, "sk-ant-team")
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"review": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "review"}, LLMFields: ir.LLMFields{Backend: "claw", Model: "anthropic/claude-sonnet-4-5"}},
	}}
	creds, err := resolveGatewayRun(t, p, "run-gw-11", wf, nil)
	if err != nil {
		t.Fatalf("resolveAndSealCredentials: %v", err)
	}
	b := openGatewayBundle(t, p, "run-gw-11", creds)
	gw := secrets.ProviderOpenAICompatible
	if b.APIKeys[secrets.ProviderAnthropic] != "sk-ant-team" {
		t.Fatalf("anthropic slot = %q: the fixture must seal the vendor key, or the absence below proves nothing", b.APIKeys[secrets.ProviderAnthropic])
	}
	if b.APIKeys[gw] != "" || b.APIBaseURLs[gw] != "" || b.APIKeyFingerprints[gw] != "" {
		t.Fatalf("a decided vendor-only run carries the gateway pair: key=%q url=%q fp=%q", b.APIKeys[gw], b.APIBaseURLs[gw], b.APIKeyFingerprints[gw])
	}
	// Under a pair order frozen before the gateway entry, the slot the run
	// never rides is no credential that list held out: no §5 line.
	buf.Reset()
	if _, err := resolveGatewayRun(t, p, "run-gw-11b", wf, preUpgradePolicy()); err != nil {
		t.Fatalf("resolveAndSealCredentials (frozen order): %v", err)
	}
	if strings.Contains(buf.String(), heldOutMarker) {
		t.Errorf("a slot no route of the run rides is reported as held out by the pair order:\n%s", buf.String())
	}
	preview, err := previewReadOnly(p).PreviewCredentials(gatewayLaunchCtx(), previewSpec("team1", "owner1"), wf)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if got := selectedDescriptions(preview); slices.Contains(got, "team:api_key:openai_compatible") {
		t.Fatalf("preview selected %v: the launch seals no gateway key for this run", got)
	}
	named := false
	for _, c := range preview.Candidates {
		if c.Provider == string(gw) && strings.Contains(c.Reason, "No route of this run rides the openai_compatible gateway") {
			named = true
		}
	}
	if !named {
		t.Errorf("the preview does not say why the team's gateway key stays out: %+v", preview.Candidates)
	}
}

// W12 (R1, the attached pool): the pool's gateway want reads the same answer
// as every tier. The attached pool is a shared tier, so a pool-mapped launch
// (D13) never reaches it; on the default runner an env-templated dial or an
// unpinned node the walk cannot decide never draws the pool's gateway entry
// (the arbitration's default-runner arm), and a decided vendor-only run never
// does either.
func TestGatewayKey_TheAttachedPoolsGatewayWantReadsTheSameAnswer(t *testing.T) {
	for _, row := range []struct {
		name, model, backend string
		sealed               bool
	}{
		{"env-templated dial on the default runner", "${C3_UNSET_ROUTE_DIAL:-anthropic/claude-sonnet-4-5}", "claw", false},
		{"unpinned claude_code node on the default runner", "", "claude_code", false},
		{"decided vendor route", "anthropic/claude-sonnet-4-5", "claw", false},
	} {
		t.Run(row.name, func(t *testing.T) {
			f := newAttachedFixture(t)
			wf := &ir.Workflow{Nodes: map[string]ir.Node{
				"review": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "review"}, LLMFields: ir.LLMFields{Backend: row.backend, Model: row.model}},
			}}
			bundle, _ := f.resolveAttached(t, "run-gw-12", wf, frozenAttachment("pool-att"))
			if len(bundle.OAuthCredentials["claude_code"]) == 0 {
				t.Fatalf("the attached pool sealed no forfait: the walk never reached it, so the gateway assertion proves nothing")
			}
			if got := bundle.APIKeys[secrets.ProviderOpenAICompatible] != ""; got != row.sealed {
				t.Fatalf("attached gateway entry sealed = %v, want %v", got, row.sealed)
			}
		})
	}
}

// W13 (R2): a gateway key is not a default LLM credential — it funds the
// routes the walk attributes to it, never an unpinned one. A mixed run whose
// team holds ONLY its gateway key still reaches the legacy pool for its vendor
// route, on the launch and in the preview alike.
func TestGatewayKey_AGatewayKeyDoesNotCloseTheLegacyPool(t *testing.T) {
	f := newPoolFixture(t, credpool.Limits{})
	f.pub.identity = &fakeTeamResolver{orgs: map[string]string{poolTeam: poolOrg}, orgDocs: map[string]identity.Org{poolOrg: {ID: poolOrg}}}
	f.pub.apiKeys = secrets.NewMemoryApiKeyStore()
	seedGatewayKey(t, f.pub.apiKeys, f.sealer, poolTeam, nil)
	wf := &ir.Workflow{Nodes: map[string]ir.Node{
		"gw":     &ir.AgentNode{BaseNode: ir.BaseNode{ID: "gw"}, LLMFields: ir.LLMFields{Backend: "claw", Model: "openai_compatible/team/m"}},
		"review": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "review"}, LLMFields: ir.LLMFields{Backend: "claw", Model: "anthropic/claude-opus-5"}},
	}}
	ctx := store.WithTenant(context.Background(), poolTeam)
	creds, err := f.pub.resolveAndSealCredentials(ctx, "run-gw-13", poolOrg, poolTeam, "owner1", gwBot, "", wf, nil, nil, model.RouteView{Workflow: wf}, nil, store.RunTrustDefault, nil, nil, nil, boundaryDerived)
	if err != nil {
		t.Fatalf("resolveAndSealCredentials: %v", err)
	}
	b := bundleFrom(t, f.pub, ctx, creds, "run-gw-13")
	if b.APIKeys[secrets.ProviderOpenAICompatible] != gwTeamKey {
		t.Fatalf("gateway slot = %q: the team's key must fund the gateway route", b.APIKeys[secrets.ProviderOpenAICompatible])
	}
	if creds.grant == nil || len(b.OAuthCredentials["claude_code"]) == 0 {
		t.Fatalf("pool grant = %v: the gateway key closed the legacy pool for the run's anthropic route", creds.grant)
	}
	preview, err := previewReadOnly(f.pub).PreviewCredentials(ctx, previewSpec(poolTeam, "owner1"), wf)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if got := selectedDescriptions(preview); !slices.Contains(got, "pool:oauth:claude_code") {
		t.Fatalf("preview selected %v, want the pool's forfait the launch draws", got)
	}
}

// W14 (R2): the fallback door opens only for a run that holds a DEFAULT
// credential of its own. A run whose every route rides the gateway and whose
// only credential is the team's gateway key stays shut out, as it was while
// env-funded: the door would otherwise ask a consenting donor for an openai
// key to cross the gateway node wholesale (claw+openai_key).
func TestGatewayKey_AGatewayOnlyRunNeverReachesTheFallbackDoor(t *testing.T) {
	sealer, err := secrets.NewKeyRingSealer(map[string][]byte{"default": make([]byte, 32)}, "default")
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	ctx := context.Background()
	keys := secrets.NewMemoryApiKeyStore()
	seedGatewayKey(t, keys, sealer, poolTeam, nil)
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
	p := &Publisher{apiKeys: keys, runSecrets: secrets.NewMemoryRunSecretsStore(), sealer: sealer, credPool: broker, logger: testLogger(),
		identity: &fakeTeamResolver{orgs: map[string]string{poolTeam: poolOrg}, orgDocs: map[string]identity.Org{poolOrg: {ID: poolOrg}}}}
	wf := &ir.Workflow{Nodes: map[string]ir.Node{"gw": &ir.AgentNode{
		BaseNode:  ir.BaseNode{ID: "gw"},
		LLMFields: ir.LLMFields{Backend: "claw", Model: "openai_compatible/team/m"},
		Tools:     []string{},
	}}}
	policy := &store.RunLLMRoutePolicy{
		PairOrder: []string{llmroute.Pair(llmroute.HarnessClaw, "openai_compatible_key"), llmroute.Pair(llmroute.HarnessClaw, "openai_key")},
		Triggers:  []string{llmroute.TriggerUsageWindow},
	}
	tctx := store.WithTenant(ctx, poolTeam)
	creds, err := p.resolveAndSealCredentials(tctx, "run-gw-14", poolOrg, poolTeam, "owner1", gwBot, "", wf, nil, nil, model.RouteView{Workflow: wf}, nil, store.RunTrustDefault, nil, policy, nil, boundaryDerived)
	if err != nil {
		t.Fatalf("resolveAndSealCredentials: %v", err)
	}
	b := bundleFrom(t, p, tctx, creds, "run-gw-14")
	if b.APIKeys[secrets.ProviderOpenAICompatible] != gwTeamKey {
		t.Fatalf("gateway slot = %q: the run must be key-funded, or the door question is moot", b.APIKeys[secrets.ProviderOpenAICompatible])
	}
	if creds.doorGrant != nil || b.APIKeys[secrets.ProviderOpenAI] != "" {
		t.Fatalf("door grant = %v, openai slot = %q: a gateway-only run reached the fallback door", creds.doorGrant, b.APIKeys[secrets.ProviderOpenAI])
	}
	spec := previewSpec(poolTeam, "owner1")
	spec.LLMRoutePolicy = policy
	preview, err := previewReadOnly(p).PreviewCredentials(tctx, spec, wf)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if preview.Pool.DoorReason != "" || len(preview.Pool.DoorWants) != 0 {
		t.Fatalf("preview door = %q %v: the launch never arms the door for this run", preview.Pool.DoorReason, preview.Pool.DoorWants)
	}
}

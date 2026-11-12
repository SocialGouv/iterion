package runner

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/compatgw"
	"github.com/SocialGouv/iterion/pkg/backend/credroute"
	"github.com/SocialGouv/iterion/pkg/backend/modelspecs"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/parser/parsertest"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/pricefeed"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/server/cloudpublisher"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ADR-163: the bundle's price-feed table plants on the run's environment
// at the one composition point every credential read already crosses —
// the ctx injectCredentials returns is the ctx the engine dispatches on,
// so the catalog the dispatch and the meter read answers from it. A
// bundle without the field plants nothing (older publishers, local
// runs): the name stays unset and the operator table is the floor.
func TestInjectCredentials_PlantsThePriceFeedTable(t *testing.T) {
	// The wire form, in the package's own seconds spelling.
	rawBytes, err := json.Marshal(pricefeed.Table{Entries: map[string]pricefeed.Entry{
		"glm-5.3": {Feed: "scw", RefreshedAt: time.Now(), FreshFor: pricefeed.D(2 * time.Hour),
			Spec: modelspecs.Spec{InputCostPerM: 0.42, OutputCostPerM: 1.4}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	raw := string(rawBytes)
	if pricefeed.ParseTable(raw) == nil {
		t.Fatal("the fixture table does not parse")
	}

	rs := secrets.NewMemoryRunSecretsStore()
	dek, err := secrets.NewRunBundleDEK()
	if err != nil {
		t.Fatal(err)
	}
	dekSealer, err := secrets.NewAESGCMSealer(dek)
	if err != nil {
		t.Fatal(err)
	}
	sealed, keyID, err := secrets.SealRunBundle(dekSealer, "team-a", "", "run-1", secrets.RunBundle{GatewayPriceFeed: json.RawMessage(raw)})
	if err != nil {
		t.Fatal(err)
	}
	if err := rs.Put(context.Background(), secrets.RunSecretsRecord{ID: "ref-1", TenantID: "team-a", RunID: "run-1", KeyID: keyID, SealedBundle: sealed}); err != nil {
		t.Fatal(err)
	}
	r := &Runner{cfg: Config{Logger: iterlog.Nop(), RunSecrets: rs}}
	msg := &queue.RunMessage{RunID: "run-1", TenantID: "team-a", SecretsRef: "ref-1", BundleDEK: dek}

	ctx, _, err := r.injectCredentials(context.Background(), msg)
	if err != nil {
		t.Fatal(err)
	}
	if got := credroute.Getenv(ctx, compatgw.FeedTableEnv); got != raw {
		t.Fatalf("the feed table did not plant on the run's environment: %q", got)
	}
	// The name answers the catalog's own read path through the ctx —
	// the resolution the dispatch performs.
	t.Setenv(compatgw.CatalogModelsEnv, `{"glm-5.3":{"input_usd_per_mtok":9,"output_usd_per_mtok":9}}`)
	t.Setenv(compatgw.CatalogProviderEnv, "")
	t.Setenv(compatgw.ResolvedEnvName, "")
	compatgw.ResetCatalogCaches()
	res := compatgw.ResolveCatalog("glm-5.3", nil, func(name string) string { return credroute.Getenv(ctx, name) })
	if res.Source != compatgw.SourceFeed("scw") || res.Spec.InputCostPerM != 0.42 {
		t.Fatalf("the dispatch-side resolution did not answer from the planted table: %+v", res)
	}

	// A bundle without the field plants nothing: the name stays unset
	// (not empty-planted) and the operator floor answers.
	sealedPlain, keyID2, err := secrets.SealRunBundle(dekSealer, "team-a", "", "run-2", secrets.RunBundle{})
	if err != nil {
		t.Fatal(err)
	}
	if err := rs.Put(context.Background(), secrets.RunSecretsRecord{ID: "ref-2", TenantID: "team-a", RunID: "run-2", KeyID: keyID2, SealedBundle: sealedPlain}); err != nil {
		t.Fatal(err)
	}
	ctx2, _, err := r.injectCredentials(context.Background(), &queue.RunMessage{RunID: "run-2", TenantID: "team-a", SecretsRef: "ref-2", BundleDEK: dek})
	if err != nil {
		t.Fatal(err)
	}
	if v, set := credroute.LookupEnv(ctx2, compatgw.FeedTableEnv); set {
		t.Fatalf("a bundle without the table planted the name: %q", v)
	}
	res2 := compatgw.ResolveCatalog("glm-5.3", nil, func(name string) string { return credroute.Getenv(ctx2, name) })
	if res2.Source != compatgw.SourceOperator || res2.Spec.InputCostPerM != 9 {
		t.Fatalf("a feedless bundle did not leave the operator floor: %+v", res2)
	}
}

// Round 1's HIGH 3, witnessed: the org-cap / credential-ledger meter
// (rateForLocked) prices a gateway model through the run's OWN context —
// the one injectCredentials planted the sealed feed table on — not the
// runner's process environment (which carries no table, or the operator
// floor).
func TestMetricsEmitterPricesGatewayThroughTheRunContext(t *testing.T) {
	table := `{"entries":{"glm-5.3":{"feed":"scw","refreshed_at":"` + time.Now().UTC().Format(time.RFC3339) + `","fresh_for":7200,"spec":{"input_cost_per_m":0.42,"output_cost_per_m":1.4}}}}`
	ctx := compatgw.OverlayFeedTable(context.Background(), []byte(table))
	usage := newMetricsEmitter(nil, nil, nil, ctx)
	r := usage.rateForLocked("openai_compatible/glm-5.3")
	if !r.known {
		t.Fatal("the emitter did not resolve the gateway model through the run ctx")
	}
	// 1M input at 0.42/mtok = $0.42.
	if diff := r.inputUSDPerToken - 0.42/1e6; diff > 1e-12 || diff < -1e-12 {
		t.Fatalf("the emitter priced input at %v per token, the feed table says 0.42e-6", r.inputUSDPerToken)
	}
	// The process-floor twin (nil ctx) prices from the env: unpriced here.
	plain := newMetricsEmitter(nil, nil, nil, nil)
	if r := plain.rateForLocked("openai_compatible/glm-5.3"); r.known {
		t.Fatalf("the nil-ctx emitter priced a model the process env does not carry: %+v", r)
	}
}

// The leg pkg/server's pool-retirement E2E
// (TestPriceFeedEndToEndRetiresThePool) cannot drive: the runner's own open
// of what the real publisher sealed for a team that holds its gateway key.
// The publisher (cloudpublisher.NewForTesting, the queue left out) launches
// on the default runner, its fold pkg/pricefeed's Fold over a platform
// declaration; the message it publishes goes to injectCredentials. The
// run's context carries the sealed key and base URL, the ambient refusal of
// the pod's environment, and the table the fold answered — from which a
// runner with no OPENAI_COMPATIBLE_* table answers deepseek's accepted
// efforts and glm-5.3's window, output cap and prices from the declaration.
func TestInjectCredentials_OpensThePublishersTeamGatewayLaunch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("ITERION_HOME", t.TempDir())
	for _, name := range []string{
		compatgw.CatalogModelsEnv, compatgw.ReasoningEffortsEnv, compatgw.CatalogProviderEnv,
		compatgw.FeedTableEnv, compatgw.ResolvedEnvName,
	} {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
	compatgw.ResetCatalogCaches()
	t.Cleanup(compatgw.ResetCatalogCaches)

	const (
		teamKey  = "sk-team-a-gateway"
		teamURL  = "https://gw.example"
		deepseek = "scaleway/deepseek-v4.1-flash"
	)
	ctx := context.Background()
	sealer, err := secrets.NewKeyRingSealer(map[string][]byte{"default": make([]byte, 32)}, "default")
	if err != nil {
		t.Fatal(err)
	}
	keys := secrets.NewMemoryApiKeyStore()
	key := secrets.ApiKey{
		ID: secrets.NewApiKeyID(), TenantID: "team-a", ScopeTeamID: "team-a",
		Provider: secrets.ProviderOpenAICompatible, Name: "team-a gateway", BaseURL: teamURL,
		Fingerprint: secrets.APIKeyFingerprint(secrets.ProviderOpenAICompatible, teamKey, teamURL),
		CreatedAt:   time.Now().UTC(),
	}
	if key.SealedSecret, err = secrets.SealAPIKeyFor(sealer, key, []byte(teamKey)); err != nil {
		t.Fatal(err)
	}
	if err := keys.Create(ctx, key); err != nil {
		t.Fatal(err)
	}
	// The platform's declaration, folded by pkg/pricefeed's Fold for the
	// ids the publisher names, as the server's fold folds it.
	platform := pricefeed.ScopeState{Label: "platform", Config: &pricefeed.ConfigRecord{UpdatedAt: time.Now().UTC(), Feeds: []pricefeed.FeedConfig{{
		Name: "pliage", Type: pricefeed.TypeDeclared, Enabled: true,
		Interval: pricefeed.D(30 * 24 * time.Hour), Timeout: pricefeed.D(time.Second),
		Params: map[string]string{"entries": `{"scaleway/deepseek-v4.1-flash":{"context_window":128000,"max_output_tokens":8192,"input_usd_per_mtok":0.6,"output_usd_per_mtok":2.4,"reasoning_efforts":["low","high","xhigh","max"]},"glm-5.3":{"context_window":256000,"max_output_tokens":16384,"input_usd_per_mtok":2.01,"output_usd_per_mtok":6.15}}`},
	}}}}
	var mu sync.Mutex
	var rawTable json.RawMessage
	fold := func(_ context.Context, _, _ string, named []string) (json.RawMessage, error) {
		raw, err := json.Marshal(pricefeed.Fold(pricefeed.FoldConfig{Now: time.Now(), Named: named}, platform))
		if err != nil {
			t.Errorf("the fold does not encode: %v", err)
			return nil, nil
		}
		mu.Lock()
		defer mu.Unlock()
		rawTable = raw
		return raw, nil
	}
	runSecrets := secrets.NewMemoryRunSecretsStore()
	runs, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var published []*queue.RunMessage
	pub := cloudpublisher.NewForTesting(cloudpublisher.Config{
		Store: runs, ApiKeys: keys, RunSecrets: runSecrets, Sealer: sealer, Logger: iterlog.Nop(),
		PriceFeedEffective: fold,
	}, func(_ context.Context, msg *queue.RunMessage) error {
		mu.Lock()
		defer mu.Unlock()
		published = append(published, msg)
		return nil
	})
	const src = `agent deep:
  backend: claw
  model: "openai_compatible/scaleway/deepseek-v4.1-flash"
  reasoning_effort: medium
  user: "Review the change."

agent glm:
  backend: claw
  model: "openai_compatible/glm-5.3"
  user: "Summarize the review."

workflow w:
  worktree: none
  sandbox: none
  entry: deep
  deep -> glm
  glm -> done
`
	pr := parsertest.Parse("retire.bot", src)
	for _, d := range pr.Diagnostics {
		if d.Severity == parser.SeverityError {
			t.Fatalf("fixture does not parse: %s", d.Error())
		}
	}
	cr := ir.Compile(pr.File)
	if cr.HasErrors() {
		t.Fatalf("fixture does not compile: %v", cr.Diagnostics)
	}
	if _, err := pub.SubmitLaunch(store.WithIdentity(ctx, "team-a", "owner-a"), "run-gw-feed", runview.LaunchSpec{Source: src}, cr.Workflow, nil); err != nil {
		t.Fatalf("the launch was refused: %v", err)
	}
	mu.Lock()
	msgs := append([]*queue.RunMessage(nil), published...)
	sealedTable := rawTable
	mu.Unlock()
	if sealedTable == nil {
		t.Fatal("the publisher's fold answered no table")
	}
	if len(msgs) != 1 || msgs[0].SecretsRef == "" || msgs[0].RunnerPool != "" {
		t.Fatalf("the launch published %+v, want one default-runner message naming a sealed bundle", msgs)
	}

	r := &Runner{cfg: Config{Logger: iterlog.Nop(), RunSecrets: runSecrets}}
	runCtx, cleanup, err := r.injectCredentials(ctx, msgs[0])
	if err != nil {
		t.Fatalf("injectCredentials: %v", err)
	}
	if cleanup != nil {
		defer cleanup()
	}
	creds, ok := secrets.CredentialsFromContext(runCtx)
	if !ok {
		t.Fatal("the run's context carries no credentials")
	}
	if creds.APIKeys[secrets.ProviderOpenAICompatible] != teamKey || creds.APIBaseURLs[secrets.ProviderOpenAICompatible] != teamURL {
		t.Errorf("the run's gateway credential is %q at %q, want the team's sealed key at %s",
			creds.APIKeys[secrets.ProviderOpenAICompatible], creds.APIBaseURLs[secrets.ProviderOpenAICompatible], teamURL)
	}
	if !creds.AmbientRefused {
		t.Error("the run's context does not refuse the pod's environment: its gateway routes could fall to the runner's env pair")
	}
	get := func(name string) string { return credroute.Getenv(runCtx, name) }
	if got := get(compatgw.FeedTableEnv); got != string(sealedTable) {
		t.Fatalf("the run's context carries table %q, the publisher's fold answered %s", got, sealedTable)
	}
	if values, source := compatgw.AcceptedEfforts(deepseek, get, t.Errorf); !slices.Equal(values, []string{"low", "high", "xhigh", "max"}) || source != compatgw.SourceFeed("pliage") {
		t.Errorf("deepseek's accepted efforts = %v from %q, want the declaration's from feed:pliage", values, source)
	}
	res := compatgw.ResolveCatalog("glm-5.3", nil, get)
	if res.Source != compatgw.SourceFeed("pliage") || res.Spec.ContextWindow != 256000 || res.Spec.MaxOutputTokens != 16384 ||
		res.Spec.InputCostPerM != 2.01 || res.Spec.OutputCostPerM != 6.15 {
		t.Errorf("glm-5.3 resolves %+v, want the declaration's window, output cap and prices from feed:pliage", res)
	}
}

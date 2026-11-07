package runner

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/compatgw"
	"github.com/SocialGouv/iterion/pkg/backend/credroute"
	"github.com/SocialGouv/iterion/pkg/backend/modelspecs"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/pricefeed"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/secrets"
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

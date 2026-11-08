package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/compatgw"
	"github.com/SocialGouv/iterion/pkg/backend/cost"
	"github.com/SocialGouv/iterion/pkg/backend/credroute"
	"github.com/SocialGouv/iterion/pkg/backend/modelroute"
	"github.com/SocialGouv/iterion/pkg/pricefeed"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// The price feed's END-TO-END chain (ADR-163), deterministic, no model
// spend: the tenant stores → the manual refresh (the reconciler's own
// body) → the launch-time fold → the sealed bundle's round trip → the
// runner's environment overlay → the catalog's feed rung → the meter's
// price → the provenance that rides the node. Every hop is the
// production one; only the source is an exec script and the stores are
// the memory twins the conformance suite pins.
func TestPriceFeedEndToEndRunChain(t *testing.T) {
	s, _ := priceFeedFixtureServer(t) // arms the exec hatch for the fixture feed

	// 1. The operator declares a feed; the manual sync fills its state.
	script := writeTariffScript(t, 0.42, 1.4)
	docID := pricefeed.TeamConfigDocID("team1")
	if err := s.priceFeedConfigs.Put(context.Background(), docID, &pricefeed.ConfigRecord{Feeds: []pricefeed.FeedConfig{priceFeedFeed(script)}}); err != nil {
		t.Fatal(err)
	}
	outcome, err := s.priceFeedRefresh(context.Background(), docID, priceFeedFeed(script))
	if err != nil || outcome.Status != pricefeed.StateOK {
		t.Fatalf("the sync did not fill the feed: %+v err %v", outcome, err)
	}

	// 2. The launch-time fold answers the table the bundle carries. The
	// operator floor (this process's env) is set BELOW the feed's price:
	// the fold is what decides, not the deployment's cheaper table.
	t.Setenv(compatgw.CatalogModelsEnv, `{"glm-5.3":{"context_window":131072,"input_usd_per_mtok":9,"output_usd_per_mtok":9}}`)
	t.Setenv(compatgw.CatalogProviderEnv, "")
	t.Setenv(compatgw.ResolvedEnvName, "")
	t.Setenv(compatgw.FeedTableEnv, "")
	compatgw.ResetCatalogCaches()
	raw := EffectivePriceFeed(context.Background(), s.priceFeedConfigs, s.priceFeedStates, "org1", "team1", []string{"glm-5.3"}, nil)
	if raw == nil {
		t.Fatal("the launch fold answered no table")
	}

	// 3. The bundle seals and reopens with the table verbatim — the
	// runner's one channel that needs no database.
	sealer, err := secrets.NewAESGCMSealer([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	sealed, keyID, err := secrets.SealRunBundle(sealer, "team1", "", "run-1", secrets.RunBundle{GatewayPriceFeed: raw})
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := secrets.OpenRunBundle(sealer, "team1", "", "run-1", keyID, sealed)
	if err != nil {
		t.Fatal(err)
	}
	if string(bundle.GatewayPriceFeed) != string(raw) {
		t.Fatalf("the table did not survive the seal: %s vs %s", bundle.GatewayPriceFeed, raw)
	}

	// 4. The runner plants it on the run's environment — the ONE
	// bundle-to-context adapter the runner itself calls.
	ctx := compatgw.OverlayFeedTable(context.Background(), bundle.GatewayPriceFeed)
	get := func(name string) string {
		value, _ := credroute.LookupEnv(ctx, name)
		return value
	}

	// 5. The dispatch-side catalog resolution answers from the feed,
	// before the operator table, with the feed's identity and age.
	res := compatgw.ResolveCatalog("glm-5.3", nil, get)
	if res.Source != compatgw.SourceFeed("scw") {
		t.Fatalf("the dispatch did not resolve from the feed: %+v", res)
	}
	if res.Spec.InputCostPerM != 0.42 || res.Spec.OutputCostPerM != 1.4 {
		t.Fatalf("the feed's prices did not answer: %+v", res.Spec)
	}

	// 6. The meter prices the SAME usage at the SAME table through the
	// SAME environment — the stamp and the price cannot disagree.
	const in, out, cache = 200_000, 100_000, 800_000
	priced := cost.EstimateUSDWithCacheInEnv(ctx, modelroute.OpenAICompatible+"/glm-5.3", in, out, cache)
	// The script's table publishes no cache-read rate, so cached input
	// prices at the input rate — the pre-split arithmetic, bit for bit.
	want := (float64(in)*0.42 + float64(out)*1.4) / 1e6
	if priced != want {
		t.Fatalf("the meter priced at %v, the feed table says %v", priced, want)
	}
	// The env-less meter (the process floor) answers differently — the
	// divergence the ctx-aware form exists to close.
	if floored := cost.EstimateUSDWithCache("openai_compatible/glm-5.3", in, out, cache); floored == priced {
		t.Fatalf("the process floor priced identically to the feed: %v", floored)
	}

	// 7. The record the crossing carries names the feed and its age, and
	// the container reading it back resolves the same answer.
	forward := compatgw.ForwardableRecord("glm-5.3", res)
	if forward.Source != compatgw.SourceFeed("scw") || forward.RefreshedAt == nil {
		t.Fatalf("the forwardable record lost the feed provenance: %+v", forward)
	}
	containerEnv := map[string]string{compatgw.ResolvedEnvName: marshalJSON(t, forward)}
	containerGet := func(name string) string { return containerEnv[name] }
	back := compatgw.ResolveCatalog("glm-5.3", nil, containerGet)
	if back.Source != compatgw.SourceFeed("scw") || back.Spec.InputCostPerM != 0.42 {
		t.Fatalf("the in-container catalog did not answer from the record: %+v", back)
	}

	// 8. Staleness is judged at the FOLD, and the fold's judgment travels:
	// the same table aged past its window still answers for THIS run
	// (the invocation holds the answer it was dispatched with), while a
	// NEW fold over the aged STATE answers nothing — the operator floor.
	aged := pricefeed.ParseTable(string(raw))
	for id, e := range aged.Entries {
		e.RefreshedAt = time.Now().Add(-3 * time.Hour)
		aged.Entries[id] = e
	}
	agedJSON := marshalJSON(t, aged)
	agedCtx := compatgw.OverlayFeedTable(context.Background(), []byte(agedJSON))
	agedGet := func(name string) string {
		value, _ := credroute.LookupEnv(agedCtx, name)
		return value
	}
	if aged := compatgw.ResolveCatalog("glm-5.3", nil, agedGet); aged.Source != compatgw.SourceFeed("scw") || aged.Spec.InputCostPerM != 0.42 {
		t.Fatalf("the run's sealed table stopped answering mid-invocation: %+v", aged)
	}
	// The FOLD over the aged state refuses: the next run folds fresh.
	agedState, err := s.priceFeedStates.Get(context.Background(), pricefeed.StateDocID(pricefeed.TeamConfigDocID("team1"), "scw"))
	if err != nil || agedState == nil {
		t.Fatalf("no state to age: %+v err %v", agedState, err)
	}
	agedState.RefreshedAt = time.Now().Add(-3 * time.Hour)
	if err := s.priceFeedStates.Put(context.Background(), pricefeed.StateDocID(pricefeed.TeamConfigDocID("team1"), "scw"), agedState); err != nil {
		t.Fatal(err)
	}
	if stale := EffectivePriceFeed(context.Background(), s.priceFeedConfigs, s.priceFeedStates, "org1", "team1", []string{"glm-5.3"}, nil); stale != nil {
		t.Fatalf("a fold over aged state answered a table: %s", stale)
	}
}

func marshalJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

package server

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/compatgw"
	"github.com/SocialGouv/iterion/pkg/backend/cost"
	"github.com/SocialGouv/iterion/pkg/backend/credroute"
	"github.com/SocialGouv/iterion/pkg/pricefeed"
)

// The actual LiteLLM adapter feeds the member-readable effective view and
// the runner's meter. No pre-normalized spec can hide a broken mapping here.
func TestLiteLLMPriceFeedEffectiveCacheValuation(t *testing.T) {
	fixture, err := os.ReadFile("../pricefeed/testdata/litellm-model-info.json")
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fixture)
	}))
	t.Cleanup(upstream.Close)
	t.Setenv("ITERION_LLM_ENDPOINT_ALLOW_PRIVATE", "1")
	for _, key := range []string{compatgw.CatalogModelsEnv, compatgw.CatalogProviderEnv, compatgw.ResolvedEnvName, compatgw.FeedTableEnv} {
		t.Setenv(key, "")
	}
	live := newPriceFeedLive(t)
	feed := pricefeed.FeedConfig{
		Name: "litellm", Type: pricefeed.TypeLiteLLM, Enabled: true,
		Interval: pricefeed.D(time.Hour), Timeout: pricefeed.D(time.Second),
		Params: map[string]string{"url": upstream.URL + "/model/info"},
	}
	docID := pricefeed.TeamConfigDocID("team-pf")
	putFeedDoc(t, live.configs, docID, feed)
	outcome, err := live.s.priceFeedRefresh(context.Background(), docID, feed)
	if err != nil || outcome.Status != pricefeed.StateOK {
		t.Fatalf("LiteLLM refresh: %+v, %v", outcome, err)
	}
	const model = "scaleway/glm-5.3"
	code, body := live.do(t, "GET", "/api/teams/team-pf/price-feeds/effective?model="+model, live.viewer, "")
	if code != http.StatusOK {
		t.Fatalf("effective: %d %s", code, body)
	}
	var view priceFeedEffectiveWire
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatal(err)
	}
	if len(view.Models) != 1 || view.Models[0].ID != model || view.Models[0].Entry == nil {
		t.Fatalf("effective model missing: %s", body)
	}
	spec := view.Models[0].Entry.Spec
	if spec.ContextWindow != 262144 || spec.MaxOutputTokens != 32768 {
		t.Fatalf("effective limits: %+v", spec)
	}
	if math.Abs(spec.CacheReadCostPerM-0.12) > 1e-12 {
		t.Fatalf("effective cache-read price: %+v", spec)
	}
	ctx := compatgw.OverlayFeedTable(context.Background(), view.Table)
	resolved := compatgw.ResolveCatalog(model, nil, func(key string) string { return credroute.Getenv(ctx, key) })
	if resolved.Source != compatgw.SourceFeed(feed.Name) || resolved.Spec.ContextWindow != 262144 || resolved.Spec.MaxOutputTokens != 32768 {
		t.Fatalf("runner catalog: %+v", resolved)
	}
	// Input includes 800k cached tokens: charging their full input price
	// would produce $0.79 instead of $0.398.
	got := cost.EstimateUSDWithCacheInEnv(ctx, "openai_compatible/"+model, 1_000_000, 100_000, 800_000)
	const want = (200_000*0.61 + 800_000*0.12 + 100_000*1.8) / 1e6
	if math.Abs(got-want) > 1e-12 {
		t.Fatalf("cached usage priced at %.12f, want %.12f", got, want)
	}
}

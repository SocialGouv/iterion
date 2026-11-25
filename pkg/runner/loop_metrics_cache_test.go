package runner

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/compatgw"
	"github.com/SocialGouv/iterion/pkg/backend/cost"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The org bucket (RunTotals), the credential ledger and the Prometheus cost
// counter price a Chat Completions step's cached share at the catalog's
// cache-read rate, the figure the run meter (EstimateUSDWithCacheInEnv)
// reports for the same usage. An entry without a cache price keeps pricing
// every input token at the input rate.
func TestMetricsEmitterPricesCachedTokensLikeTheRunMeter(t *testing.T) {
	for _, key := range []string{compatgw.CatalogModelsEnv, compatgw.CatalogProviderEnv, compatgw.ResolvedEnvName, compatgw.FeedTableEnv} {
		t.Setenv(key, "")
	}
	const model = "openai_compatible/scaleway/glm-5.3"
	for _, tc := range []struct {
		name      string
		cacheRate string
		want      float64
	}{
		// 200k uncached at 0.61 + 800k cached at 0.12 + 100k output at 1.8.
		{"cache price", `,"cache_read_cost_per_m":0.12`, 0.398},
		// 1M input at 0.61 + 100k output at 1.8.
		{"no cache price", ``, 0.79},
	} {
		t.Run(tc.name, func(t *testing.T) {
			table := `{"entries":{"scaleway/glm-5.3":{"feed":"litellm","refreshed_at":"` + time.Now().UTC().Format(time.RFC3339) + `","fresh_for":7200,"spec":{"input_cost_per_m":0.61,"output_cost_per_m":1.8` + tc.cacheRate + `}}}}`
			ctx := compatgw.OverlayFeedTable(context.Background(), []byte(table))
			runMeter := cost.EstimateUSDWithCacheInEnv(ctx, model, 1_000_000, 100_000, 800_000)
			if math.Abs(runMeter-tc.want) > 1e-9 {
				t.Fatalf("run meter = %v, want %v", runMeter, tc.want)
			}
			usage := newMetricsEmitter(nil, nil, nil, ctx)
			usage.observe(store.Event{Type: store.EventDelegateStarted, NodeID: "n", Data: map[string]interface{}{"declared_model": model}})
			usage.observe(store.Event{Type: store.EventLLMStepFinished, NodeID: "n", Data: map[string]interface{}{
				"input_tokens": float64(1_000_000), "output_tokens": float64(100_000), "cache_read_tokens": float64(800_000),
			}})
			org, _, _, _ := usage.RunTotals()
			if math.Abs(org-runMeter) > 1e-9 {
				t.Fatalf("org bucket charged %v, the run meter reports %v", org, runMeter)
			}
		})
	}
}

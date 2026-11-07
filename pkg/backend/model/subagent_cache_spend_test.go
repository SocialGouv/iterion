package model

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/compatgw"
	"github.com/SocialGouv/iterion/pkg/backend/cost"
)

// A child conversation metered through SubagentSpend.Record must price its
// cache read exactly like the results the backend books: Record feeds
// subagentSpendFold, which adds the child's costUSD to the node's
// Result.CostUSD — the budget reads. A child served from cache priced at
// the full input rate over-charges the run's budget on every delegation.
func TestSubagentSpendRecord_PricesTheCacheReadItCarries(t *testing.T) {
	stub := func(string) (float64, float64, float64, bool) { return 0.60, 1.80, 0.12, true }
	cost.SetGatewayPricer(stub, stub)
	// Restore the production wiring, never nil: compatgw's init cabled the
	// real pricers for this whole test binary, and a nil pricer would price
	// every later test's gateway id at 0.
	t.Cleanup(func() { cost.SetGatewayPricer(compatgw.StaticPricer, compatgw.StaticPricerQuiet) })
	s := &SubagentSpend{}
	usage := Usage{InputTokens: 1_000_000, OutputTokens: 100_000, CacheReadTokens: 800_000}
	s.Record("openai_compatible/gw/m", usage)
	_, got := s.Total()
	want := cost.EstimateUSDWithCache("openai_compatible/gw/m", usage.InputTokens, usage.OutputTokens, usage.CacheReadTokens)
	if got != want {
		t.Errorf("Record priced the child at %v, the cache-aware booking paths price the SAME usage at %v — over-charge %v",
			got, want, got-want)
	}
}

package compatgw

import (
	"encoding/json"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/cost"
	"github.com/SocialGouv/iterion/pkg/backend/modelspecs"
)

// The operator table may carry a cache-read rate below the input rate —
// Scaleway bills deepseek-v4.1-flash cached input at a fifth of uncached.
// The field is optional: an entry without one resolves with a zero cache
// rate, which the estimator reads as "price cache at the input rate".
func TestOperatorModels_CarriesTheCacheReadRate(t *testing.T) {
	ResetCatalogCaches()
	get := mapGet(map[string]string{CatalogModelsEnv: `{"gw/m":{"input_usd_per_mtok":0.6,"output_usd_per_mtok":1.8,"cache_read_usd_per_mtok":0.12}}`})
	res := ResolveCatalog("gw/m", nil, get)
	if res.Source != SourceOperator || !res.Priced() {
		t.Fatalf("resolved %+v, want the operator record, priced", res)
	}
	if res.Spec.CacheReadCostPerM != 0.12 {
		t.Errorf("CacheReadCostPerM = %v, want 0.12 — the operator's cache rate fell off the spec", res.Spec.CacheReadCostPerM)
	}
	ResetCatalogCaches()
	get = mapGet(map[string]string{CatalogModelsEnv: `{"gw/m":{"input_usd_per_mtok":0.6,"output_usd_per_mtok":1.8}}`})
	res = ResolveCatalog("gw/m", nil, get)
	if !res.Priced() || res.Spec.CacheReadCostPerM != 0 {
		t.Fatalf("resolved %+v, want priced with a zero cache rate", res)
	}
}

// The forwarded record crosses the host→container IPC with its cache rate:
// the container's estimator must see the same figure the host priced with,
// or the two sides of one run disagree on what a cached turn cost.
func TestResolveCatalog_TheForwardedRecordCarriesTheCacheRate(t *testing.T) {
	ResetCatalogCaches()
	get := mapGet(map[string]string{
		CatalogModelsEnv: operatorTableJSON,
		ResolvedEnvName:  `{"model":"gw/m","source":"operator","input_usd_per_mtok":0.6,"output_usd_per_mtok":1.8,"cache_read_usd_per_mtok":0.12}`,
	})
	res := ResolveCatalog("gw/m", nil, get)
	if res.Spec.CacheReadCostPerM != 0.12 {
		t.Errorf("CacheReadCostPerM = %v, want 0.12 — the forwarded record lost the cache rate", res.Spec.CacheReadCostPerM)
	}
	// A record written before the field existed unmarshals with a zero
	// cache rate: old hosts stay compatible, and the estimator's
	// zero-means-input-rate fallback covers the gap.
	ResetCatalogCaches()
	get = mapGet(map[string]string{
		CatalogModelsEnv: operatorTableJSON,
		ResolvedEnvName:  `{"model":"gw/m","source":"operator","input_usd_per_mtok":0.6,"output_usd_per_mtok":1.8}`,
	})
	if res := ResolveCatalog("gw/m", nil, get); res.Spec.CacheReadCostPerM != 0 {
		t.Errorf("CacheReadCostPerM = %v, want 0 for a record without the field", res.Spec.CacheReadCostPerM)
	}
}

// End to end through the real catalog: the estimator's gateway branch
// splits the input between the uncached share at the input rate and the
// cached share at the operator's cache rate.
func TestEstimateUSDWithCache_TheGatewayBranchSplitsTheCachedShare(t *testing.T) {
	ResetCatalogCaches()
	t.Setenv(CatalogModelsEnv, `{"gw/m":{"input_usd_per_mtok":0.6,"output_usd_per_mtok":1.8,"cache_read_usd_per_mtok":0.12}}`)
	t.Setenv(CatalogProviderEnv, "")
	t.Setenv(ResolvedEnvName, "")
	// 200k uncached * 0.6 + 800k cached * 0.12 + 100k out * 1.8.
	want := 0.2*0.6 + 0.8*0.12 + 0.1*1.8
	if got := cost.EstimateUSDWithCache("openai_compatible/gw/m", 1_000_000, 100_000, 800_000); got != want {
		t.Errorf("EstimateUSDWithCache = %v, want %v", got, want)
	}
	// The pre-cache entry point prices the same call at the full input
	// rate: an operator who has not set a cache rate is unchanged.
	if got, plain := cost.EstimateUSD("openai_compatible/gw/m", 1_000_000, 100_000), cost.EstimateUSDWithCache("openai_compatible/gw/m", 1_000_000, 100_000, 0); got != plain {
		t.Errorf("EstimateUSD = %v, EstimateUSDWithCache(0) = %v — the delegates disagree", got, plain)
	}
}

// The IPC round trip is the seam where a field can die invisibly: the host
// marshals ForwardableRecord, the container unmarshals it into the
// resolution. Marshalling the host side and resolving through the record
// proves the cache rate survives the crossing — a one-sided test would let
// either half drop the field and still read its own write.
func TestForwardableRecord_RoundTripsTheCacheRate(t *testing.T) {
	ResetCatalogCaches()
	rec := ForwardableRecord("gw/m", Resolved{
		Spec: modelspecs.Spec{
			ContextWindow:     131072,
			InputCostPerM:     0.6,
			OutputCostPerM:    1.8,
			CacheReadCostPerM: 0.12,
		},
		Source: SourceOperator,
	})
	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal the forwarded record: %v", err)
	}
	get := mapGet(map[string]string{
		CatalogModelsEnv: operatorTableJSON,
		ResolvedEnvName:  string(raw),
	})
	res := ResolveCatalog("gw/m", nil, get)
	if res.Spec.CacheReadCostPerM != 0.12 {
		t.Errorf("CacheReadCostPerM = %v, want 0.12 — the cache rate died crossing the IPC", res.Spec.CacheReadCostPerM)
	}
}

// A negative cache rate is a typo, and the operator table's contract refuses
// typos loudly — "a price is negative" covers every price field or a
// mistyped cache rate is silently ignored (the estimator degrades it to the
// input rate) and the operator's mistake is never said.
func TestOperatorModels_ANegativeCacheRateRefusesTheTable(t *testing.T) {
	_, err := parseOperatorTable(`{"gw/m":{"input_usd_per_mtok":0.6,"output_usd_per_mtok":1.8,"cache_read_usd_per_mtok":-0.5}}`)
	if err == nil {
		t.Fatal("a negative cache rate parsed — the strict table must refuse it like any other negative price")
	}
}

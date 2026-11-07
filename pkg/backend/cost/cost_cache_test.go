package cost

import "testing"

// withGatewayPricer wires a stub catalog for the duration of one test. The
// production wiring lives in compatgw's init; here the stub IS the catalog.
func withGatewayPricer(t *testing.T, loud, quiet func(string, func(string) string) (float64, float64, float64, bool)) {
	t.Helper()
	prevLoud, prevQuiet := gatewayPricer, gatewayPricerQuiet
	gatewayPricer, gatewayPricerQuiet = loud, quiet
	t.Cleanup(func() {
		gatewayPricer, gatewayPricerQuiet = prevLoud, prevQuiet
	})
}

func constGatewayPricer(input, output, cacheRead float64, known bool) func(string, func(string) string) (float64, float64, float64, bool) {
	return func(string, func(string) string) (float64, float64, float64, bool) {
		return input, output, cacheRead, known
	}
}

// The parity case: a gateway whose catalog carries a cache-read rate prices
// cached prompt tokens below the input rate. Scaleway's deepseek-v4.1-flash
// bills cached input at €0.12/1M against €0.60/1M uncached — a metered run
// serving most of its prompt from cache was over-read at the full input
// rate until the cache read reached the estimator.
func TestEstimateUSDWithCache_PricesCachedInputBelowInput(t *testing.T) {
	withGatewayPricer(t,
		constGatewayPricer(0.60, 1.80, 0.12, true),
		constGatewayPricer(0.60, 1.80, 0.12, true))
	got := EstimateUSDWithCache("openai_compatible/deepseek-v4.1-flash-1017", 1_000_000, 0, 800_000)
	// 200k uncached * 0.60 + 800k cached * 0.12 per million.
	want := 0.2*0.60 + 0.8*0.12
	if got != want {
		t.Errorf("EstimateUSDWithCache = %v, want %v — cached input did not price at the cache rate", got, want)
	}
}

// A catalog entry with no cache rate prices cached tokens at the input
// rate: identical arithmetic to what EstimateUSD computed before the cache
// split existed. Absence of a cache price must not change any number.
func TestEstimateUSDWithCache_UnknownCacheRateFallsBackToInputRate(t *testing.T) {
	withGatewayPricer(t,
		constGatewayPricer(0.60, 1.80, 0, true),
		constGatewayPricer(0.60, 1.80, 0, true))
	withCache := EstimateUSDWithCache("openai_compatible/some-model", 1_000_000, 0, 900_000)
	without := EstimateUSD("openai_compatible/some-model", 1_000_000, 0)
	if withCache != without {
		t.Errorf("EstimateUSDWithCache = %v, EstimateUSD = %v — an unpriced cache rate changed the figure", withCache, without)
	}
}

// A provider reporting more cached tokens than the prompt carried is a
// corrupt account, not a debt: the cached share clamps to the input total
// so the uncached share never goes negative.
func TestEstimateUSDWithCache_ClampsCacheReadToInput(t *testing.T) {
	withGatewayPricer(t,
		constGatewayPricer(0.60, 1.80, 0.12, true),
		constGatewayPricer(0.60, 1.80, 0.12, true))
	got := EstimateUSDWithCache("openai_compatible/deepseek-v4.1-flash-1017", 100_000, 0, 2_000_000)
	want := 0.1 * 0.12 // everything cached, nothing negative
	if got != want {
		t.Errorf("EstimateUSDWithCache = %v, want %v — the cache share was not clamped to the input", got, want)
	}
}

// EstimateUSD is the pre-cache signature; it must keep delegating with no
// cache share so every one of its ~15 existing callers read the same number
// they always did.
func TestEstimateUSD_DelegatesWithNoCacheShare(t *testing.T) {
	withGatewayPricer(t,
		constGatewayPricer(0.60, 1.80, 0.12, true),
		constGatewayPricer(0.60, 1.80, 0.12, true))
	if got, want := EstimateUSD("openai_compatible/deepseek-v4.1-flash-1017", 1_000_000, 500_000), EstimateUSDWithCache("openai_compatible/deepseek-v4.1-flash-1017", 1_000_000, 500_000, 0); got != want {
		t.Errorf("EstimateUSD = %v, EstimateUSDWithCache(cacheRead=0) = %v — the delegates disagree", got, want)
	}
}

// Without a cache rate, the fallback must be the pre-split arithmetic
// BIT-FOR-BIT, for any cache share: (uncached+cached)*inPerM is not
// uncached*inPerM + cached*inPerM in floating point, and "an unextended
// table changes no number" is a claim about every bit, not about 4-decimal
// rounding. Awkward magnitudes, not powers of ten, are the case that
// separates the two expressions.
func TestEstimateUSDWithCache_NoCacheRateIsBitExactWithThePreSplitArithmetic(t *testing.T) {
	withGatewayPricer(t,
		constGatewayPricer(0.60, 1.80, 0, true),
		constGatewayPricer(0.60, 1.80, 0, true))
	got := EstimateUSDWithCache("openai_compatible/gw/m", 1_000_003, 777_777, 500_001)
	want := EstimateUSD("openai_compatible/gw/m", 1_000_003, 777_777)
	if got != want {
		t.Errorf("EstimateUSDWithCache = %v, EstimateUSD = %v — the no-cache-rate fallback is not bit-exact", got, want)
	}
}

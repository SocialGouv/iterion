package cost

import "testing"

func TestResolvedRatePreservesKnownZero(t *testing.T) {
	t.Parallel()
	for _, known := range []bool{true, false} {
		amount, got := estimateUSDWithCacheKnown("openai_compatible/free", 0, 0, 0, func(string, func(string) string) (float64, float64, float64, bool) { return 0, 0, 0, known }, func(string) string { return "" })
		if amount != 0 || got != known {
			t.Fatalf("known=%v -> %g,%v", known, amount, got)
		}
	}
}

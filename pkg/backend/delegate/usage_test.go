package delegate

import "testing"

// Reading cost or unreported calls from model JSON instead of harness fields
// makes the unknown-model case retain its invented bill. Omitting the typed
// assignment loses the provider bill even though the output map looks right.
func TestAnnotateResultUsageIgnoresModelCounters(t *testing.T) {
	for _, usd := range []float64{0, 0.25} {
		result := Result{Output: map[string]any{"_tokens": 99999999, "_cost_usd": 99999999, "_usage_unreported_calls": 999}}
		AnnotateResultUsage(&result, "unknown/model", 70, 30, usd)
		if result.Tokens != 100 || result.CostUSD != usd || result.UnreportedCalls != 0 {
			t.Fatalf("typed bill = %+v", result)
		}
		if result.Output["_tokens"] != 100 {
			t.Fatalf("output tokens = %v", result.Output["_tokens"])
		}
		if _, ok := result.Output["_usage_unreported_calls"]; ok {
			t.Fatal("model can invent unreported calls")
		}
		if usd == 0 {
			if _, ok := result.Output["_cost_usd"]; ok {
				t.Fatal("model can invent an unpriced bill")
			}
		} else if result.Output["_cost_usd"] != usd {
			t.Fatalf("provider bill = %v, want %v", result.Output["_cost_usd"], usd)
		}
	}
}

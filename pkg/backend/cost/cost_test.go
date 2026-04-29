package cost

import "testing"

func TestEstimateUSD(t *testing.T) {
	cases := []struct {
		name        string
		model       string
		in, out     int
		want        float64
		approximate bool
	}{
		{"unknown model returns 0", "made-up-model", 1000, 1000, 0, false},
		{"haiku 1k in / 1k out", "claude-haiku-4-5", 1_000_000, 1_000_000, 1.50, false},
		{"haiku with provider prefix", "anthropic/claude-haiku-4-5", 1_000_000, 1_000_000, 1.50, false},
		{"haiku with tenant-prefixed spec", "anthropic/eu/claude-haiku-4-5", 1_000_000, 1_000_000, 1.50, false},
		{"sonnet 1m+1m", "claude-sonnet-4-6", 1_000_000, 1_000_000, 18.00, false},
		{"opus 1m+1m", "claude-opus-4-7", 1_000_000, 1_000_000, 90.00, false},
		{"gpt-5 1m+1m", "openai/gpt-5", 1_000_000, 1_000_000, 11.25, false},
		{"zero tokens", "claude-haiku-4-5", 0, 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := EstimateUSD(tc.model, tc.in, tc.out)
			if got != tc.want {
				t.Fatalf("EstimateUSD(%q, %d, %d) = %v, want %v", tc.model, tc.in, tc.out, got, tc.want)
			}
		})
	}
}

func TestAnnotate(t *testing.T) {
	t.Run("known model writes _cost_usd", func(t *testing.T) {
		out := map[string]interface{}{}
		total := Annotate(out, "claude-haiku-4-5", 1000, 500)
		if total != 1500 {
			t.Fatalf("total = %d, want 1500", total)
		}
		if out["_tokens"].(int) != 1500 {
			t.Fatalf("_tokens = %v, want 1500", out["_tokens"])
		}
		if out["_model"].(string) != "claude-haiku-4-5" {
			t.Fatalf("_model = %v, want claude-haiku-4-5", out["_model"])
		}
		if _, ok := out["_cost_usd"].(float64); !ok {
			t.Fatalf("_cost_usd missing or wrong type: %v", out["_cost_usd"])
		}
	})

	t.Run("unknown model omits _cost_usd", func(t *testing.T) {
		out := map[string]interface{}{}
		Annotate(out, "made-up-model", 1000, 500)
		if _, ok := out["_cost_usd"]; ok {
			t.Fatalf("_cost_usd should be absent for unknown model")
		}
		if out["_tokens"].(int) != 1500 {
			t.Fatalf("_tokens still expected for unknown model")
		}
	})

	t.Run("nil output map is no-op", func(t *testing.T) {
		total := Annotate(nil, "claude-haiku-4-5", 100, 100)
		if total != 200 {
			t.Fatalf("total = %d, want 200", total)
		}
	})
}

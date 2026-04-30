package cost

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

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
		// Newer OpenAI tiers — exercised by claw delegate; previously
		// missing from the table they silently reported $0 in run
		// observability (vibe_review_alternating run_1777560043656).
		{"gpt-5.5 1m+1m", "openai/gpt-5.5", 1_000_000, 1_000_000, 17.00, false},
		{"gpt-5.4-mini 1m+1m", "openai/gpt-5.4-mini", 1_000_000, 1_000_000, 2.70, false},
		{"opus 4-6 inherits opus rate", "claude-opus-4-6", 1_000_000, 1_000_000, 90.00, false},
		{"sonnet 4-7 inherits sonnet rate", "claude-sonnet-4-7", 1_000_000, 1_000_000, 18.00, false},
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

// TestEstimateUSD_PrefersLiveRegistry covers the resolution chain: when
// claw's live cache contains the model, EstimateUSD uses those rates
// rather than the static table. This is the path that eliminates the
// static-table maintenance burden as new models ship via OpenRouter.
func TestEstimateUSD_PrefersLiveRegistry(t *testing.T) {
	// Seed claw's live cache with rates that intentionally differ from
	// the static gpt-5 entry so we can verify which source EstimateUSD
	// trusted.
	dir := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", dir)
	clawDir := filepath.Join(dir, "claw-code-go")
	if err := os.MkdirAll(clawDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Inline the LiveCache JSON so we don't import claw's internal
	// package (Go's internal-import rule blocks cross-module access).
	// The on-disk format is the public source of truth for the
	// integration: any change to the JSON shape would break consumers
	// regardless of whether they go through claw's typed APIs.
	cache := fmt.Sprintf(`{
  "entries": [
    {
      "canonical": "gpt-5",
      "provider": "openai",
      "input_usd_per_million": 99.0,
      "output_usd_per_million": 999.0
    }
  ],
  "fetched_at": %q,
  "source": "test"
}`, time.Now().UTC().Format(time.RFC3339Nano))
	if err := os.WriteFile(filepath.Join(clawDir, "models-cache.json"), []byte(cache), 0o644); err != nil {
		t.Fatalf("write cache: %v", err)
	}

	got := EstimateUSD("gpt-5", 1_000_000, 1_000_000)
	// 99 + 999 = 1098 if claw is consulted; 11.25 from the static
	// table otherwise.
	if got != 1098.0 {
		t.Errorf("EstimateUSD did not consult claw live cache: got %v, want 1098 (99+999)", got)
	}
}

// TestEstimateUSD_FallsBackToStaticTable covers the cold-start path:
// when the live cache has no entry for the model, EstimateUSD falls
// back to the static table seeded in this package.
func TestEstimateUSD_FallsBackToStaticTable(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir()) // empty cache dir
	got := EstimateUSD("gpt-5", 1_000_000, 1_000_000)
	if got != 11.25 {
		t.Errorf("static fallback: got %v, want 11.25", got)
	}
}

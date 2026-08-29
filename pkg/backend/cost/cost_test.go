package cost

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// liveCacheDir returns a cache root for a test that needs claw's live
// registry ENABLED, so $XDG_CACHE_HOME has to point at a real directory.
//
// It deliberately does NOT use t.TempDir(). EstimateUSD reaches
// clawapi.LookupModelPricing, which calls MaybeRefreshLive — a DETACHED
// goroutine that resolves $XDG_CACHE_HOME only when it runs and re-creates
// <root>/claw-code-go through MkdirAll on its way to the cache file. That
// write can land while t.TempDir()'s RemoveAll is walking the tree, failing
// an otherwise-green test with "TempDir RemoveAll cleanup: ... directory not
// empty" (observed on the merge queue, 2026-08-04, run 30944201638).
// Retrying absorbs the race; a directory we still cannot remove is a few
// bytes left in os.TempDir(), never a red test.
func liveCacheDir(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("", "cost-live-cache-")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() {
		for attempt := 0; ; attempt++ {
			err := os.RemoveAll(root)
			if err == nil {
				return
			}
			if attempt == 4 {
				t.Logf("leaving %s behind (claw's async refresh re-created it): %v", root, err)
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	})
	return root
}

// seedClawCache writes claw's live-registry cache under root so a test can pin
// the rates tier 1 will answer with.
//
// The LiveCache JSON is inlined rather than built through claw's own types:
// Go's internal-import rule blocks cross-module access to the package that
// defines them. The on-disk format is the public source of truth for the
// integration anyway — a change to the JSON shape breaks consumers whether or
// not they went through claw's typed APIs.
func seedClawCache(t *testing.T, root, canonical string, inPerM, outPerM float64) {
	t.Helper()
	clawDir := filepath.Join(root, "claw-code-go")
	if err := os.MkdirAll(clawDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cache := fmt.Sprintf(`{
  "entries": [
    {
      "canonical": %q,
      "provider": "openai",
      "input_usd_per_million": %v,
      "output_usd_per_million": %v
    }
  ],
  "fetched_at": %q,
  "source": "test"
}`, canonical, inPerM, outPerM, time.Now().UTC().Format(time.RFC3339Nano))
	if err := os.WriteFile(filepath.Join(clawDir, "models-cache.json"), []byte(cache), 0o644); err != nil {
		t.Fatalf("write cache: %v", err)
	}
}

func TestEstimateUSD(t *testing.T) {
	// Pin the static table as the only price source. Without this the
	// assertions below are machine-dependent: on a host whose claw live
	// cache already holds claude-haiku-4-5 & co, EstimateUSD answers from
	// the live registry (it is consulted FIRST) and every exact figure
	// here becomes a third party's number. Disabling also keeps the async
	// refresh goroutine from ever starting — see liveCacheDir.
	t.Setenv("CLAW_DISABLE_LIVE_REGISTRY", "1")
	cases := []struct {
		name        string
		model       string
		in, out     int
		want        float64
		approximate bool
	}{
		{"unknown model returns 0", "made-up-model", 1000, 1000, 0, false},
		{"haiku 1k in / 1k out", "claude-haiku-4-5", 1_000_000, 1_000_000, 6.00, false},
		{"haiku with provider prefix", "anthropic/claude-haiku-4-5", 1_000_000, 1_000_000, 6.00, false},
		{"haiku with tenant-prefixed spec", "anthropic/eu/claude-haiku-4-5", 1_000_000, 1_000_000, 6.00, false},
		{"sonnet 1m+1m", "claude-sonnet-4-6", 1_000_000, 1_000_000, 18.00, false},
		{"opus 1m+1m", "claude-opus-4-7", 1_000_000, 1_000_000, 30.00, false},
		{"gpt-5 1m+1m", "openai/gpt-5", 1_000_000, 1_000_000, 11.25, false},
		// Newer OpenAI tiers — exercised by claw delegate; previously
		// missing from the table they silently reported $0 in run
		// observability (whole_improve_loop run_1777560043656).
		{"gpt-5.5 1m+1m", "openai/gpt-5.5", 1_000_000, 1_000_000, 35.00, false},
		{"gpt-5.4-mini 1m+1m", "openai/gpt-5.4-mini", 1_000_000, 1_000_000, 5.25, false},
		{"opus 4-6 inherits opus rate", "claude-opus-4-6", 1_000_000, 1_000_000, 30.00, false},
		// Opus 4 predates the 4.5 price drop and legitimately keeps the old
		// rate. Its agreeing with the aggregator is what made the staleness
		// of every later release legible.
		{"opus 4 keeps the pre-drop rate", "claude-opus-4", 1_000_000, 1_000_000, 90.00, false},
		{"opus 5 inherits the current opus rate", "claude-opus-5", 1_000_000, 1_000_000, 30.00, false},
		{"sonnet 5 has its own rate", "claude-sonnet-5", 1_000_000, 1_000_000, 12.00, false},

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
	t.Setenv("CLAW_DISABLE_LIVE_REGISTRY", "1") // static table only — see TestEstimateUSD
	t.Run("known model writes _cost_usd", func(t *testing.T) {
		out := map[string]any{}
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
		out := map[string]any{}
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

func TestAnnotateWithUSD(t *testing.T) {
	t.Setenv("CLAW_DISABLE_LIVE_REGISTRY", "1") // static table only — see TestEstimateUSD
	t.Run("provider-computed cost wins over the estimate", func(t *testing.T) {
		out := map[string]any{}
		total := AnnotateWithUSD(out, "claude-haiku-4-5", 1000, 500, 0.42)
		if total != 1500 {
			t.Fatalf("total = %d, want 1500", total)
		}
		if got := out["_cost_usd"].(float64); got != 0.42 {
			t.Fatalf("_cost_usd = %v, want 0.42 — an authoritative provider figure must beat the table", got)
		}
	})

	t.Run("cost for a model the table never heard of", func(t *testing.T) {
		out := map[string]any{}
		AnnotateWithUSD(out, "some-vendor/brand-new-model", 10, 20, 0.001)
		if got := out["_cost_usd"].(float64); got != 0.001 {
			t.Fatalf("_cost_usd = %v, want 0.001", got)
		}
	})

	t.Run("zero cost degrades to Annotate", func(t *testing.T) {
		withUSD := map[string]any{}
		AnnotateWithUSD(withUSD, "claude-haiku-4-5", 1000, 500, 0)
		plain := map[string]any{}
		Annotate(plain, "claude-haiku-4-5", 1000, 500)
		if withUSD["_cost_usd"] != plain["_cost_usd"] {
			t.Fatalf("_cost_usd = %v, want the estimate %v", withUSD["_cost_usd"], plain["_cost_usd"])
		}

		unknown := map[string]any{}
		AnnotateWithUSD(unknown, "made-up-model", 1000, 500, 0)
		if _, ok := unknown["_cost_usd"]; ok {
			t.Fatal("_cost_usd should stay absent when neither the CLI nor the table knows the price")
		}
	})

	t.Run("nil output map is no-op", func(t *testing.T) {
		if total := AnnotateWithUSD(nil, "m", 100, 100, 1.5); total != 200 {
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
	dir := liveCacheDir(t)
	t.Setenv("XDG_CACHE_HOME", dir)
	seedClawCache(t, dir, "gpt-5", 99.0, 999.0)

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
	// "No live source" is expressed with claw's own switch rather than an
	// empty XDG_CACHE_HOME: an empty cache dir left the registry ENABLED,
	// so the lookup fired a real network refresh whose detached goroutine
	// then wrote the fetched cache back into this test's temp dir — racing
	// its removal and, worse, sometimes populating the very cache the test
	// asserts is absent.
	t.Setenv("CLAW_DISABLE_LIVE_REGISTRY", "1")
	got := EstimateUSD("gpt-5", 1_000_000, 1_000_000)
	if got != 11.25 {
		t.Errorf("static fallback: got %v, want 11.25", got)
	}
}

// GLM is absent from the committed table on purpose — 24 providers publish it
// at rates from 0 to 1.44 per million — yet a run IS charged for it, because
// the live registry answers before the table is ever consulted. Asserting a
// zero here was the mistake that exposed an audit judging a source the
// estimator never reaches first.
//
// The assertion is on the PROPERTY, not the number: the live rate is a third
// party's blended figure and pinning it would break CI on their next revision,
// while proving nothing about iterion. When the registry is unavailable
// (offline, CLAW_DISABLE_LIVE_REGISTRY=1) there is legitimately no price and
// the test says so rather than failing.
func TestGLMIsPricedByTheLiveRegistryNotTheTable(t *testing.T) {
	if _, _, ok := StaticRate("glm-5.2"); ok {
		t.Fatal("glm-5.2 gained a committed entry: pick which provider's rate it is, or drop it again")
	}
	in, out, ok := EffectiveRate("anthropic/glm-5.2")
	if !ok {
		t.Skip("live registry unavailable — no effective price to check")
	}
	if in <= 0 || out <= 0 {
		t.Errorf("effective rate %v/%v: the registry answered, so both sides must be positive", in, out)
	}
	if out <= in {
		t.Errorf("effective rate %v/%v: output tokens cost more than input on every provider", in, out)
	}
}

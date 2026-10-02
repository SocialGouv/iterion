// Package pricingtest isolates a test binary from the host's live model
// prices. cost.EstimateUSD consults two live tiers before its static table:
// claw's registry, which reads the cache the host last fetched and refreshes
// it from the network in the background, and iterion's aggregator, which reads
// ~/.iterion and fetches models.dev. A test whose verdict reads a price would
// otherwise take it from whatever the machine last saw, and see it change
// mid-run when a refresh lands (#1686).
package pricingtest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/cost"
	"github.com/SocialGouv/iterion/pkg/backend/modelspecs"
)

// Isolate switches both live tiers off for the whole process — claw's
// registry by its own switch, the aggregator by an empty registry that never
// fetches — and returns the function restoring the aggregator. Call it from
// TestMain, around m.Run.
func Isolate() (restore func()) {
	if err := os.Setenv("CLAW_DISABLE_LIVE_REGISTRY", "1"); err != nil {
		panic(err)
	}
	return modelspecs.SetDefault(modelspecs.New(modelspecs.Options{
		CachePath:   filepath.Join(os.TempDir(), "iterion-modelspecs-test-absent.json"),
		NoAutoFetch: true,
	}))
}

// RequireIsolated fails the test unless this process prices from the static
// table: it points claw's registry at a fresh cache that prices a model far
// from the table, and demands the table's price. A package whose TestMain
// does not call Isolate fails it.
func RequireIsolated(t *testing.T) {
	t.Helper()
	const model = "gpt-5.6-sol"
	in, _, ok := cost.StaticRate(model)
	if !ok {
		t.Fatalf("the static table no longer prices %s: point RequireIsolated at a model it prices", model)
	}
	dir := t.TempDir()
	cache := map[string]any{
		"entries": []map[string]any{{
			"canonical":              model,
			"provider":               "openai",
			"input_usd_per_million":  in / 1000,
			"output_usd_per_million": in / 1000,
			"aliases":                []string{"openai/" + model},
		}},
		// Fresh, so a registry that is not switched off reads it instead of
		// refreshing it from the network.
		"fetched_at": time.Now().UTC(),
		"source":     "pricingtest",
	}
	body, err := json.Marshal(cache)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "claw-code-go"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "claw-code-go", "models-cache.json"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CACHE_HOME", dir)
	if got := cost.EstimateUSD(model, 1_000_000, 0); got != in {
		t.Fatalf("cost.EstimateUSD(%s) = %v, want the static table's %v: a live pricing tier reaches this test "+
			"binary, so any verdict that reads a price depends on the host — call pricingtest.Isolate() from TestMain",
			model, got, in)
	}
}

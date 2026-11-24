package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/claw/internal/apikit"
)

// TestLookupModelPricing_FromLiveCache covers the live-cache-hit path:
// when the on-disk cache contains pricing for the requested model
// (canonical or alias), LookupModelPricing returns it as ModelPricing
// with Source="live".
func TestLookupModelPricing_FromLiveCache(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", dir)

	clawDir := filepath.Join(dir, "claw-code-go")
	if err := os.MkdirAll(clawDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cache := apikit.LiveCache{
		Entries: []apikit.LiveCacheEntry{
			{
				Canonical:     "gpt-5.5",
				Provider:      "openai",
				ContextWindow: 1_050_000,
				InputUSDPerM:  2.0,
				OutputUSDPerM: 15.0,
				Aliases:       []string{"openai/gpt-5.5"},
			},
			{
				Canonical:     "free-model",
				Provider:      "test",
				ContextWindow: 4096,
				// Both pricing fields zero — treated as unknown.
			},
		},
		FetchedAt: time.Now().UTC(),
		Source:    "test",
	}
	data, _ := json.Marshal(cache)
	if err := os.WriteFile(filepath.Join(clawDir, "models-cache.json"), data, 0o644); err != nil {
		t.Fatalf("write cache: %v", err)
	}
	// TestMain switches the live registry off for the process; the switch
	// also short-circuits liveCachePath, which would hide the cache seeded
	// above, so this test clears it — after seeding: the cache is fresh
	// (FetchedAt now), so the refresh it re-enables finds nothing to fetch.
	t.Setenv("CLAW_DISABLE_LIVE_REGISTRY", "")
	// The refresh this test enables reads the environment whenever it gets
	// scheduled: do not return before it has read this test's.
	t.Cleanup(func() {
		deadline := time.Now().Add(10 * time.Second)
		for apikit.DefaultModelRegistry().LookupModel("free-model") == nil {
			if time.Now().After(deadline) {
				t.Error("the refresh this test enabled never merged the seeded cache")
				return
			}
			time.Sleep(time.Millisecond)
		}
	})

	cases := []struct {
		name    string
		input   string
		wantOK  bool
		wantIn  float64
		wantOut float64
	}{
		{"canonical hit", "gpt-5.5", true, 2.0, 15.0},
		{"alias hit", "openai/gpt-5.5", true, 2.0, 15.0},
		{"tenant-prefixed alias", "openai/eu/gpt-5.5", true, 2.0, 15.0}, // last segment used
		{"empty input", "", false, 0, 0},
		{"unknown model", "claude-mystery-9", false, 0, 0},
		{"known but pricing 0", "free-model", false, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := LookupModelPricing(c.input)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v (got=%+v)", ok, c.wantOK, got)
			}
			if !ok {
				return
			}
			if got.InputUSDPerMillion != c.wantIn || got.OutputUSDPerMillion != c.wantOut {
				t.Errorf("pricing mismatch: got %+v, want input=%v output=%v", got, c.wantIn, c.wantOut)
			}
			if got.Source != "live" {
				t.Errorf("Source = %q, want %q", got.Source, "live")
			}
		})
	}
}

// With the live registry switched off (TestMain), the live tier prices
// nothing — not even from a fresh cache that prices the model. The
// missing-cache branch with the registry on has no witness in this package:
// reaching it starts a refresh that fetches.
func TestLookupModelPricing_RegistrySwitchedOff(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", dir)
	clawDir := filepath.Join(dir, "claw-code-go")
	if err := os.MkdirAll(clawDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	data, _ := json.Marshal(apikit.LiveCache{
		Entries:   []apikit.LiveCacheEntry{{Canonical: "gpt-5.5", Provider: "openai", InputUSDPerM: 2.0, OutputUSDPerM: 15.0}},
		FetchedAt: time.Now().UTC(),
		Source:    "test",
	})
	if err := os.WriteFile(filepath.Join(clawDir, "models-cache.json"), data, 0o644); err != nil {
		t.Fatalf("write cache: %v", err)
	}

	if got, ok := LookupModelPricing("gpt-5.5"); ok {
		t.Errorf("LookupModelPricing = %+v from the cache, want nothing with the live registry switched off", got)
	}
}

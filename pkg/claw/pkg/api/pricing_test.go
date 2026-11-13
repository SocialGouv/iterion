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
	// CLAW_DISABLE_LIVE_REGISTRY is intentionally NOT set — that env
	// var also short-circuits liveCachePath (returns an error) which
	// would prevent LoadLiveCache from finding the file we seeded
	// below. Leaving it unset means MaybeRefreshLive's once-guard
	// fires a goroutine that tries to fetch from OpenRouter; in CI
	// without network it fails silently and the test still completes
	// because our seeded cache is read synchronously by LoadLiveCache.

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

// TestLookupModelPricing_NoCache returns ok=false when the cache is
// missing — the caller should fall back to whatever static table it
// maintains for offline / cold-start operation.
func TestLookupModelPricing_NoCache(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	if _, ok := LookupModelPricing("gpt-5.5"); ok {
		t.Error("expected ok=false when no cache file exists")
	}
}

package apikit

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestLoadLiveCache_Missing(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	cache, err := LoadLiveCache()
	if err != nil {
		t.Fatalf("missing cache should not error: %v", err)
	}
	if cache != nil {
		t.Errorf("expected nil cache, got %+v", cache)
	}
}

func TestLoadLiveCache_Corrupt(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", dir)
	clawDir := filepath.Join(dir, "claw-code-go")
	if err := os.MkdirAll(clawDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(clawDir, liveCacheFilename), []byte("not json"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := LoadLiveCache(); err == nil {
		t.Error("expected error on corrupt cache, got nil")
	}
}

func TestParseUSDPerToken(t *testing.T) {
	cases := []struct {
		in   string
		want float64
	}{
		{"", 0},
		{"0", 0},            // free model — treated as unknown
		{"-1", 0},           // malformed → unknown
		{"not a number", 0}, // unparseable → unknown
		{"0.000001", 1},     // $1 / M
		{"1e-6", 1},         // scientific form → $1 / M
		{"0.0000125", 12.5}, // $12.50 / M (gpt-5 input)
		{"0.000075", 75},    // $75 / M (opus-4-7 output)
	}
	for _, c := range cases {
		got := parseUSDPerToken(c.in)
		if got != c.want {
			t.Errorf("parseUSDPerToken(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestSaveAndLoadLiveCachePreservesPricing(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	want := &LiveCache{
		Entries: []LiveCacheEntry{
			{
				Canonical:     "gpt-5.5",
				Provider:      "openai",
				ContextWindow: 1_050_000,
				MaxOutput:     128_000,
				InputUSDPerM:  2.0,
				OutputUSDPerM: 15.0,
			},
		},
		FetchedAt: time.Now().UTC(),
		Source:    "test",
	}
	if err := saveLiveCache(want); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := LoadLiveCache()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got == nil || len(got.Entries) != 1 {
		t.Fatalf("expected 1 entry, got %+v", got)
	}
	e := got.Entries[0]
	if e.InputUSDPerM != 2.0 || e.OutputUSDPerM != 15.0 {
		t.Errorf("pricing not preserved: %+v", e)
	}
}

func TestSaveAndLoadLiveCacheRoundtrip(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	want := &LiveCache{
		Entries: []LiveCacheEntry{
			{Canonical: "claude-opus-4-7", Provider: "anthropic", ContextWindow: 1_000_000, MaxOutput: 128_000, Aliases: []string{"opus"}},
		},
		FetchedAt: time.Now().UTC(),
		Source:    "test",
	}
	if err := saveLiveCache(want); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := LoadLiveCache()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got == nil || len(got.Entries) != 1 || got.Entries[0].Canonical != "claude-opus-4-7" {
		t.Errorf("roundtrip mismatch: %+v", got)
	}
}

func TestMergeLiveIntoRegistry_AddsNewEntry(t *testing.T) {
	reg := &ModelRegistry{}
	reg.ensureInit()
	cache := &LiveCache{Entries: []LiveCacheEntry{
		{Canonical: "claude-opus-9-9", Provider: "anthropic", ContextWindow: 2_000_000, MaxOutput: 256_000, Aliases: []string{"future-opus"}},
	}}
	mergeLiveIntoRegistry(reg, cache)
	entry := reg.LookupModel("claude-opus-9-9")
	if entry == nil {
		t.Fatal("expected new entry after merge")
	}
	if entry.ContextWindow != 2_000_000 || entry.MaxOutput != 256_000 {
		t.Errorf("limits not propagated: %+v", entry)
	}
	if reg.LookupModel("future-opus") == nil {
		t.Error("alias not registered")
	}
	if entry.Metadata == nil || entry.Metadata.Provider != ProviderAnthropic {
		t.Error("provider metadata not resolved for new entry")
	}
}

func TestMergeLiveIntoRegistry_UpdatesContextWindow(t *testing.T) {
	reg := &ModelRegistry{}
	reg.ensureInit()
	// Embed value for opus-4-6 is 1_000_000; bump it to 1_500_000 via live data.
	cache := &LiveCache{Entries: []LiveCacheEntry{
		{Canonical: "claude-opus-4-6", ContextWindow: 1_500_000, MaxOutput: 200_000},
	}}
	mergeLiveIntoRegistry(reg, cache)
	entry := reg.LookupModel("claude-opus-4-6")
	if entry == nil {
		t.Fatal("opus-4-6 should remain registered")
	}
	if entry.ContextWindow != 1_500_000 {
		t.Errorf("ContextWindow not bumped: got %d", entry.ContextWindow)
	}
	if entry.MaxOutput != 200_000 {
		t.Errorf("MaxOutput not bumped: got %d", entry.MaxOutput)
	}
}

func TestMergeLiveIntoRegistry_PreservesEmbedAliases(t *testing.T) {
	reg := &ModelRegistry{}
	reg.ensureInit()
	// Live data does not list "opus" alias; merge must not strip it.
	// The embed maps "opus" to the newest Opus (claude-opus-5-5).
	cache := &LiveCache{Entries: []LiveCacheEntry{
		{Canonical: "claude-opus-5-5", ContextWindow: 1_000_000, MaxOutput: 128_000},
	}}
	mergeLiveIntoRegistry(reg, cache)
	if reg.ResolveAlias("opus") != "claude-opus-5-5" {
		t.Errorf("embed alias 'opus' was lost after merge")
	}
}

func TestMergeLiveIntoRegistry_ZeroValuesDontOverwrite(t *testing.T) {
	reg := &ModelRegistry{}
	reg.ensureInit()
	before := reg.LookupModel("claude-opus-4-7")
	if before == nil {
		t.Fatal("opus-4-7 should be in embed")
	}
	beforeCtx := before.ContextWindow

	// Live entry with zero ContextWindow must not zero out the embed value.
	cache := &LiveCache{Entries: []LiveCacheEntry{
		{Canonical: "claude-opus-4-7", ContextWindow: 0, MaxOutput: 0},
	}}
	mergeLiveIntoRegistry(reg, cache)
	if reg.LookupModel("claude-opus-4-7").ContextWindow != beforeCtx {
		t.Errorf("zero live value clobbered embed value")
	}
}

func TestCanonicalFromOpenRouterID(t *testing.T) {
	tests := []struct {
		in           string
		wantCanon    string
		wantProvider string
	}{
		{"anthropic/claude-opus-4.7", "claude-opus-4-7", "anthropic"},
		{"anthropic/claude-sonnet-4.6", "claude-sonnet-4-6", "anthropic"},
		{"openai/gpt-5.5", "gpt-5.5", "openai"},
		{"x-ai/grok-3", "grok-3", "xai"},
		{"some-unknown-id", "some-unknown-id", ""},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			gotCanon, gotProvider := canonicalFromOpenRouterID(tt.in)
			if gotCanon != tt.wantCanon {
				t.Errorf("canonical: got %q, want %q", gotCanon, tt.wantCanon)
			}
			if gotProvider != tt.wantProvider {
				t.Errorf("provider: got %q, want %q", gotProvider, tt.wantProvider)
			}
		})
	}
}

func TestMaybeRefreshLive_DisabledByEnv(t *testing.T) {
	t.Setenv("CLAW_DISABLE_LIVE_REGISTRY", "1")
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	// Reset the once so the test exercises the disabled path.
	resetLiveFetchOnce()

	reg := &ModelRegistry{}
	reg.ensureInit()
	MaybeRefreshLive(reg)
	// Nothing observable to assert beyond "no panic, no goroutine spawned".
	// The env-guard inside MaybeRefreshLive returns immediately.
}

// A live refresh must not mutate a published *ModelEntry: LookupModel hands
// that pointer out and callers dereference it after the read lock is gone, so
// an in-place update races every one of them (the mutex guards the map, not
// the structs). Reproduces the report from iterion's -race CI.
func TestMergeLiveIntoRegistryDoesNotMutatePublishedEntries(t *testing.T) {
	reg := &ModelRegistry{}
	const canonical = "claude-opus-4-8"
	if reg.LookupModel(canonical) == nil {
		t.Fatalf("%s is missing from the embed registry — pick another fixture", canonical)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// The exact shape preflight.ModelTokenLimitForModel uses: take the
		// pointer under the read lock, read its fields after.
		for {
			select {
			case <-stop:
				return
			default:
			}
			if entry := reg.LookupModel(canonical); entry != nil {
				_ = entry.ContextWindow
				_ = entry.MaxOutput
			}
		}
	}()

	const passes = 300
	for i := 1; i <= passes; i++ {
		mergeLiveIntoRegistry(reg, &LiveCache{Entries: []LiveCacheEntry{{
			Canonical:     canonical,
			ContextWindow: uint32(1_000_000 + i),
			MaxOutput:     uint32(64_000 + i),
		}}})
	}
	close(stop)
	wg.Wait()

	entry := reg.LookupModel(canonical)
	if entry == nil {
		t.Fatal("the entry vanished from the registry")
	}
	if got, want := entry.ContextWindow, uint32(1_000_000+passes); got != want {
		t.Errorf("last merge must win for ContextWindow: got %d, want %d", got, want)
	}
	if got, want := entry.MaxOutput, uint32(64_000+passes); got != want {
		t.Errorf("last merge must win for MaxOutput: got %d, want %d", got, want)
	}
}

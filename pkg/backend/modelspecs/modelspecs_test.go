package modelspecs

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// modelsDevJSON returns a minimal models.dev-shaped api.json. Pass includeGLM
// to control whether glm-5.2 is present (it is omitted by real aggregators
// today, which is exactly the case where the caller must keep its curated
// value — asserted in pkg/backend/model, which owns that table).
func modelsDevJSON(t *testing.T, includeGLM bool) string {
	t.Helper()
	providers := map[string]mdProvider{
		"anthropic": {Models: map[string]mdModel{
			"claude-sonnet-4-6": mdModelLit(1_000_000, 64000, 3, 15, boolp(true), boolp(true), boolp(true)),
		}},
		"openai": {Models: map[string]mdModel{
			"gpt-5": mdModelLit(400_000, 128000, 1.25, 10, boolp(true), boolp(true), boolp(false)),
		}},
	}
	if includeGLM {
		providers["z-ai"] = mdProvider{Models: map[string]mdModel{
			"glm-5.2": mdModelLit(1_000_000, 128000, 0.6, 2.2, boolp(true), boolp(true), boolp(true)),
		}}
	}
	b, err := json.Marshal(providers)
	if err != nil {
		t.Fatalf("marshal models.dev fixture: %v", err)
	}
	return string(b)
}

func mdModelLit(ctx, out int, in, outc float64, reasoning, tool, temp *bool) mdModel {
	var m mdModel
	m.Limit.Context = ctx
	m.Limit.Output = out
	m.Cost.Input = in
	m.Cost.Output = outc
	m.Reasoning = reasoning
	m.ToolCall = tool
	m.Temperature = temp
	return m
}

// newTestRegistry builds an isolated registry pointing at url with a temp cache
// path. Auto-fetch is off; tests drive Refresh synchronously.
func newTestRegistry(t *testing.T, url string) *Registry {
	t.Helper()
	return New(Options{
		URL:         url,
		CachePath:   filepath.Join(t.TempDir(), "model-specs-cache.json"),
		Client:      &http.Client{Timeout: defaultTimeout},
		NoAutoFetch: true,
	})
}

func TestModelSpecs_FetchAndLookup(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(modelsDevJSON(t, true)))
	}))
	defer srv.Close()

	r := newTestRegistry(t, srv.URL)
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	got, ok := r.Lookup("anthropic", "claude-sonnet-4-6")
	if !ok {
		t.Fatal("claude-sonnet-4-6 not found after refresh")
	}
	if got.ContextWindow != 1_000_000 || got.MaxOutputTokens != 64000 {
		t.Errorf("claude limits = %d ctx / %d out, want 1000000 / 64000", got.ContextWindow, got.MaxOutputTokens)
	}
	if got.InputCostPerM != 3 || got.OutputCostPerM != 15 {
		t.Errorf("claude price = %v/%v, want 3/15", got.InputCostPerM, got.OutputCostPerM)
	}
	if got.Reasoning == nil || !*got.Reasoning || got.ToolCall == nil || !*got.ToolCall {
		t.Errorf("claude flags = %+v, want reasoning+tool_call true", got)
	}

	gpt, ok := r.Lookup("openai", "gpt-5")
	if !ok {
		t.Fatal("gpt-5 not found after refresh")
	}
	if gpt.Temperature == nil || *gpt.Temperature {
		t.Errorf("gpt-5 Temperature = %v, want an explicit false", gpt.Temperature)
	}
	if gpt.ContextWindow != 400_000 {
		t.Errorf("gpt-5 ContextWindow = %d, want 400000", gpt.ContextWindow)
	}

	// The cache file must have been written for subsequent processes.
	if _, err := os.Stat(r.cachePath); err != nil {
		t.Errorf("cache file not written: %v", err)
	}
}

// LookupBare is the no-provider entry point (a `.bot` may pin a bare model id,
// and the cost estimator is handed whatever string a backend reported). It must
// answer from the bare index and must NOT answer from the qualified one.
func TestModelSpecs_LookupBare(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(modelsDevJSON(t, true)))
	}))
	defer srv.Close()

	r := newTestRegistry(t, srv.URL)
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	got, ok := r.LookupBare("claude-sonnet-4-6")
	if !ok || got.InputCostPerM != 3 {
		t.Errorf("LookupBare(claude-sonnet-4-6) = %+v, %v; want the published spec", got, ok)
	}
	// Case and surrounding space are normalised like the qualified path.
	if _, ok := r.LookupBare("  Claude-Sonnet-4-6 "); !ok {
		t.Error("LookupBare does not normalise case/whitespace")
	}
	if _, ok := r.LookupBare("anthropic/claude-sonnet-4-6"); ok {
		t.Error("LookupBare answered a qualified spec; the bare index is keyed on bare ids only")
	}
	if _, ok := r.LookupBare("no-such-model"); ok {
		t.Error("LookupBare invented an answer for an unknown model")
	}
}

func TestModelSpecs_OfflineDegradesToNoAnswer(t *testing.T) {
	// Point at a server we immediately close → connection refused.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	url := srv.URL
	srv.Close()

	r := newTestRegistry(t, url)
	r.client = &http.Client{Timeout: 200 * time.Millisecond}
	// Refresh must not panic and must return an error, but never block a run.
	if err := r.Refresh(context.Background()); err == nil {
		t.Fatal("expected error from offline refresh")
	}

	// Offline means "no answer", which is what leaves the caller's curated
	// value standing — never a zero-valued Spec reported as authoritative.
	if _, ok := r.Lookup("anthropic", "glm-5.2"); ok {
		t.Error("offline lookup reported an answer")
	}
}

func TestModelSpecs_CacheHit(t *testing.T) {
	// Pre-write a fresh cache; the server must never be contacted.
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		t.Error("aggregator hit despite fresh cache")
	}))
	defer srv.Close()

	r := newTestRegistry(t, srv.URL)
	r.autoFetch = true // would trigger a refresh if the cache were stale
	cf := cacheFile{
		FetchedAt: time.Now(),
		Source:    "test",
		Specs: map[string]Spec{
			"anthropic/claude-sonnet-4-6": {ContextWindow: 1_000_000, Reasoning: boolp(true), ToolCall: boolp(true), Temperature: boolp(true)},
		},
	}
	data, _ := json.MarshalIndent(cf, "", "  ")
	if err := os.WriteFile(r.cachePath, data, 0o644); err != nil {
		t.Fatalf("write cache: %v", err)
	}

	got, ok := r.Lookup("anthropic", "claude-sonnet-4-6")
	if !ok || got.ContextWindow != 1_000_000 {
		t.Errorf("cache-hit lookup = %+v, %v; want 1M context", got, ok)
	}
	// Second call within TTL also performs no fetch.
	_, _ = r.Lookup("anthropic", "claude-sonnet-4-6")
	// Give any (erroneous) background goroutine a chance to run.
	time.Sleep(50 * time.Millisecond)
	if hit {
		t.Error("aggregator was contacted on the cache-hit path")
	}
}

func TestModelSpecs_StaleRefresh(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(modelsDevJSON(t, true)))
	}))
	defer srv.Close()

	r := newTestRegistry(t, srv.URL)
	r.ttl = 10 * time.Millisecond
	// Seed a stale cache (old FetchedAt) holding a wrong value.
	cf := cacheFile{
		FetchedAt: time.Now().Add(-time.Hour),
		Source:    "test",
		Specs:     map[string]Spec{"anthropic/claude-sonnet-4-6": {ContextWindow: 123}},
	}
	data, _ := json.MarshalIndent(cf, "", "  ")
	if err := os.WriteFile(r.cachePath, data, 0o644); err != nil {
		t.Fatalf("write cache: %v", err)
	}

	// A synchronous refresh replaces the stale value.
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	got, _ := r.Lookup("anthropic", "claude-sonnet-4-6")
	if got.ContextWindow != 1_000_000 {
		t.Errorf("after stale refresh ContextWindow = %d, want 1000000", got.ContextWindow)
	}
}

func TestModelSpecs_MalformedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("{ this is not valid json"))
	}))
	defer srv.Close()

	r := newTestRegistry(t, srv.URL)
	if err := r.Refresh(context.Background()); err == nil {
		t.Fatal("expected error from malformed response")
	}
	if _, ok := r.Lookup("openai", "o1-preview"); ok {
		t.Error("malformed response left the registry answering")
	}
}

// A model the aggregator omits must produce no answer at all, so the caller's
// curated value survives. glm-5.2 is the live instance of that case.
func TestModelSpecs_OmittedModelHasNoAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(modelsDevJSON(t, false))) // GLM omitted
	}))
	defer srv.Close()

	r := newTestRegistry(t, srv.URL)
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	for _, m := range []string{"glm-5.2", "glm-5.1", "glm-4.6"} {
		if _, ok := r.Lookup("anthropic", m); ok {
			t.Errorf("%s: aggregator omits it, so lookup must not answer", m)
		}
	}
}

// ITERION_MODEL_SPECS=off must make every read a no-op — no answer, no disk,
// no network.
func TestModelSpecs_DisabledAnswersNothing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("aggregator hit while disabled")
	}))
	defer srv.Close()

	r := New(Options{URL: srv.URL, CachePath: filepath.Join(t.TempDir(), "c.json"), Disabled: true})
	if _, ok := r.Lookup("anthropic", "claude-sonnet-4-6"); ok {
		t.Error("disabled registry answered a lookup")
	}
	if _, ok := r.LookupBare("claude-sonnet-4-6"); ok {
		t.Error("disabled registry answered a bare lookup")
	}
	if err := r.Refresh(context.Background()); err != nil {
		t.Errorf("Refresh on a disabled registry = %v, want a silent no-op", err)
	}
	time.Sleep(50 * time.Millisecond)
}

// A nil registry is the zero value a caller gets before wiring; it must read as
// "no answer" rather than panic, since resolution is on the run hot path.
func TestModelSpecs_NilRegistryIsSafe(t *testing.T) {
	var r *Registry
	if _, ok := r.Lookup("anthropic", "claude-opus-5"); ok {
		t.Error("nil registry answered a lookup")
	}
	if _, ok := r.LookupBare("claude-opus-5"); ok {
		t.Error("nil registry answered a bare lookup")
	}
	if err := r.Refresh(context.Background()); err != nil {
		t.Errorf("nil Refresh = %v, want nil", err)
	}
}

func TestModelSpecs_StaleCacheOfflineRefreshDoesNotRefetchWithinTTL(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		http.Error(w, "offline", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	r := newTestRegistry(t, srv.URL)
	r.autoFetch = true
	r.ttl = time.Hour

	// Seed a stale cache. It should be used as a non-blocking fallback while the
	// one allowed background refresh attempt fails.
	cf := cacheFile{
		FetchedAt: time.Now().Add(-2 * time.Hour),
		Source:    "test",
		Specs: map[string]Spec{
			"anthropic/claude-sonnet-4-6": {ContextWindow: 123, Reasoning: boolp(true)},
		},
	}
	data, _ := json.MarshalIndent(cf, "", "  ")
	if err := os.WriteFile(r.cachePath, data, 0o644); err != nil {
		t.Fatalf("write cache: %v", err)
	}

	got, _ := r.Lookup("anthropic", "claude-sonnet-4-6")
	if got.ContextWindow != 123 {
		t.Fatalf("initial stale-cache ContextWindow = %d, want 123", got.ContextWindow)
	}

	waitFor(t, func() bool { return attempts.Load() == 1 })
	waitFor(t, func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return !r.inFlight
	})

	// Repeated hot-path calls within the TTL must not launch another offline
	// refresh attempt, and the stale cache remains available until a successful
	// refresh swaps in newer specs.
	for i := 0; i < 5; i++ {
		got, _ = r.Lookup("anthropic", "claude-sonnet-4-6")
		if got.ContextWindow != 123 {
			t.Fatalf("post-failure stale-cache ContextWindow = %d, want 123", got.ContextWindow)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if got := attempts.Load(); got != 1 {
		t.Fatalf("HTTP attempts within TTL = %d, want 1", got)
	}
}

func TestModelSpecs_ForceRefreshIsOneShot(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		_, _ = w.Write([]byte(modelsDevJSON(t, true)))
	}))
	defer srv.Close()

	r := newTestRegistry(t, srv.URL)
	r.autoFetch = true
	r.force = true
	r.ttl = time.Hour

	_, _ = r.Lookup("anthropic", "claude-sonnet-4-6")
	waitFor(t, func() bool { return attempts.Load() == 1 })
	waitFor(t, func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return !r.inFlight
	})

	for i := 0; i < 3; i++ {
		_, _ = r.Lookup("anthropic", "claude-sonnet-4-6")
	}
	time.Sleep(50 * time.Millisecond)
	if got := attempts.Load(); got != 1 {
		t.Fatalf("force-refresh HTTP attempts within TTL = %d, want 1", got)
	}
}

// SetDefault is the seam every cross-package test relies on, so it is pinned
// here: it swaps the process-wide registry and the returned restore puts the
// previous one back, including the not-yet-built (nil) state a fresh process
// starts in.
func TestSetDefault_SwapsAndRestores(t *testing.T) {
	seeded := newTestRegistry(t, "http://127.0.0.1:0")
	seeded.mu.Lock()
	seeded.indexLocked(map[string]Spec{"anthropic/claude-opus-5": {ContextWindow: 42}})
	seeded.loadedAt = time.Now()
	seeded.diskTried = true
	seeded.mu.Unlock()

	before := Default()
	restore := SetDefault(seeded)
	if got, ok := Default().Lookup("anthropic", "claude-opus-5"); !ok || got.ContextWindow != 42 {
		t.Fatalf("after SetDefault, lookup = %+v, %v; want the seeded registry", got, ok)
	}
	restore()
	if Default() != before {
		t.Error("restore did not put the previous default back")
	}
}

// OptionsFromEnv is read LAZILY by Default. A package-var initializer would
// have read it at import time, which is exactly what made an env-based fixture
// unreachable from another package's test.
func TestOptionsFromEnv_ReadsKnobs(t *testing.T) {
	t.Setenv("ITERION_MODEL_SPECS", "off")
	t.Setenv("ITERION_MODEL_SPECS_URL", "https://example.invalid/api.json")
	t.Setenv("ITERION_MODEL_SPECS_CACHE", "/tmp/iterion-fixture-cache.json")
	t.Setenv("ITERION_MODEL_SPECS_TTL", "90s")
	t.Setenv("ITERION_MODEL_SPECS_REFRESH", "1")

	opts := OptionsFromEnv()
	if !opts.Disabled || !opts.ForceRefresh {
		t.Errorf("flags = disabled:%v force:%v, want both true", opts.Disabled, opts.ForceRefresh)
	}
	if opts.URL != "https://example.invalid/api.json" || opts.CachePath != "/tmp/iterion-fixture-cache.json" {
		t.Errorf("url/cache = %q / %q", opts.URL, opts.CachePath)
	}
	if opts.TTL != 90*time.Second {
		t.Errorf("TTL = %v, want 90s", opts.TTL)
	}

	// An unparsable duration leaves TTL unset so New falls back to the default
	// rather than to zero, which ensureFresh would read as permanently stale.
	t.Setenv("ITERION_MODEL_SPECS_TTL", "not-a-duration")
	if got := New(OptionsFromEnv()).ttl; got != defaultTTL {
		t.Errorf("ttl after unparsable TTL = %v, want the %v default", got, defaultTTL)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not satisfied before timeout")
}

func boolp(b bool) *bool { return &b }

// NewSeeded is the cross-package fixture seam, and its whole reason to exist is
// that a test asserting on a price must not resolve against — or overwrite —
// whatever the host's ~/.iterion cache last fetched. NoAutoFetch alone only
// suppressed the BACKGROUND refresh: an explicit Refresh still reached
// models.dev and rewrote DefaultCachePath(), and `iterion models --refresh` /
// `models pricing --refresh` are exactly the paths a CLI test drives into it.
func TestNewSeeded_RefreshTouchesNeitherNetworkNorDisk(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		_, _ = w.Write([]byte(`{"anthropic":{"models":{"claude-opus-5":{"limit":{"context":999}}}}}`))
	}))
	defer srv.Close()

	r := NewSeeded(map[string]Spec{"anthropic/claude-opus-5": {ContextWindow: 42}})
	if r.cachePath != "" {
		t.Errorf("seeded cachePath = %q, want empty — never the host's real cache", r.cachePath)
	}
	// Stand in for models.dev: even pointed at a live endpoint it must not go.
	r.url = srv.URL

	if err := r.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh on a seeded registry = %v, want nil (a no-op, not an error)", err)
	}
	if got := attempts.Load(); got != 0 {
		t.Errorf("HTTP attempts = %d, want 0 — a seeded registry must never fetch", got)
	}
	// And the fixture is still the answer: sealing must not empty the table.
	if got, ok := r.Lookup("anthropic", "claude-opus-5"); !ok || got.ContextWindow != 42 {
		t.Errorf("post-Refresh lookup = %+v, %v; want the seeded 42", got, ok)
	}
}

// inFlight is set by ensureFresh and cleared by Refresh's defer, so every
// Refresh return path must clear it — including the no-op ones, which sit
// BEFORE that defer. A path that returned without clearing would leave the
// registry believing a refresh is running and never spawn another one.
func TestRefresh_NoOpPathsStillReleaseInFlight(t *testing.T) {
	for _, tc := range []struct {
		name string
		reg  *Registry
	}{
		{"disabled", New(Options{Disabled: true})},
		{"sealed", NewSeeded(map[string]Spec{"anthropic/claude-opus-5": {ContextWindow: 1}})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.reg.mu.Lock()
			tc.reg.inFlight = true // what ensureFresh stakes before spawning
			tc.reg.mu.Unlock()

			if err := tc.reg.Refresh(context.Background()); err != nil {
				t.Fatalf("Refresh = %v, want nil", err)
			}

			tc.reg.mu.Lock()
			defer tc.reg.mu.Unlock()
			if tc.reg.inFlight {
				t.Error("inFlight still set after a no-op Refresh — the background refresh is wedged for the process")
			}
		})
	}
}

package model

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// TestMain disables auto-fetch on the package-global registry so unrelated
// tests in this package never spawn a background network goroutine. Local
// registries built inside individual tests still fetch from httptest servers.
func TestMain(m *testing.M) {
	specs.mu.Lock()
	specs.autoFetch = false
	// Point the package-global registry's cache at a nonexistent path so
	// curated-fallback assertions (TestCapabilitiesForModel) are deterministic
	// regardless of the host's ~/.iterion/model-specs-cache.json. Without this,
	// merge() lazily loads that real cache (ensureFresh → loadDiskCacheLocked)
	// and overrides curated with live models.dev values — so the curated-equality
	// checks flaked on dev machines that had run a real bot (clean CI has no
	// cache, so it only failed locally).
	specs.cachePath = filepath.Join(os.TempDir(), "iterion-modelspecs-test-absent.json")
	specs.diskTried = false
	specs.mu.Unlock()
	os.Exit(m.Run())
}

// modelsDevJSON returns a minimal models.dev-shaped api.json. Pass includeGLM
// to control whether glm-5.2 is present (it is omitted by real aggregators
// today, which is exactly the curated-fallback case).
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
// path. autoFetch is left false; tests drive refresh() synchronously.
func newTestRegistry(t *testing.T, url string) *specRegistry {
	t.Helper()
	return &specRegistry{
		url:       url,
		cachePath: filepath.Join(t.TempDir(), "model-specs-cache.json"),
		ttl:       defaultSpecTTL,
		client:    &http.Client{Timeout: defaultSpecTimeout},
		enabled:   true,
		autoFetch: false,
		byFull:    map[string]fetchedSpec{},
		byModel:   map[string]fetchedSpec{},
	}
}

func TestModelSpecs_FetchAndMerge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(modelsDevJSON(t, true)))
	}))
	defer srv.Close()

	r := newTestRegistry(t, srv.URL)
	if err := r.refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	// Fetched ContextWindow>0 overrides the curated default (which has no
	// context window for claude).
	curated := curatedCapabilities("anthropic", "claude-sonnet-4-6")
	got := r.merge("anthropic", "claude-sonnet-4-6", curated)
	if got.ContextWindow != 1_000_000 {
		t.Errorf("claude ContextWindow = %d, want 1000000", got.ContextWindow)
	}
	if !got.Reasoning || !got.ToolCall || !got.Temperature {
		t.Errorf("claude flags = %+v, want all true", got)
	}

	// openai/gpt-5: aggregator says temperature=false → overrides heuristic.
	gpt := r.merge("openai", "gpt-5", curatedCapabilities("openai", "gpt-5"))
	if gpt.Temperature {
		t.Errorf("gpt-5 Temperature = true, want false (from aggregator)")
	}
	if gpt.ContextWindow != 400_000 {
		t.Errorf("gpt-5 ContextWindow = %d, want 400000", gpt.ContextWindow)
	}

	// The cache file must have been written for subsequent processes.
	if _, err := os.Stat(r.cachePath); err != nil {
		t.Errorf("cache file not written: %v", err)
	}
}

func TestModelSpecs_OfflineFallback(t *testing.T) {
	// Point at a server we immediately close → connection refused.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	url := srv.URL
	srv.Close()

	r := newTestRegistry(t, url)
	r.client = &http.Client{Timeout: 200 * time.Millisecond}
	// refresh must not panic and must return an error, but never block a run.
	if err := r.refresh(context.Background()); err == nil {
		t.Fatal("expected error from offline refresh")
	}

	// merge degrades to curated — glm-5.2 still resolves to 1M.
	got := r.merge("anthropic", "glm-5.2", curatedCapabilities("anthropic", "glm-5.2"))
	if got.ContextWindow != 1_000_000 {
		t.Errorf("offline glm-5.2 ContextWindow = %d, want 1000000 (curated)", got.ContextWindow)
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
	cf := specCacheFile{
		FetchedAt: time.Now(),
		Source:    "test",
		Specs: map[string]fetchedSpec{
			"anthropic/claude-sonnet-4-6": {ContextWindow: 1_000_000, Reasoning: boolp(true), ToolCall: boolp(true), Temperature: boolp(true)},
		},
	}
	data, _ := json.MarshalIndent(cf, "", "  ")
	if err := os.WriteFile(r.cachePath, data, 0o644); err != nil {
		t.Fatalf("write cache: %v", err)
	}

	got := r.merge("anthropic", "claude-sonnet-4-6", curatedCapabilities("anthropic", "claude-sonnet-4-6"))
	if got.ContextWindow != 1_000_000 {
		t.Errorf("cache-hit ContextWindow = %d, want 1000000", got.ContextWindow)
	}
	// Second call within TTL also performs no fetch.
	_ = r.merge("anthropic", "claude-sonnet-4-6", curatedCapabilities("anthropic", "claude-sonnet-4-6"))
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
	cf := specCacheFile{
		FetchedAt: time.Now().Add(-time.Hour),
		Source:    "test",
		Specs:     map[string]fetchedSpec{"anthropic/claude-sonnet-4-6": {ContextWindow: 123}},
	}
	data, _ := json.MarshalIndent(cf, "", "  ")
	if err := os.WriteFile(r.cachePath, data, 0o644); err != nil {
		t.Fatalf("write cache: %v", err)
	}

	// A synchronous refresh replaces the stale value.
	if err := r.refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	got := r.merge("anthropic", "claude-sonnet-4-6", curatedCapabilities("anthropic", "claude-sonnet-4-6"))
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
	if err := r.refresh(context.Background()); err == nil {
		t.Fatal("expected error from malformed response")
	}
	// Degrades to curated.
	got := r.merge("openai", "o1-preview", curatedCapabilities("openai", "o1-preview"))
	want := curatedCapabilities("openai", "o1-preview")
	if got != want {
		t.Errorf("malformed-response merge = %+v, want curated %+v", got, want)
	}
}

// TestModelSpecs_GLM52FallbackWhenOmitted is the explicit requirement-6 case:
// when the aggregator omits glm-5.2, the curated 1M value must win; glm-5.1 /
// glm-4.6 stay 200K.
func TestModelSpecs_GLM52FallbackWhenOmitted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(modelsDevJSON(t, false))) // GLM omitted
	}))
	defer srv.Close()

	r := newTestRegistry(t, srv.URL)
	if err := r.refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	cases := []struct {
		model string
		want  int
	}{
		{"glm-5.2", 1_000_000},
		{"glm-5.1", 200_000},
		{"glm-4.6", 200_000},
	}
	for _, c := range cases {
		got := r.merge("anthropic", c.model, curatedCapabilities("anthropic", c.model))
		if got.ContextWindow != c.want {
			t.Errorf("%s ContextWindow = %d, want %d (curated fallback)", c.model, got.ContextWindow, c.want)
		}
	}
}

// TestModelSpecs_DisabledIsPureCurated verifies ITERION_MODEL_SPECS=off yields
// the curated fallback with no aggregator contact.
func TestModelSpecs_DisabledIsPureCurated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("aggregator hit while disabled")
	}))
	defer srv.Close()

	r := newTestRegistry(t, srv.URL)
	r.enabled = false
	got := r.merge("anthropic", "claude-sonnet-4-6", curatedCapabilities("anthropic", "claude-sonnet-4-6"))
	if got != curatedCapabilities("anthropic", "claude-sonnet-4-6") {
		t.Errorf("disabled merge = %+v, want curated", got)
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
	cf := specCacheFile{
		FetchedAt: time.Now().Add(-2 * time.Hour),
		Source:    "test",
		Specs: map[string]fetchedSpec{
			"anthropic/claude-sonnet-4-6": {ContextWindow: 123, Reasoning: boolp(true)},
		},
	}
	data, _ := json.MarshalIndent(cf, "", "  ")
	if err := os.WriteFile(r.cachePath, data, 0o644); err != nil {
		t.Fatalf("write cache: %v", err)
	}

	curated := curatedCapabilities("anthropic", "claude-sonnet-4-6")
	got := r.merge("anthropic", "claude-sonnet-4-6", curated)
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
		got = r.merge("anthropic", "claude-sonnet-4-6", curated)
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

	curated := curatedCapabilities("anthropic", "claude-sonnet-4-6")
	_ = r.merge("anthropic", "claude-sonnet-4-6", curated)
	waitFor(t, func() bool { return attempts.Load() == 1 })
	waitFor(t, func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return !r.inFlight
	})

	for i := 0; i < 3; i++ {
		_ = r.merge("anthropic", "claude-sonnet-4-6", curated)
	}
	time.Sleep(50 * time.Millisecond)
	if got := attempts.Load(); got != 1 {
		t.Fatalf("force-refresh HTTP attempts within TTL = %d, want 1", got)
	}
}

func TestModelSpecs_ConcurrentForceRefreshIsCoalesced(t *testing.T) {
	var attempts atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		<-release
		_, _ = w.Write([]byte(modelsDevJSON(t, true)))
	}))
	defer srv.Close()

	r := newTestRegistry(t, srv.URL)
	const callers = 24
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		go func() { errs <- r.refresh(context.Background()) }()
	}
	waitFor(t, func() bool { return attempts.Load() == 1 })
	// Give every goroutine time to join the in-flight channel. If refresh
	// starts independent work, attempts rises before the first is released.
	time.Sleep(25 * time.Millisecond)
	if got := attempts.Load(); got != 1 {
		t.Fatalf("concurrent refresh attempts = %d, want exactly one", got)
	}
	close(release)
	for i := 0; i < callers; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("waiter %d: %v", i, err)
		}
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("HTTP attempts after all waiters = %d, want 1", got)
	}
}

func TestModelSpecs_CancelledWaiterDoesNotCancelSharedRefresh(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		_, _ = w.Write([]byte(modelsDevJSON(t, true)))
	}))
	defer srv.Close()

	r := newTestRegistry(t, srv.URL)
	ownerDone := make(chan error, 1)
	go func() { ownerDone <- r.refresh(context.Background()) }()
	<-started

	ctx, cancel := context.WithCancel(context.Background())
	waiterDone := make(chan error, 1)
	go func() { waiterDone <- r.refresh(ctx) }()
	cancel()
	if err := <-waiterDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled waiter = %v, want context.Canceled", err)
	}
	close(release)
	if err := <-ownerDone; err != nil {
		t.Fatalf("shared refresh was cancelled with a waiter: %v", err)
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

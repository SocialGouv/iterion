package apikit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func sampleRequest(text string) *CacheRequest {
	return &CacheRequest{
		Model:    "claude-3-7-sonnet-latest",
		System:   "system",
		Messages: []map[string]string{{"role": "user", "content": text}},
	}
}

func sampleResponse(cacheReadTokens, outputTokens uint32, text string) *CacheResponse {
	raw, _ := json.Marshal(map[string]any{
		"id":          "msg_test",
		"type":        "message",
		"role":        "assistant",
		"content":     []map[string]string{{"type": "text", "text": text}},
		"model":       "claude-3-7-sonnet-latest",
		"stop_reason": "end_turn",
	})
	return &CacheResponse{
		Raw: raw,
		Usage: CacheUsage{
			InputTokens:              10,
			CacheCreationInputTokens: 5,
			CacheReadInputTokens:     cacheReadTokens,
			OutputTokens:             outputTokens,
		},
	}
}

func TestNewCacheEmptyStats(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_HOME", t.TempDir())
	cache := NewPromptCache("test-session")
	stats := cache.Stats()
	if stats.TrackedRequests != 0 || stats.CompletionCacheHits != 0 {
		t.Error("new cache should have zero stats")
	}
}

func TestRecordUsageUpdatesStats(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_HOME", t.TempDir())
	cache := NewPromptCache("test-session")

	request := sampleRequest("hello")
	usage := &CacheUsage{
		InputTokens:              100,
		CacheCreationInputTokens: 50,
		CacheReadInputTokens:     30,
		OutputTokens:             20,
	}

	record := cache.RecordUsage(request, usage)
	if record.Stats.TrackedRequests != 1 {
		t.Errorf("expected 1 tracked request, got %d", record.Stats.TrackedRequests)
	}
	if record.Stats.TotalCacheCreationInputTokens != 50 {
		t.Errorf("expected 50 creation tokens, got %d", record.Stats.TotalCacheCreationInputTokens)
	}
	if record.Stats.TotalCacheReadInputTokens != 30 {
		t.Errorf("expected 30 read tokens, got %d", record.Stats.TotalCacheReadInputTokens)
	}
}

func TestLookupCompletionMissAndHit(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_HOME", t.TempDir())
	cache := NewPromptCache("test-session")

	request := sampleRequest("cache me")
	response := sampleResponse(42, 12, "cached")

	// Miss
	result := cache.LookupCompletion(request)
	if result != nil {
		t.Error("expected miss on empty cache")
	}

	// Record
	record := cache.RecordResponse(request, response)
	if record.CacheBreak != nil {
		t.Error("first record should not detect cache break")
	}

	// Hit
	cached := cache.LookupCompletion(request)
	if cached == nil {
		t.Fatal("expected cache hit")
	}

	stats := cache.Stats()
	if stats.CompletionCacheHits != 1 {
		t.Errorf("expected 1 hit, got %d", stats.CompletionCacheHits)
	}
	if stats.CompletionCacheMisses != 1 {
		t.Errorf("expected 1 miss, got %d", stats.CompletionCacheMisses)
	}
	if stats.CompletionCacheWrites != 1 {
		t.Errorf("expected 1 write, got %d", stats.CompletionCacheWrites)
	}
}

func TestLookupCompletionExpired(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_HOME", t.TempDir())
	cache := NewPromptCacheWithConfig(PromptCacheConfig{
		SessionID:         "expired-session",
		CompletionTTL:     0, // immediate expiry
		PromptTTL:         5 * time.Minute,
		CacheBreakMinDrop: defaultBreakMinDrop,
	})

	request := sampleRequest("expire me")
	response := sampleResponse(7, 3, "stale")
	cache.RecordResponse(request, response)

	cached := cache.LookupCompletion(request)
	if cached != nil {
		t.Error("expired entry should not be returned")
	}

	stats := cache.Stats()
	if stats.CompletionCacheHits != 0 {
		t.Errorf("expected 0 hits, got %d", stats.CompletionCacheHits)
	}
}

func TestDistinctRequestsDoNotCollide(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_HOME", t.TempDir())
	cache := NewPromptCache("distinct-session")

	first := sampleRequest("first")
	second := sampleRequest("second")
	response := sampleResponse(42, 12, "cached")

	cache.RecordResponse(first, response)

	cached := cache.LookupCompletion(second)
	if cached != nil {
		t.Error("different request should not match cached entry")
	}
}

func TestCacheBreakOnTokenDrop(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_HOME", t.TempDir())
	cache := NewPromptCache("break-session")

	request := sampleRequest("same")
	highUsage := &CacheUsage{CacheReadInputTokens: 6000}
	lowUsage := &CacheUsage{CacheReadInputTokens: 1000}

	cache.RecordUsage(request, highUsage)
	record := cache.RecordUsage(request, lowUsage)

	if record.CacheBreak == nil {
		t.Fatal("expected cache break event")
	}
	if !record.CacheBreak.Unexpected {
		t.Error("same fingerprint with token drop should be unexpected")
	}
	if record.CacheBreak.TokenDrop != 5000 {
		t.Errorf("expected 5000 token drop, got %d", record.CacheBreak.TokenDrop)
	}
}

func TestCacheBreakOnChangedPrompt(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_HOME", t.TempDir())
	cache := NewPromptCache("changed-session")

	first := sampleRequest("first")
	second := sampleRequest("second")
	highUsage := &CacheUsage{CacheReadInputTokens: 6000}
	lowUsage := &CacheUsage{CacheReadInputTokens: 1000}

	cache.RecordUsage(first, highUsage)
	record := cache.RecordUsage(second, lowUsage)

	if record.CacheBreak == nil {
		t.Fatal("expected cache break event")
	}
	if record.CacheBreak.Unexpected {
		t.Error("changed message should be an expected break")
	}
}

func TestSmallTokenDropNotDetected(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_HOME", t.TempDir())
	cache := NewPromptCache("small-drop-session")

	request := sampleRequest("same")
	highUsage := &CacheUsage{CacheReadInputTokens: 6000}
	slightlyLow := &CacheUsage{CacheReadInputTokens: 5000} // drop = 1000 < 2000 min

	cache.RecordUsage(request, highUsage)
	record := cache.RecordUsage(request, slightlyLow)

	if record.CacheBreak != nil {
		t.Error("small token drop should not trigger cache break")
	}
}

func TestConcurrentAccess(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_HOME", t.TempDir())
	cache := NewPromptCache("concurrent-session")

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			req := sampleRequest("concurrent")
			usage := &CacheUsage{CacheReadInputTokens: uint32(n * 100)}
			cache.RecordUsage(req, usage)
		}(i)
	}
	wg.Wait()

	stats := cache.Stats()
	if stats.TrackedRequests != 50 {
		t.Errorf("expected 50 tracked requests, got %d", stats.TrackedRequests)
	}
}

func TestFilePersistenceRoundTrip(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_HOME", root)

	// Create cache and record data
	cache1 := NewPromptCache("persist-session")
	request := sampleRequest("persist me")
	response := sampleResponse(42, 12, "persisted")
	cache1.RecordResponse(request, response)

	stats1 := cache1.Stats()
	if stats1.CompletionCacheWrites != 1 {
		t.Fatal("expected 1 write")
	}

	// Re-open the cache from disk
	cache2 := NewPromptCache("persist-session")
	stats2 := cache2.Stats()

	if stats2.CompletionCacheWrites != 1 {
		t.Errorf("persisted writes: expected 1, got %d", stats2.CompletionCacheWrites)
	}
	if stats2.TrackedRequests != 1 {
		t.Errorf("persisted tracked: expected 1, got %d", stats2.TrackedRequests)
	}

	// Lookup should still work
	cached := cache2.LookupCompletion(request)
	if cached == nil {
		t.Error("persisted completion should be loadable")
	}
}

func TestSanitizePathSegment(t *testing.T) {
	t.Run("replaces special chars", func(t *testing.T) {
		result := SanitizePathSegment("session:/with spaces")
		if result != "session--with-spaces" {
			t.Errorf("expected 'session--with-spaces', got %q", result)
		}
	})

	t.Run("caps long values", func(t *testing.T) {
		long := ""
		for i := 0; i < 200; i++ {
			long += "x"
		}
		result := SanitizePathSegment(long)
		if len(result) > maxSanitizedLength {
			t.Errorf("sanitized length %d exceeds max %d", len(result), maxSanitizedLength)
		}
	})

	t.Run("preserves alphanumeric", func(t *testing.T) {
		result := SanitizePathSegment("abc123")
		if result != "abc123" {
			t.Errorf("alphanumeric should be preserved, got %q", result)
		}
	})
}

func TestRequestHashesAreVersionedAndStable(t *testing.T) {
	request := sampleRequest("stable")
	first := requestHashHex(request)
	second := requestHashHex(request)

	if first != second {
		t.Error("same request should produce same hash")
	}
	if first[:2] != "v1" {
		t.Errorf("hash should start with v1, got %s", first[:2])
	}
}

func TestFNV1aKnownValue(t *testing.T) {
	// FNV-1a of empty byte slice should equal the offset basis
	hash := StableHashBytes(nil)
	if hash != fnvOffsetBasis {
		t.Errorf("FNV-1a of empty input should equal offset basis, got %x", hash)
	}

	// FNV-1a must be deterministic
	data := []byte("hello world")
	h1 := StableHashBytes(data)
	h2 := StableHashBytes(data)
	if h1 != h2 {
		t.Error("FNV-1a must be deterministic")
	}

	// Different inputs should produce different hashes
	h3 := StableHashBytes([]byte("hello world!"))
	if h1 == h3 {
		t.Error("different inputs should produce different hashes")
	}
}

func TestPromptCachePathsForSession(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_HOME", "/tmp/test-config")
	paths := PromptCachePathsForSession("session:/with spaces")

	sessionDir := filepath.Base(paths.SessionDir)
	if sessionDir != "session--with-spaces" {
		t.Errorf("session dir name: got %q", sessionDir)
	}
	if filepath.Base(paths.CompletionDir) != "completions" {
		t.Error("completion dir should end with 'completions'")
	}
	if filepath.Base(paths.StatsPath) != "stats.json" {
		t.Error("stats path should end with 'stats.json'")
	}
	if filepath.Base(paths.SessionStatePath) != "session-state.json" {
		t.Error("session state path should end with 'session-state.json'")
	}
}

func TestCacheBreakEmitsTelemetryEvent(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_HOME", t.TempDir())

	sink := &MemoryTelemetrySink{}
	cache := NewPromptCache("telemetry-session").WithTelemetrySink(sink)

	request := sampleRequest("same")
	highUsage := &CacheUsage{CacheReadInputTokens: 6000}
	lowUsage := &CacheUsage{CacheReadInputTokens: 1000}

	cache.RecordUsage(request, highUsage)
	record := cache.RecordUsage(request, lowUsage)

	if record.CacheBreak == nil {
		t.Fatal("expected cache break event")
	}

	events := sink.Events()
	if len(events) != 1 {
		t.Fatalf("expected 1 telemetry event, got %d", len(events))
	}

	ev := events[0]
	if ev.Type != EventTypeSessionTrace {
		t.Errorf("expected event type %s, got %s", EventTypeSessionTrace, ev.Type)
	}
	if ev.SessionTrace == nil {
		t.Fatal("expected non-nil SessionTrace")
	}
	if ev.SessionTrace.Name != "prompt_cache_break" {
		t.Errorf("expected name 'prompt_cache_break', got %q", ev.SessionTrace.Name)
	}
	if ev.SessionTrace.TimestampMs == 0 {
		t.Error("expected non-zero timestamp")
	}

	attrs := ev.SessionTrace.Attributes
	if attrs["unexpected"] != true {
		t.Errorf("expected unexpected=true, got %v", attrs["unexpected"])
	}
	if attrs["reason"] != "cache read tokens dropped while prompt fingerprint remained stable" {
		t.Errorf("unexpected reason: %v", attrs["reason"])
	}
	if attrs["token_drop"] != uint32(5000) {
		t.Errorf("expected token_drop=5000, got %v", attrs["token_drop"])
	}
}

func TestCacheBreakWithoutSinkNoPanic(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_HOME", t.TempDir())

	// No sink attached — should not panic
	cache := NewPromptCache("no-sink-session")

	request := sampleRequest("same")
	highUsage := &CacheUsage{CacheReadInputTokens: 6000}
	lowUsage := &CacheUsage{CacheReadInputTokens: 1000}

	cache.RecordUsage(request, highUsage)
	record := cache.RecordUsage(request, lowUsage)

	if record.CacheBreak == nil {
		t.Fatal("expected cache break event")
	}
	// If we got here without panicking, the test passes.
}

func TestBaseCacheRootPrecedence(t *testing.T) {
	t.Run("CLAUDE_CONFIG_HOME takes precedence", func(t *testing.T) {
		t.Setenv("CLAUDE_CONFIG_HOME", "/custom/config")
		t.Setenv("HOME", "/home/user")
		root := baseCacheRoot()
		expected := filepath.Join("/custom/config", "cache", "prompt-cache")
		if root != expected {
			t.Errorf("expected %s, got %s", expected, root)
		}
	})

	t.Run("falls back to HOME", func(t *testing.T) {
		t.Setenv("CLAUDE_CONFIG_HOME", "")
		t.Setenv("HOME", "/home/user")
		root := baseCacheRoot()
		expected := filepath.Join("/home/user", ".claude", "cache", "prompt-cache")
		if root != expected {
			t.Errorf("expected %s, got %s", expected, root)
		}
	})

	t.Run("falls back to temp dir", func(t *testing.T) {
		t.Setenv("CLAUDE_CONFIG_HOME", "")
		t.Setenv("HOME", "")
		root := baseCacheRoot()
		expected := filepath.Join(os.TempDir(), "claude-prompt-cache")
		if root != expected {
			t.Errorf("expected %s, got %s", expected, root)
		}
	})
}

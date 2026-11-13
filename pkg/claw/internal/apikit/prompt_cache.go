package apikit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	defaultCompletionTTLSecs = 30
	defaultPromptTTLSecs     = 5 * 60
	defaultBreakMinDrop      = 2000
)

// PromptCacheConfig configures prompt cache behavior.
type PromptCacheConfig struct {
	SessionID         string
	CompletionTTL     time.Duration
	PromptTTL         time.Duration
	CacheBreakMinDrop uint32
}

// NewPromptCacheConfig creates a config with defaults for the given session.
func NewPromptCacheConfig(sessionID string) PromptCacheConfig {
	return PromptCacheConfig{
		SessionID:         sessionID,
		CompletionTTL:     time.Duration(defaultCompletionTTLSecs) * time.Second,
		PromptTTL:         time.Duration(defaultPromptTTLSecs) * time.Second,
		CacheBreakMinDrop: defaultBreakMinDrop,
	}
}

// CacheBreakEvent describes a detected prompt cache invalidation.
type CacheBreakEvent struct {
	Unexpected                   bool   `json:"unexpected"`
	Reason                       string `json:"reason"`
	PreviousCacheReadInputTokens uint32 `json:"previous_cache_read_input_tokens"`
	CurrentCacheReadInputTokens  uint32 `json:"current_cache_read_input_tokens"`
	TokenDrop                    uint32 `json:"token_drop"`
}

// PromptCacheRecord is the result of recording a response or usage.
type PromptCacheRecord struct {
	CacheBreak *CacheBreakEvent
	Stats      PromptCacheStats
}

// PromptCacheStats tracks prompt cache performance metrics.
type PromptCacheStats struct {
	TrackedRequests               uint64  `json:"tracked_requests"`
	CompletionCacheHits           uint64  `json:"completion_cache_hits"`
	CompletionCacheMisses         uint64  `json:"completion_cache_misses"`
	CompletionCacheWrites         uint64  `json:"completion_cache_writes"`
	ExpectedInvalidations         uint64  `json:"expected_invalidations"`
	UnexpectedCacheBreaks         uint64  `json:"unexpected_cache_breaks"`
	TotalCacheCreationInputTokens uint64  `json:"total_cache_creation_input_tokens"`
	TotalCacheReadInputTokens     uint64  `json:"total_cache_read_input_tokens"`
	LastCacheCreationInputTokens  *uint32 `json:"last_cache_creation_input_tokens,omitempty"`
	LastCacheReadInputTokens      *uint32 `json:"last_cache_read_input_tokens,omitempty"`
	LastRequestHash               string  `json:"last_request_hash,omitempty"`
	LastCompletionCacheKey        string  `json:"last_completion_cache_key,omitempty"`
	LastBreakReason               string  `json:"last_break_reason,omitempty"`
	LastCacheSource               string  `json:"last_cache_source,omitempty"`
}

// CacheUsage represents token usage info relevant to prompt caching.
type CacheUsage struct {
	InputTokens              uint32
	CacheCreationInputTokens uint32
	CacheReadInputTokens     uint32
	OutputTokens             uint32
}

// CacheRequest represents a simplified request for prompt cache operations.
// Callers marshal their actual request type into this.
type CacheRequest struct {
	Model    string `json:"model"`
	System   any    `json:"system,omitempty"`
	Messages any    `json:"messages"`
	Tools    any    `json:"tools,omitempty"`
}

// CacheResponse represents a cached response. Callers provide the full
// response as raw JSON for storage and retrieval.
type CacheResponse struct {
	Raw   json.RawMessage `json:"raw"`
	Usage CacheUsage      `json:"usage"`
}

// PromptCache manages completion caching and cache break detection.
// Safe for concurrent use.
type PromptCache struct {
	mu       sync.Mutex
	config   PromptCacheConfig
	paths    PromptCachePaths
	stats    PromptCacheStats
	previous *trackedPromptState
	sink     TelemetrySink
}

// NewPromptCache creates a prompt cache for the given session ID.
func NewPromptCache(sessionID string) *PromptCache {
	return NewPromptCacheWithConfig(NewPromptCacheConfig(sessionID))
}

// NewPromptCacheWithConfig creates a prompt cache with explicit config.
func NewPromptCacheWithConfig(config PromptCacheConfig) *PromptCache {
	paths := PromptCachePathsForSession(config.SessionID)
	stats := readJSONFile[PromptCacheStats](paths.StatsPath)
	previous := readJSONFilePtr[trackedPromptState](paths.SessionStatePath)
	return &PromptCache{
		config:   config,
		paths:    paths,
		stats:    stats,
		previous: previous,
	}
}

// WithTelemetrySink attaches a telemetry sink for cache break event logging.
func (c *PromptCache) WithTelemetrySink(sink TelemetrySink) *PromptCache {
	c.sink = sink
	return c
}

// Paths returns the cache directory paths.
func (c *PromptCache) Paths() PromptCachePaths {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.paths
}

// Stats returns a snapshot of the cache statistics.
func (c *PromptCache) Stats() PromptCacheStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stats
}

// LookupCompletion checks for a cached completion matching the request.
// Returns the cached response if found and not expired, nil otherwise.
func (c *PromptCache) LookupCompletion(request *CacheRequest) *CacheResponse {
	requestHash := requestHashHex(request)

	c.mu.Lock()
	entryPath := c.paths.CompletionEntryPath(requestHash)
	ttl := c.config.CompletionTTL
	c.mu.Unlock()

	entry := readJSONFilePtr[completionCacheEntry](entryPath)
	if entry == nil {
		c.mu.Lock()
		c.stats.CompletionCacheMisses++
		c.stats.LastCompletionCacheKey = requestHash
		c.persistState()
		c.mu.Unlock()
		return nil
	}

	if entry.FingerprintVersion != currentFingerprintVersion {
		c.mu.Lock()
		c.stats.CompletionCacheMisses++
		c.stats.LastCompletionCacheKey = requestHash
		_ = os.Remove(entryPath)
		c.persistState()
		c.mu.Unlock()
		return nil
	}

	expired := nowUnixSecs()-entry.CachedAtUnixSecs >= uint64(ttl.Seconds())

	c.mu.Lock()
	defer c.mu.Unlock()
	c.stats.LastCompletionCacheKey = requestHash

	if expired {
		c.stats.CompletionCacheMisses++
		_ = os.Remove(entryPath)
		c.persistState()
		return nil
	}

	c.stats.CompletionCacheHits++
	applyUsageToStats(&c.stats, &entry.Response.Usage, requestHash, "completion-cache")
	c.previous = trackedFromUsage(request, &entry.Response.Usage)
	c.persistState()
	return &entry.Response
}

// RecordResponse records a response and caches it for future lookups.
func (c *PromptCache) RecordResponse(request *CacheRequest, response *CacheResponse) PromptCacheRecord {
	return c.recordUsageInternal(request, &response.Usage, response)
}

// RecordUsage records token usage without caching the response.
func (c *PromptCache) RecordUsage(request *CacheRequest, usage *CacheUsage) PromptCacheRecord {
	return c.recordUsageInternal(request, usage, nil)
}

func (c *PromptCache) recordUsageInternal(request *CacheRequest, usage *CacheUsage, response *CacheResponse) PromptCacheRecord {
	requestHash := requestHashHex(request)

	c.mu.Lock()
	defer c.mu.Unlock()

	current := trackedFromUsage(request, usage)
	cacheBreak := detectCacheBreak(&c.config, c.previous, current)

	c.stats.TrackedRequests++
	applyUsageToStats(&c.stats, usage, requestHash, "api-response")

	if cacheBreak != nil {
		if cacheBreak.Unexpected {
			c.stats.UnexpectedCacheBreaks++
		} else {
			c.stats.ExpectedInvalidations++
		}
		c.stats.LastBreakReason = cacheBreak.Reason

		if c.sink != nil {
			c.sink.Record(TelemetryEvent{
				Type: EventTypeSessionTrace,
				SessionTrace: &SessionTraceRecord{
					Name:        "prompt_cache_break",
					TimestampMs: uint64(time.Now().UnixMilli()),
					Attributes: map[string]any{
						"unexpected":          cacheBreak.Unexpected,
						"reason":              cacheBreak.Reason,
						"token_drop":          cacheBreak.TokenDrop,
						"previous_cache_read": cacheBreak.PreviousCacheReadInputTokens,
						"current_cache_read":  cacheBreak.CurrentCacheReadInputTokens,
					},
				},
			})
		}
	}

	c.previous = current

	if response != nil {
		c.writeCompletionEntry(requestHash, response)
		c.stats.CompletionCacheWrites++
	}

	c.persistState()

	return PromptCacheRecord{
		CacheBreak: cacheBreak,
		Stats:      c.stats,
	}
}

func (c *PromptCache) writeCompletionEntry(requestHash string, response *CacheResponse) {
	_ = c.ensureCacheDirs()
	entry := completionCacheEntry{
		CachedAtUnixSecs:   nowUnixSecs(),
		FingerprintVersion: currentFingerprintVersion,
		Response:           *response,
	}
	_ = writeJSONFile(c.paths.CompletionEntryPath(requestHash), entry)
}

func (c *PromptCache) persistState() {
	_ = c.ensureCacheDirs()
	_ = writeJSONFile(c.paths.StatsPath, c.stats)
	if c.previous != nil {
		_ = writeJSONFile(c.paths.SessionStatePath, c.previous)
	}
}

func (c *PromptCache) ensureCacheDirs() error {
	return os.MkdirAll(c.paths.CompletionDir, 0o755)
}

type completionCacheEntry struct {
	CachedAtUnixSecs   uint64        `json:"cached_at_unix_secs"`
	FingerprintVersion uint32        `json:"fingerprint_version"`
	Response           CacheResponse `json:"response"`
}

func applyUsageToStats(stats *PromptCacheStats, usage *CacheUsage, requestHash, source string) {
	stats.TotalCacheCreationInputTokens += uint64(usage.CacheCreationInputTokens)
	stats.TotalCacheReadInputTokens += uint64(usage.CacheReadInputTokens)
	creation := usage.CacheCreationInputTokens
	read := usage.CacheReadInputTokens
	stats.LastCacheCreationInputTokens = &creation
	stats.LastCacheReadInputTokens = &read
	stats.LastRequestHash = requestHash
	stats.LastCacheSource = source
}

func nowUnixSecs() uint64 {
	return uint64(time.Now().Unix())
}

func writeJSONFile(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	// Atomic write: tmp + rename
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readJSONFile[T any](path string) T {
	var zero T
	data, err := os.ReadFile(path)
	if err != nil {
		return zero
	}
	var result T
	if err := json.Unmarshal(data, &result); err != nil {
		return zero
	}
	return result
}

func readJSONFilePtr[T any](path string) *T {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var result T
	if err := json.Unmarshal(data, &result); err != nil {
		return nil
	}
	return &result
}

func detectCacheBreak(config *PromptCacheConfig, previous, current *trackedPromptState) *CacheBreakEvent {
	if previous == nil {
		return nil
	}

	if previous.FingerprintVersion != current.FingerprintVersion {
		tokenDrop := uint32(0)
		if previous.CacheReadInputTokens > current.CacheReadInputTokens {
			tokenDrop = previous.CacheReadInputTokens - current.CacheReadInputTokens
		}
		return &CacheBreakEvent{
			Unexpected:                   false,
			Reason:                       fmt.Sprintf("fingerprint version changed (v%d -> v%d)", previous.FingerprintVersion, current.FingerprintVersion),
			PreviousCacheReadInputTokens: previous.CacheReadInputTokens,
			CurrentCacheReadInputTokens:  current.CacheReadInputTokens,
			TokenDrop:                    tokenDrop,
		}
	}

	tokenDrop := uint32(0)
	if previous.CacheReadInputTokens > current.CacheReadInputTokens {
		tokenDrop = previous.CacheReadInputTokens - current.CacheReadInputTokens
	}

	if tokenDrop < config.CacheBreakMinDrop {
		return nil
	}

	var reasons []string
	if previous.ModelHash != current.ModelHash {
		reasons = append(reasons, "model changed")
	}
	if previous.SystemHash != current.SystemHash {
		reasons = append(reasons, "system prompt changed")
	}
	if previous.ToolsHash != current.ToolsHash {
		reasons = append(reasons, "tool definitions changed")
	}
	if previous.MessagesHash != current.MessagesHash {
		reasons = append(reasons, "message payload changed")
	}

	elapsed := current.ObservedAtUnixSecs - previous.ObservedAtUnixSecs
	if previous.ObservedAtUnixSecs > current.ObservedAtUnixSecs {
		elapsed = 0
	}

	var unexpected bool
	var reason string
	if len(reasons) == 0 {
		if elapsed > uint64(config.PromptTTL.Seconds()) {
			unexpected = false
			reason = fmt.Sprintf("possible prompt cache TTL expiry after %ds", elapsed)
		} else {
			unexpected = true
			reason = "cache read tokens dropped while prompt fingerprint remained stable"
		}
	} else {
		unexpected = false
		reason = joinStrings(reasons, ", ")
	}

	return &CacheBreakEvent{
		Unexpected:                   unexpected,
		Reason:                       reason,
		PreviousCacheReadInputTokens: previous.CacheReadInputTokens,
		CurrentCacheReadInputTokens:  current.CacheReadInputTokens,
		TokenDrop:                    tokenDrop,
	}
}

func joinStrings(parts []string, sep string) string {
	result := ""
	for i, p := range parts {
		if i > 0 {
			result += sep
		}
		result += p
	}
	return result
}

// PromptCachePaths holds the directory layout for a session's cache.
type PromptCachePaths struct {
	Root             string
	SessionDir       string
	CompletionDir    string
	SessionStatePath string
	StatsPath        string
}

// PromptCachePathsForSession constructs cache paths for a session.
func PromptCachePathsForSession(sessionID string) PromptCachePaths {
	root := baseCacheRoot()
	sessionDir := filepath.Join(root, SanitizePathSegment(sessionID))
	completionDir := filepath.Join(sessionDir, "completions")
	return PromptCachePaths{
		Root:             root,
		SessionDir:       sessionDir,
		CompletionDir:    completionDir,
		SessionStatePath: filepath.Join(sessionDir, "session-state.json"),
		StatsPath:        filepath.Join(sessionDir, "stats.json"),
	}
}

// CompletionEntryPath returns the file path for a cached completion.
func (p PromptCachePaths) CompletionEntryPath(requestHash string) string {
	return filepath.Join(p.CompletionDir, requestHash+".json")
}

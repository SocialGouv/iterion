package apikit

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// LiveCacheTTL is how long a fetched model snapshot is considered fresh.
const LiveCacheTTL = 24 * time.Hour

// liveCacheFilename is the on-disk cache file (relative to the cache dir).
const liveCacheFilename = "models-cache.json"

// LiveCacheEntry mirrors a normalized model record from the unified model
// sources (OpenRouter, models.dev). Only fields used by claw are retained.
type LiveCacheEntry struct {
	Canonical     string   `json:"canonical"`
	Provider      string   `json:"provider"`
	ContextWindow uint32   `json:"context_window"`
	MaxOutput     uint32   `json:"max_output"`
	Aliases       []string `json:"aliases,omitempty"`
}

// LiveCache is the persisted snapshot of model data fetched from upstream
// sources. The struct is JSON-serialised to disk.
type LiveCache struct {
	Entries   []LiveCacheEntry `json:"entries"`
	FetchedAt time.Time        `json:"fetched_at"`
	Source    string           `json:"source"`
}

var (
	liveFetchOnce sync.Once
	liveHTTP      = &http.Client{Timeout: 5 * time.Second}
)

// resetLiveFetchOnce is a test hook that resets the once-guard so the
// MaybeRefreshLive path can be exercised multiple times in unit tests.
// Not exported.
func resetLiveFetchOnce() {
	liveFetchOnce = sync.Once{}
}

// liveCachePath returns the absolute path to the live cache file. It honours
// $XDG_CACHE_HOME when set, falls back to ~/.cache, and finally os.TempDir.
func liveCachePath() (string, error) {
	if os.Getenv("CLAW_DISABLE_LIVE_REGISTRY") == "1" {
		return "", fmt.Errorf("live registry disabled via CLAW_DISABLE_LIVE_REGISTRY")
	}
	base := os.Getenv("XDG_CACHE_HOME")
	if base == "" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			base = filepath.Join(home, ".cache")
		}
	}
	if base == "" {
		base = os.TempDir()
	}
	dir := filepath.Join(base, "claw-code-go")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return filepath.Join(dir, liveCacheFilename), nil
}

// loadLiveCache reads the on-disk cache, if any. Returns (nil, nil) when the
// file is missing — that's not an error, just a cache miss.
func loadLiveCache() (*LiveCache, error) {
	path, err := liveCachePath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var cache LiveCache
	if err := json.Unmarshal(data, &cache); err != nil {
		return nil, fmt.Errorf("parse live cache: %w", err)
	}
	return &cache, nil
}

// saveLiveCache writes the cache atomically (temp file + rename) so a crash
// mid-write never leaves a half-written file behind.
func saveLiveCache(cache *LiveCache) error {
	path, err := liveCachePath()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// mergeLiveIntoRegistry applies live entries on top of the embed registry.
// Existing canonical names get their token limits updated; unknown canonical
// names are inserted as new ModelEntry. The provider metadata is preserved
// when the canonical already exists; for new entries, metadata is resolved
// via MetadataForModel using the canonical name.
//
// Caller must NOT hold reg.mu — this function takes the write lock.
func mergeLiveIntoRegistry(reg *ModelRegistry, cache *LiveCache) {
	if reg == nil || cache == nil || len(cache.Entries) == 0 {
		return
	}
	reg.ensureInit()
	reg.mu.Lock()
	defer reg.mu.Unlock()

	for _, e := range cache.Entries {
		canonical := strings.TrimSpace(e.Canonical)
		if canonical == "" {
			continue
		}
		existing, ok := reg.models[canonical]
		if ok {
			// Update non-zero fields only — never zero out values that were
			// curated in the embed registry but are missing from live data.
			if e.ContextWindow > 0 {
				existing.ContextWindow = e.ContextWindow
			}
			if e.MaxOutput > 0 {
				existing.MaxOutput = e.MaxOutput
			}
			// Add new aliases without removing existing ones.
			for _, alias := range e.Aliases {
				lower := strings.ToLower(alias)
				if _, exists := reg.aliases[lower]; !exists {
					reg.aliases[lower] = canonical
				}
			}
			continue
		}
		// New entry. Resolve metadata via the package-level helper (uses
		// prefix matching). It's safe to call here because MetadataForModel
		// uses ResolveAlias which acquires its own lock — but we currently
		// hold the write lock. Use a tiny inline prefix detection to avoid
		// re-entering ResolveAlias.
		meta := metadataForCanonicalUnlocked(canonical, e.Provider)
		entry := &ModelEntry{
			Canonical:     canonical,
			Provider:      providerFromString(e.Provider),
			MaxOutput:     e.MaxOutput,
			ContextWindow: e.ContextWindow,
			Aliases:       append([]string{}, e.Aliases...),
			Metadata:      meta,
		}
		reg.models[canonical] = entry
		reg.aliases[strings.ToLower(canonical)] = canonical
		for _, alias := range e.Aliases {
			lower := strings.ToLower(alias)
			if _, exists := reg.aliases[lower]; !exists {
				reg.aliases[lower] = canonical
			}
		}
	}
}

// metadataForCanonicalUnlocked picks the right ProviderMetadata pointer for
// a canonical model id without going through MetadataForModel (which would
// re-enter the registry mutex). Falls back to nil when the provider is
// unknown.
func metadataForCanonicalUnlocked(canonical, providerHint string) *ProviderMetadata {
	switch providerFromString(providerHint) {
	case ProviderAnthropic:
		return anthropicMeta
	case ProviderOpenAI:
		return openaiMeta
	case ProviderXai:
		return xaiMeta
	case ProviderDashScope:
		return dashScopeMeta
	}
	lower := strings.ToLower(canonical)
	switch {
	case strings.HasPrefix(lower, "claude"):
		return anthropicMeta
	case strings.HasPrefix(lower, "gpt-") || strings.HasPrefix(lower, "openai/"):
		return openaiMeta
	case strings.HasPrefix(lower, "grok"):
		return xaiMeta
	case strings.HasPrefix(lower, "qwen"):
		return dashScopeMeta
	}
	return nil
}

func providerFromString(p string) ProviderKind {
	switch strings.ToLower(strings.TrimSpace(p)) {
	case "anthropic":
		return ProviderAnthropic
	case "openai":
		return ProviderOpenAI
	case "xai", "x-ai":
		return ProviderXai
	case "dashscope", "qwen":
		return ProviderDashScope
	}
	return ""
}

// MaybeRefreshLive triggers an async fetch of upstream model data when no
// cache exists or when the cache is older than LiveCacheTTL. It is the boot
// hook used by DefaultModelRegistry — callers should not block on it.
//
// When CLAW_DISABLE_LIVE_REGISTRY=1, this is a no-op.
func MaybeRefreshLive(reg *ModelRegistry) {
	if os.Getenv("CLAW_DISABLE_LIVE_REGISTRY") == "1" {
		return
	}
	liveFetchOnce.Do(func() {
		go func() {
			cache, _ := loadLiveCache()
			fresh := cache != nil && time.Since(cache.FetchedAt) < LiveCacheTTL
			if cache != nil {
				mergeLiveIntoRegistry(reg, cache)
			}
			if fresh {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if newCache, err := fetchLive(ctx); err == nil && newCache != nil {
				_ = saveLiveCache(newCache)
				mergeLiveIntoRegistry(reg, newCache)
			}
		}()
	})
}

// fetchLive hits OpenRouter and models.dev, normalises the responses, merges
// the two by canonical name, and returns the resulting LiveCache. Returns
// (nil, err) when both sources fail; when only one fails, the result reflects
// only the successful source.
func fetchLive(ctx context.Context) (*LiveCache, error) {
	merged := map[string]LiveCacheEntry{}

	if entries, err := fetchModelsDev(ctx); err == nil {
		for _, e := range entries {
			merged[e.Canonical] = e
		}
	}
	if entries, err := fetchOpenRouter(ctx); err == nil {
		for _, e := range entries {
			if existing, ok := merged[e.Canonical]; ok {
				// Take the MAX of context windows when sources disagree —
				// under-declaring is more dangerous than over-declaring for
				// a compaction threshold (sub-cap means a compaction fires
				// too early, never fires past the real limit).
				if e.ContextWindow > existing.ContextWindow {
					existing.ContextWindow = e.ContextWindow
				}
				if e.MaxOutput > existing.MaxOutput {
					existing.MaxOutput = e.MaxOutput
				}
				for _, a := range e.Aliases {
					if !containsString(existing.Aliases, a) {
						existing.Aliases = append(existing.Aliases, a)
					}
				}
				merged[e.Canonical] = existing
			} else {
				merged[e.Canonical] = e
			}
		}
	}

	if len(merged) == 0 {
		return nil, fmt.Errorf("no model data fetched")
	}

	out := &LiveCache{
		FetchedAt: time.Now().UTC(),
		Source:    "openrouter+models.dev",
		Entries:   make([]LiveCacheEntry, 0, len(merged)),
	}
	for _, e := range merged {
		out.Entries = append(out.Entries, e)
	}
	return out, nil
}

func containsString(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// OpenRouter normalisation
// ---------------------------------------------------------------------------

type openRouterResponse struct {
	Data []openRouterModel `json:"data"`
}

type openRouterModel struct {
	ID            string `json:"id"`
	ContextLength uint32 `json:"context_length"`
	TopProvider   struct {
		MaxCompletionTokens uint32 `json:"max_completion_tokens"`
	} `json:"top_provider"`
}

func fetchOpenRouter(ctx context.Context) ([]LiveCacheEntry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://openrouter.ai/api/v1/models", nil)
	if err != nil {
		return nil, err
	}
	resp, err := liveHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openrouter status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var parsed openRouterResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, err
	}
	out := make([]LiveCacheEntry, 0, len(parsed.Data))
	for _, m := range parsed.Data {
		canonical, provider := canonicalFromOpenRouterID(m.ID)
		if canonical == "" {
			continue
		}
		entry := LiveCacheEntry{
			Canonical:     canonical,
			Provider:      provider,
			ContextWindow: m.ContextLength,
			MaxOutput:     m.TopProvider.MaxCompletionTokens,
		}
		// The OpenRouter id (e.g. "openai/gpt-5.5") is kept as alias.
		if m.ID != canonical {
			entry.Aliases = []string{m.ID}
		}
		out = append(out, entry)
	}
	return out, nil
}

// canonicalFromOpenRouterID strips the provider prefix from "anthropic/foo"
// and normalises Anthropic-style canonical names to the dot-free form used
// by the embed registry (e.g. "claude-opus-4.7" → "claude-opus-4-7").
// Returns empty string when the id should be skipped.
func canonicalFromOpenRouterID(id string) (canonical, provider string) {
	parts := strings.SplitN(id, "/", 2)
	if len(parts) != 2 {
		return strings.ReplaceAll(id, ".", "-"), ""
	}
	provider = parts[0]
	tail := parts[1]
	switch provider {
	case "anthropic":
		// Anthropic SDK uses "claude-opus-4-7" while OpenRouter exposes
		// "claude-opus-4.7"; normalise to the SDK form.
		return strings.ReplaceAll(tail, ".", "-"), "anthropic"
	case "openai":
		// Keep dots — OpenAI ids like "gpt-5.5" use them natively.
		return tail, "openai"
	case "x-ai":
		return strings.ReplaceAll(tail, ".", "-"), "xai"
	default:
		return tail, provider
	}
}

// ---------------------------------------------------------------------------
// models.dev normalisation
// ---------------------------------------------------------------------------

type modelsDevResponse map[string]modelsDevProvider // keyed by provider id

type modelsDevProvider struct {
	ID     string                       `json:"id"`
	Models map[string]modelsDevModelDoc `json:"models"`
}

type modelsDevModelDoc struct {
	ID    string `json:"id"`
	Limit struct {
		Context uint32 `json:"context"`
		Output  uint32 `json:"output"`
	} `json:"limit"`
}

func fetchModelsDev(ctx context.Context) ([]LiveCacheEntry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://models.dev/api.json", nil)
	if err != nil {
		return nil, err
	}
	resp, err := liveHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models.dev status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var parsed modelsDevResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, err
	}
	out := []LiveCacheEntry{}
	for providerID, prov := range parsed {
		// Only emit entries for providers we actually route. Other
		// providers' entries would just be noise (no API path to use them).
		var providerHint string
		switch providerID {
		case "anthropic":
			providerHint = "anthropic"
		case "openai":
			providerHint = "openai"
		case "x-ai", "xai":
			providerHint = "xai"
		case "dashscope", "qwen":
			providerHint = "dashscope"
		default:
			continue
		}
		for _, m := range prov.Models {
			if m.Limit.Context == 0 {
				continue
			}
			canonical := m.ID
			out = append(out, LiveCacheEntry{
				Canonical:     canonical,
				Provider:      providerHint,
				ContextWindow: m.Limit.Context,
				MaxOutput:     m.Limit.Output,
			})
		}
	}
	return out, nil
}

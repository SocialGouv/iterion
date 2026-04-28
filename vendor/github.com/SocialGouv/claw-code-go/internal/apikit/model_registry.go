package apikit

import (
	"strings"
	"sync"
)

// ModelEntry stores metadata for a registered model.
type ModelEntry struct {
	Canonical     string       // canonical model name (e.g., "claude-opus-4-6")
	Provider      ProviderKind // provider for this model
	MaxOutput     uint32       // max output tokens
	ContextWindow uint32       // context window tokens
	Aliases       []string     // short aliases (e.g., "opus")
	Metadata      *ProviderMetadata
}

// ModelRegistry is a dynamic registry of known models.
// It replaces the hardcoded switches in preflight.go and provider_routing.go
// with a map-based lookup that supports runtime extension.
//
// Thread-safe: uses sync.RWMutex so that LookupModel/ResolveAlias take a
// read lock while RegisterModel takes a write lock. This avoids the race
// condition that existed with the previous sync.Once approach where
// RegisterModel wrote to maps without holding any lock after initialization.
type ModelRegistry struct {
	mu      sync.RWMutex
	init    bool
	models  map[string]*ModelEntry // keyed by canonical name
	aliases map[string]string      // alias → canonical name
}

// defaultRegistry is the package-level singleton.
var defaultRegistry = &ModelRegistry{}

// Shared provider metadata — one instance per provider, referenced by all
// models belonging to that provider.
var (
	anthropicMeta = &ProviderMetadata{
		Provider:       ProviderAnthropic,
		AuthEnvVar:     "ANTHROPIC_API_KEY",
		BaseURLEnvVar:  "ANTHROPIC_BASE_URL",
		DefaultBaseURL: "https://api.anthropic.com",
	}
	xaiMeta = &ProviderMetadata{
		Provider:       ProviderXai,
		AuthEnvVar:     "XAI_API_KEY",
		BaseURLEnvVar:  "XAI_BASE_URL",
		DefaultBaseURL: "https://api.x.ai/v1",
	}
	dashScopeMeta = &ProviderMetadata{
		Provider:       ProviderDashScope,
		AuthEnvVar:     "DASHSCOPE_API_KEY",
		BaseURLEnvVar:  "DASHSCOPE_BASE_URL",
		DefaultBaseURL: "https://dashscope.aliyuncs.com/compatible-mode/v1",
	}
)

// ensureInit populates the registry with built-in models on first access.
// Caller must NOT hold r.mu — this method acquires a write lock internally.
func (r *ModelRegistry) ensureInit() {
	r.mu.RLock()
	if r.init {
		r.mu.RUnlock()
		return
	}
	r.mu.RUnlock()

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.init {
		return // double-check after acquiring write lock
	}

	r.models = make(map[string]*ModelEntry)
	r.aliases = make(map[string]string)

	entries := []ModelEntry{
		{Canonical: "claude-opus-4-6", Provider: ProviderAnthropic, MaxOutput: 32_000, ContextWindow: 200_000, Aliases: []string{"opus"}, Metadata: anthropicMeta},
		{Canonical: "claude-sonnet-4-6", Provider: ProviderAnthropic, MaxOutput: 64_000, ContextWindow: 200_000, Aliases: []string{"sonnet"}, Metadata: anthropicMeta},
		{Canonical: "claude-haiku-4-5-20251213", Provider: ProviderAnthropic, MaxOutput: 64_000, ContextWindow: 200_000, Aliases: []string{"haiku"}, Metadata: anthropicMeta},
		{Canonical: "grok-3", Provider: ProviderXai, MaxOutput: 64_000, ContextWindow: 131_072, Aliases: []string{"grok"}, Metadata: xaiMeta},
		{Canonical: "grok-3-mini", Provider: ProviderXai, MaxOutput: 64_000, ContextWindow: 131_072, Aliases: []string{"grok-mini"}, Metadata: xaiMeta},
		{Canonical: "grok-2", Provider: ProviderXai, Metadata: xaiMeta},
		{Canonical: "qwen-max", Provider: ProviderDashScope, Aliases: []string{"qwen"}, Metadata: dashScopeMeta},
		{Canonical: "qwen-plus", Provider: ProviderDashScope, Metadata: dashScopeMeta},
		{Canonical: "qwen-turbo", Provider: ProviderDashScope, Metadata: dashScopeMeta},
		{Canonical: "qwen-qwq-32b", Provider: ProviderDashScope, Metadata: dashScopeMeta},
	}

	for i := range entries {
		entry := &entries[i]
		r.models[entry.Canonical] = entry
		for _, alias := range entry.Aliases {
			r.aliases[strings.ToLower(alias)] = entry.Canonical
		}
		r.aliases[strings.ToLower(entry.Canonical)] = entry.Canonical
	}
	r.init = true
}

// LookupModel returns the model entry for a given name or alias.
// Returns nil if the model is not found in the registry.
func (r *ModelRegistry) LookupModel(nameOrAlias string) *ModelEntry {
	r.ensureInit()
	lower := strings.ToLower(strings.TrimSpace(nameOrAlias))
	r.mu.RLock()
	defer r.mu.RUnlock()
	if canonical, ok := r.aliases[lower]; ok {
		return r.models[canonical]
	}
	return nil
}

// ResolveAlias resolves a model alias to its canonical name.
// Returns the input unchanged if no alias match is found.
func (r *ModelRegistry) ResolveAlias(nameOrAlias string) string {
	r.ensureInit()
	lower := strings.ToLower(strings.TrimSpace(nameOrAlias))
	r.mu.RLock()
	defer r.mu.RUnlock()
	if canonical, ok := r.aliases[lower]; ok {
		return canonical
	}
	return strings.TrimSpace(nameOrAlias)
}

// RegisterModel adds or replaces a model entry in the registry.
// This is intended for runtime extension (e.g., plugin-provided models).
// Thread-safe: acquires a write lock to mutate the maps.
func (r *ModelRegistry) RegisterModel(entry ModelEntry) {
	r.ensureInit()
	r.mu.Lock()
	defer r.mu.Unlock()
	e := entry // copy for pointer stability
	r.models[e.Canonical] = &e
	for _, alias := range e.Aliases {
		r.aliases[strings.ToLower(alias)] = e.Canonical
	}
	r.aliases[strings.ToLower(e.Canonical)] = e.Canonical
}

// DefaultModelRegistry returns the package-level singleton registry.
func DefaultModelRegistry() *ModelRegistry {
	defaultRegistry.ensureInit()
	return defaultRegistry
}

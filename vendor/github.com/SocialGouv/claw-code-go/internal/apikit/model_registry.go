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

	// SupportedReasoningEfforts lists the reasoning_effort levels accepted
	// by this model, ordered from lowest to highest. Nil means the model
	// does not support reasoning_effort at all (callers should omit the
	// parameter). Curated from provider documentation:
	//   - Anthropic: platform.claude.com/docs/en/build-with-claude/effort
	//   - OpenAI:    platform.openai.com/docs/guides/reasoning
	SupportedReasoningEfforts []string

	// DefaultReasoningEffort is the level the API uses when the parameter
	// is omitted. Empty string means "no API default" (the provider
	// behaves as if reasoning is off).
	DefaultReasoningEffort string
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
	openaiMeta = &ProviderMetadata{
		Provider:       ProviderOpenAI,
		AuthEnvVar:     "OPENAI_API_KEY",
		BaseURLEnvVar:  "OPENAI_BASE_URL",
		DefaultBaseURL: "https://api.openai.com/v1",
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

	// Effort level matrices, curated from provider docs (May 2026).
	// Anthropic source: platform.claude.com/docs/en/build-with-claude/effort
	// OpenAI source:    platform.openai.com/docs/guides/reasoning
	anthropicOpus47Effort := []string{"low", "medium", "high", "xhigh", "max"}
	anthropicOpus46Effort := []string{"low", "medium", "high", "max"}
	anthropicSonnet46Effort := []string{"low", "medium", "high", "max"}
	openaiReasoningEffort := []string{"minimal", "low", "medium", "high"}

	entries := []ModelEntry{
		// Anthropic — context windows from models.dev / OpenRouter (1M for current SDKs).
		{Canonical: "claude-opus-4-7", Provider: ProviderAnthropic, MaxOutput: 128_000, ContextWindow: 1_000_000, Aliases: []string{"opus", "opus-4-7"}, Metadata: anthropicMeta,
			SupportedReasoningEfforts: anthropicOpus47Effort, DefaultReasoningEffort: "xhigh"},
		{Canonical: "claude-opus-4-6", Provider: ProviderAnthropic, MaxOutput: 128_000, ContextWindow: 1_000_000, Aliases: []string{"opus-4-6"}, Metadata: anthropicMeta,
			SupportedReasoningEfforts: anthropicOpus46Effort, DefaultReasoningEffort: "high"},
		// Sonnet 4.7 effort matrix not yet documented by Anthropic — leaving nil
		// rather than guessing. Update once code.claude.com/docs/en/model-config
		// publishes the table.
		{Canonical: "claude-sonnet-4-7", Provider: ProviderAnthropic, MaxOutput: 128_000, ContextWindow: 1_000_000, Aliases: []string{"sonnet", "sonnet-4-7"}, Metadata: anthropicMeta},
		{Canonical: "claude-sonnet-4-6", Provider: ProviderAnthropic, MaxOutput: 64_000, ContextWindow: 1_000_000, Aliases: []string{"sonnet-4-6"}, Metadata: anthropicMeta,
			SupportedReasoningEfforts: anthropicSonnet46Effort, DefaultReasoningEffort: "high"},
		// Haiku does not support effort — the docs only list Opus and Sonnet.
		{Canonical: "claude-haiku-4-5", Provider: ProviderAnthropic, MaxOutput: 64_000, ContextWindow: 200_000, Aliases: []string{"haiku", "claude-haiku-4-5-20251213"}, Metadata: anthropicMeta},
		// OpenAI — gpt-5.x family (1M+ context). Reasoning effort is
		// supported on the gpt-5 reasoning models with the standard
		// minimal/low/medium/high enum. OpenAI's Responses API
		// documents `medium` as the implicit default when the
		// reasoning_effort parameter is omitted.
		{Canonical: "gpt-5.5", Provider: ProviderOpenAI, MaxOutput: 128_000, ContextWindow: 1_050_000, Aliases: []string{"openai/gpt-5.5"}, Metadata: openaiMeta,
			SupportedReasoningEfforts: openaiReasoningEffort, DefaultReasoningEffort: "medium"},
		{Canonical: "gpt-5.5-pro", Provider: ProviderOpenAI, MaxOutput: 128_000, ContextWindow: 1_050_000, Aliases: []string{"openai/gpt-5.5-pro"}, Metadata: openaiMeta,
			SupportedReasoningEfforts: openaiReasoningEffort, DefaultReasoningEffort: "medium"},
		{Canonical: "gpt-5.4", Provider: ProviderOpenAI, MaxOutput: 128_000, ContextWindow: 1_050_000, Aliases: []string{"openai/gpt-5.4"}, Metadata: openaiMeta,
			SupportedReasoningEfforts: openaiReasoningEffort, DefaultReasoningEffort: "medium"},
		{Canonical: "gpt-5.4-pro", Provider: ProviderOpenAI, MaxOutput: 128_000, ContextWindow: 1_050_000, Aliases: []string{"openai/gpt-5.4-pro"}, Metadata: openaiMeta,
			SupportedReasoningEfforts: openaiReasoningEffort, DefaultReasoningEffort: "medium"},
		{Canonical: "gpt-5.4-mini", Provider: ProviderOpenAI, MaxOutput: 128_000, ContextWindow: 400_000, Aliases: []string{"openai/gpt-5.4-mini"}, Metadata: openaiMeta,
			SupportedReasoningEfforts: openaiReasoningEffort, DefaultReasoningEffort: "medium"},
		{Canonical: "gpt-5.4-nano", Provider: ProviderOpenAI, MaxOutput: 128_000, ContextWindow: 400_000, Aliases: []string{"openai/gpt-5.4-nano"}, Metadata: openaiMeta,
			SupportedReasoningEfforts: openaiReasoningEffort, DefaultReasoningEffort: "medium"},
		// xAI / DashScope — no documented effort matrix yet, leaving nil.
		{Canonical: "grok-3", Provider: ProviderXai, MaxOutput: 64_000, ContextWindow: 131_072, Aliases: []string{"grok"}, Metadata: xaiMeta},
		{Canonical: "grok-3-mini", Provider: ProviderXai, MaxOutput: 64_000, ContextWindow: 131_072, Aliases: []string{"grok-mini"}, Metadata: xaiMeta},
		{Canonical: "grok-2", Provider: ProviderXai, MaxOutput: 64_000, ContextWindow: 131_072, Metadata: xaiMeta},
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
//
// On first access, the embed entries are loaded synchronously and the live
// cache (if present and recent) is merged on top. A background fetch of the
// upstream model sources is then scheduled — the current call does not wait
// for it. See MaybeRefreshLive for details and the CLAW_DISABLE_LIVE_REGISTRY
// env var to opt out.
func DefaultModelRegistry() *ModelRegistry {
	defaultRegistry.ensureInit()
	MaybeRefreshLive(defaultRegistry)
	return defaultRegistry
}

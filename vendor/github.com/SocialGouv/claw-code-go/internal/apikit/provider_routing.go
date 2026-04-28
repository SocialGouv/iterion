package apikit

import (
	"os"
	"strings"
)

// ProviderKind identifies an API provider.
type ProviderKind string

const (
	ProviderAnthropic ProviderKind = "anthropic"
	ProviderXai       ProviderKind = "xai"
	ProviderOpenAI    ProviderKind = "openai"
	ProviderDashScope ProviderKind = "dashscope"
)

// ProviderMetadata contains routing info for a provider.
type ProviderMetadata struct {
	Provider       ProviderKind
	AuthEnvVar     string // e.g., "ANTHROPIC_API_KEY"
	BaseURLEnvVar  string // e.g., "ANTHROPIC_BASE_URL"
	DefaultBaseURL string
}

// MetadataForModel returns provider routing metadata for a model.
// It first checks the ModelRegistry for an exact match (supports runtime-registered
// models), then falls back to prefix-based detection for Go-only providers like
// Bedrock/Vertex/Foundry that route qwen/openai models via prefix.
// Returns nil if neither lookup matches.
func MetadataForModel(model string) *ProviderMetadata {
	canonical := ResolveModelAlias(model)

	// 1. Registry lookup — handles all registered models (built-in + runtime).
	if entry := DefaultModelRegistry().LookupModel(canonical); entry != nil && entry.Metadata != nil {
		return entry.Metadata
	}

	// 2. Prefix-based fallback — important for models not in the registry
	//    (e.g., Bedrock/Vertex/Foundry routing for qwen/openai prefixed models).
	lower := strings.ToLower(canonical)
	switch {
	case strings.HasPrefix(lower, "claude"):
		return &ProviderMetadata{
			Provider:       ProviderAnthropic,
			AuthEnvVar:     "ANTHROPIC_API_KEY",
			BaseURLEnvVar:  "ANTHROPIC_BASE_URL",
			DefaultBaseURL: "https://api.anthropic.com",
		}
	case strings.HasPrefix(lower, "grok"):
		return &ProviderMetadata{
			Provider:       ProviderXai,
			AuthEnvVar:     "XAI_API_KEY",
			BaseURLEnvVar:  "XAI_BASE_URL",
			DefaultBaseURL: "https://api.x.ai/v1",
		}
	case strings.HasPrefix(lower, "openai/") || strings.HasPrefix(lower, "gpt-"):
		return &ProviderMetadata{
			Provider:       ProviderOpenAI,
			AuthEnvVar:     "OPENAI_API_KEY",
			BaseURLEnvVar:  "OPENAI_BASE_URL",
			DefaultBaseURL: "https://api.openai.com/v1",
		}
	case strings.HasPrefix(lower, "qwen/") || strings.HasPrefix(lower, "qwen-"):
		return &ProviderMetadata{
			Provider:       ProviderDashScope,
			AuthEnvVar:     "DASHSCOPE_API_KEY",
			BaseURLEnvVar:  "DASHSCOPE_BASE_URL",
			DefaultBaseURL: "https://dashscope.aliyuncs.com/compatible-mode/v1",
		}
	}
	return nil
}

// DetectProviderKind determines the provider for a model using multi-stage fallback:
//  1. Explicit prefix match via MetadataForModel
//  2. ANTHROPIC_API_KEY or ANTHROPIC_AUTH_TOKEN env vars → Anthropic
//  3. OPENAI_API_KEY env var → OpenAI
//  4. XAI_API_KEY env var → Xai
//  5. Default to Anthropic
func DetectProviderKind(model string) ProviderKind {
	if meta := MetadataForModel(model); meta != nil {
		return meta.Provider
	}
	if os.Getenv("ANTHROPIC_API_KEY") != "" || os.Getenv("ANTHROPIC_AUTH_TOKEN") != "" {
		return ProviderAnthropic
	}
	if os.Getenv("OPENAI_API_KEY") != "" {
		return ProviderOpenAI
	}
	if os.Getenv("XAI_API_KEY") != "" {
		return ProviderXai
	}
	if os.Getenv("DASHSCOPE_API_KEY") != "" {
		return ProviderDashScope
	}
	return ProviderAnthropic
}

// LookupModelTokenLimit returns token limits for a known model.
// This is an alias for ModelTokenLimitForModel for API consistency.
func LookupModelTokenLimit(model string) *ModelTokenLimit {
	return ModelTokenLimitForModel(model)
}

// MaxTokensForModel returns the max output tokens for a known model.
// Falls back to a heuristic: 32,000 for opus models, 64,000 for others.
// Matches Rust's max_tokens_for_model().
func MaxTokensForModel(model string) uint32 {
	if limit := ModelTokenLimitForModel(model); limit != nil {
		return limit.MaxOutputTokens
	}
	canonical := ResolveModelAlias(model)
	if strings.Contains(strings.ToLower(canonical), "opus") {
		return 32_000
	}
	return 64_000
}

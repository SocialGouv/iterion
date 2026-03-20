// Package provider defines the interfaces and types that AI providers implement.
//
// GoAI ships provider implementations in sub-packages (openai, anthropic, google, etc.).
// Each provider implements one or more of the model interfaces defined here.
package provider

import "context"

// LanguageModel generates text and tool calls from messages.
type LanguageModel interface {
	// ModelID returns the provider-specific model identifier (e.g. "gpt-4o", "claude-sonnet-4-20250514").
	ModelID() string

	// DoGenerate performs a non-streaming generation request.
	DoGenerate(ctx context.Context, params GenerateParams) (*GenerateResult, error)

	// DoStream performs a streaming generation request.
	DoStream(ctx context.Context, params GenerateParams) (*StreamResult, error)
}

// CapableModel is an optional interface that LanguageModel implementations
// can satisfy to declare their capabilities. Use ModelCapabilitiesOf to query.
type CapableModel interface {
	Capabilities() ModelCapabilities
}

// ModelCapabilitiesOf returns the model's capabilities if it implements CapableModel,
// or a zero-value ModelCapabilities otherwise.
func ModelCapabilitiesOf(m LanguageModel) ModelCapabilities {
	if c, ok := m.(CapableModel); ok {
		return c.Capabilities()
	}
	return ModelCapabilities{}
}

// EmbeddingModel generates vector embeddings from text.
type EmbeddingModel interface {
	// ModelID returns the provider-specific model identifier.
	ModelID() string

	// DoEmbed generates embeddings for the given values.
	DoEmbed(ctx context.Context, values []string, params EmbedParams) (*EmbedResult, error)

	// MaxValuesPerCall returns the maximum number of values that can be embedded in a single call.
	// Returns 0 if there is no limit.
	MaxValuesPerCall() int
}

// ImageModel generates images from text prompts.
type ImageModel interface {
	// ModelID returns the provider-specific model identifier.
	ModelID() string

	// DoGenerate generates images from the given parameters.
	DoGenerate(ctx context.Context, params ImageParams) (*ImageResult, error)
}

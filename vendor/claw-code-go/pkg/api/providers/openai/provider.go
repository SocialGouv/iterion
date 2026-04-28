// Package openai re-exports the internal OpenAI provider via type alias.
package openai

import (
	internal "claw-code-go/internal/api/providers/openai"
)

// Provider implements api.Provider for the OpenAI API.
type Provider = internal.Provider

// DefaultOpenAIModel is the default model for OpenAI requests.
const DefaultOpenAIModel = internal.DefaultOpenAIModel

// New creates a new OpenAI provider.
func New() *Provider {
	return internal.New()
}

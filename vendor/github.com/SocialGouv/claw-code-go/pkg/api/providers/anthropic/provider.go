// Package anthropic re-exports the internal Anthropic provider via type alias.
package anthropic

import (
	internal "github.com/SocialGouv/claw-code-go/internal/api/providers/anthropic"
)

// Provider implements api.Provider for the Anthropic direct API.
type Provider = internal.Provider

// New creates a new Anthropic provider.
func New() *Provider {
	return internal.New()
}

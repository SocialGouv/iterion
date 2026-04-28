// Package foundry re-exports the internal Azure AI Foundry provider via type alias.
package foundry

import (
	internal "claw-code-go/internal/api/providers/foundry"
)

// Provider implements api.Provider for Azure AI Foundry (stub).
type Provider = internal.Provider

// New creates a new Foundry provider.
func New() *Provider {
	return internal.New()
}

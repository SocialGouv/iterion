// Package vertex re-exports the internal Vertex AI provider via type alias.
package vertex

import (
	internal "claw-code-go/internal/api/providers/vertex"
)

// Provider implements api.Provider for Google Cloud Vertex AI (stub).
type Provider = internal.Provider

// New creates a new Vertex AI provider.
func New() *Provider {
	return internal.New()
}

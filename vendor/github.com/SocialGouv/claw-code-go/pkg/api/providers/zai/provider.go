// Package zai re-exports the internal z.ai provider via type alias.
package zai

import (
	internal "github.com/SocialGouv/claw-code-go/internal/api/providers/zai"
)

// Provider implements api.Provider for z.ai.
type Provider = internal.Provider

// New creates a new z.ai provider.
func New() *Provider {
	return internal.New()
}

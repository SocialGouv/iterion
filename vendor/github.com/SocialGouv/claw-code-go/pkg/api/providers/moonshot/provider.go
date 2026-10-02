// Package moonshot re-exports the internal Moonshot provider via type alias.
package moonshot

import (
	internal "github.com/SocialGouv/claw-code-go/internal/api/providers/moonshot"
)

// Provider implements api.Provider for Moonshot.
type Provider = internal.Provider

// New creates a new Moonshot provider.
func New() *Provider {
	return internal.New()
}

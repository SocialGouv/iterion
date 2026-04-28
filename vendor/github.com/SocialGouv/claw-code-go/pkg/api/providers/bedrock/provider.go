// Package bedrock re-exports the internal Bedrock provider via type alias.
package bedrock

import (
	internal "github.com/SocialGouv/claw-code-go/internal/api/providers/bedrock"
)

// Provider implements api.Provider for AWS Bedrock (stub).
type Provider = internal.Provider

// New creates a new Bedrock provider.
func New() *Provider {
	return internal.New()
}

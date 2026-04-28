// Package runtime re-exports selected provider factory functions from
// claw-code-go's internal runtime. Only the provider selection surface is
// promoted; the full ConversationLoop/Session/Config stays internal.
package runtime

import (
	"claw-code-go/pkg/api"

	internalrt "claw-code-go/internal/runtime"
)

// SelectProvider returns the Provider for the given name.
// Supported: "anthropic" (default), "openai", "xai", "dashscope", "bedrock", "vertex", "foundry".
func SelectProvider(name string) api.Provider {
	return internalrt.SelectProvider(name)
}

// ProviderConfig holds the fields needed to create a provider client.
// This avoids leaking the full internal Config struct.
type ProviderConfig struct {
	ProviderName string
	APIKey       string
	OAuthToken   string
	BaseURL      string
	Model        string
	MaxTokens    int
}

// NewProviderClient creates an API client for the named provider.
func NewProviderClient(cfg *ProviderConfig) (api.APIClient, error) {
	return internalrt.NewProviderClient(&internalrt.Config{
		ProviderName: cfg.ProviderName,
		APIKey:       cfg.APIKey,
		OAuthToken:   cfg.OAuthToken,
		BaseURL:      cfg.BaseURL,
		Model:        cfg.Model,
		MaxTokens:    cfg.MaxTokens,
	})
}

// NewNoAuthClient returns a placeholder APIClient that always returns a
// friendly "not authenticated" error.
func NewNoAuthClient() api.APIClient {
	return internalrt.NewNoAuthClient()
}

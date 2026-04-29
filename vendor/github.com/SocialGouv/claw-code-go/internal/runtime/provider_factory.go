package runtime

import (
	"context"
	"fmt"
	"github.com/SocialGouv/claw-code-go/internal/api"
	anthropicprovider "github.com/SocialGouv/claw-code-go/internal/api/providers/anthropic"
	bedrockprovider "github.com/SocialGouv/claw-code-go/internal/api/providers/bedrock"
	foundryprovider "github.com/SocialGouv/claw-code-go/internal/api/providers/foundry"
	openaiprovider "github.com/SocialGouv/claw-code-go/internal/api/providers/openai"
	vertexprovider "github.com/SocialGouv/claw-code-go/internal/api/providers/vertex"
)

// SelectProvider returns the Provider for the given name.
// Supported: "anthropic" (default), "openai", "xai", "dashscope", "bedrock", "vertex", "foundry".
func SelectProvider(name string) api.Provider {
	switch name {
	case "openai", "xai", "dashscope":
		return openaiprovider.New()
	case "bedrock":
		return bedrockprovider.New()
	case "vertex":
		return vertexprovider.New()
	case "foundry":
		return foundryprovider.New()
	default:
		return anthropicprovider.New()
	}
}

// NewProviderClient creates an API client for the provider named in cfg.ProviderName.
// Returns an error for stub providers that are not yet implemented.
func NewProviderClient(cfg *Config) (api.APIClient, error) {
	provider := SelectProvider(cfg.ProviderName)
	return provider.NewClient(api.ProviderConfig{
		APIKey:     cfg.APIKey,
		OAuthToken: cfg.OAuthToken,
		BaseURL:    cfg.BaseURL,
		Model:      cfg.Model,
		MaxTokens:  cfg.MaxTokens,
	})
}

// ----- NoAuthClient ----------------------------------------------------------

// NoAuthClient is a placeholder APIClient used when no credentials are configured.
// Every call returns a friendly error directing the user to /login.
type NoAuthClient struct{}

// NewNoAuthClient returns a NoAuthClient as an api.APIClient interface value.
func NewNoAuthClient() api.APIClient {
	return &NoAuthClient{}
}

func (c *NoAuthClient) StreamResponse(_ context.Context, _ api.CreateMessageRequest) (<-chan api.StreamEvent, error) {
	return nil, fmt.Errorf("not authenticated — type /login to connect to an AI provider")
}

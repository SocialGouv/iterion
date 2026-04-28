// Package anthropic implements the Anthropic direct API provider.
// Authentication: API key (x-api-key header) or OAuth 2.0 Bearer token.
package anthropic

import (
	"github.com/SocialGouv/claw-code-go/internal/api"
)

const defaultBaseURL = "https://api.anthropic.com"

// Provider implements api.Provider for the Anthropic direct API.
type Provider struct{}

// New returns a new Anthropic Provider.
func New() *Provider { return &Provider{} }

// Name returns the provider identifier.
func (p *Provider) Name() string { return "anthropic" }

// AuthMethod returns the default auth method; OAuth is also supported at runtime.
func (p *Provider) AuthMethod() api.AuthMethod { return api.AuthMethodAPIKey }

// NewClient creates an Anthropic HTTP client.
// cfg.OAuthToken takes precedence over cfg.APIKey when both are set.
func (p *Provider) NewClient(cfg api.ProviderConfig) (api.APIClient, error) {
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	client := api.NewClient(cfg.APIKey, cfg.Model)
	client.BaseURL = baseURL
	if cfg.OAuthToken != "" {
		client.OAuthToken = cfg.OAuthToken
	}
	return client, nil
}

// MapModelID converts a canonical Claude model ID to the Anthropic API format.
// For Anthropic direct, model IDs are used as-is.
func MapModelID(model string) string { return model }

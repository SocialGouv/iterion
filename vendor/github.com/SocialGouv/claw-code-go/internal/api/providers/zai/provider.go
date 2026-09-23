// Package zai implements the z.ai provider: GLM models reached through
// z.ai's Anthropic-compatible endpoint (the Coding Plan's /api/anthropic
// wire). The request shape is Anthropic's, the account key rides the
// x-api-key header, and the model IDs are z.ai's own (glm-5.3, …).
//
// Why a first-class provider instead of pointing ANTHROPIC_BASE_URL at z.ai:
// the redirect works but it is invisible — a spec like "zai/glm-5.3" failed
// with "unknown provider" in every registry that routes model prefixes, and
// the credential that pays (the z.ai account key) could not be selected for
// a route the registries believed was Anthropic. Naming the provider makes
// the route, the credential and the billing all explicit.
package zai

import (
	"github.com/SocialGouv/claw-code-go/internal/api"
)

// DefaultBaseURL is z.ai's Anthropic-compatible endpoint. The bigmodel.cn
// gateway answers on the same wire — callers override via cfg.BaseURL.
const DefaultBaseURL = "https://api.z.ai/api/anthropic"

// Provider implements api.Provider for z.ai.
type Provider struct{}

// New returns a new z.ai Provider.
func New() *Provider { return &Provider{} }

// Name returns the provider identifier used in model specs ("zai/<model>").
func (p *Provider) Name() string { return "zai" }

// AuthMethod returns the primary auth method: the account key, sent as
// x-api-key. z.ai's Anthropic-compatible endpoint keys on that header —
// an OAuth bearer is not its contract.
func (p *Provider) AuthMethod() api.AuthMethod { return api.AuthMethodAPIKey }

// NewClient creates the HTTP client. cfg.BaseURL overrides DefaultBaseURL;
// cfg.APIKey is the account key. The env fallback (ZAI_API_KEY) belongs to
// the caller, mirroring the anthropic provider's seam: the provider wires,
// the resolver decides where the key comes from.
func (p *Provider) NewClient(cfg api.ProviderConfig) (api.APIClient, error) {
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	client := api.NewClient(cfg.APIKey, cfg.Model)
	client.BaseURL = baseURL
	if cfg.OAuthToken != "" {
		client.OAuthToken = cfg.OAuthToken
	}
	client.UserAgent = cfg.UserAgent
	client.ExtraHeaders = cfg.ExtraHeaders
	return client, nil
}

// MapModelID converts a canonical model ID to z.ai's API format.
// Model IDs are used as-is.
func MapModelID(model string) string { return model }

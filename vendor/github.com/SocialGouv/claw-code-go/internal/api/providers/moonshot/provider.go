// Package moonshot implements the Moonshot provider: Kimi models reached
// through Moonshot's Anthropic-compatible endpoint. The request shape is
// Anthropic's, the account key rides the x-api-key header, and the model IDs
// are Moonshot's own (kimi-k2, …).
//
// Why a first-class provider instead of pointing ANTHROPIC_BASE_URL at
// Moonshot: the redirect works but it is invisible — a spec like
// "moonshot/kimi-k2" fails with "unknown provider" in every registry that
// routes model prefixes, and the credential that pays (the Moonshot account
// key) cannot be selected for a route the registries believe is Anthropic.
// Naming the provider makes the route, the credential and the billing all
// explicit.
package moonshot

import (
	"github.com/SocialGouv/claw-code-go/internal/api"
)

// DefaultBaseURL is Moonshot's Anthropic-compatible endpoint. The api.moonshot.cn
// gateway answers on the same wire — callers override via cfg.BaseURL.
const DefaultBaseURL = "https://api.moonshot.ai/anthropic"

// Provider implements api.Provider for Moonshot.
type Provider struct{}

// New returns a new Moonshot Provider.
func New() *Provider { return &Provider{} }

// Name returns the provider identifier used in model specs ("moonshot/<model>").
func (p *Provider) Name() string { return "moonshot" }

// AuthMethod returns the primary auth method: the account key, sent as
// x-api-key. Moonshot's Anthropic-compatible endpoint keys on that header —
// an OAuth bearer is not its contract.
func (p *Provider) AuthMethod() api.AuthMethod { return api.AuthMethodAPIKey }

// NewClient creates the HTTP client. cfg.BaseURL overrides DefaultBaseURL;
// cfg.APIKey is the account key. The env fallback (MOONSHOT_API_KEY) belongs
// to the caller, mirroring the anthropic provider's seam: the provider wires,
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

// MapModelID converts a canonical model ID to Moonshot's API format.
// Model IDs are used WHOLE, slash included.
//
// API trap, and the reason this is not a no-op by accident: Kimi aliases
// contain a slash of their own ("kimi-code/kimi-for-coding"). Every other
// prefix-routing site in the stack reads "<provider>/<model>", so the
// temptation is to strip up to the first "/" here too — that yields
// "kimi-for-coding", an alias Moonshot cannot resolve. The provider has
// already been selected by the time the id reaches this function; the id is
// the model's, not a route.
func MapModelID(model string) string { return model }

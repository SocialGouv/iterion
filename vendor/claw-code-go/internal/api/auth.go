package api

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// AuthSourceKind discriminates AuthSource variants.
type AuthSourceKind int

const (
	AuthSourceNone     AuthSourceKind = iota
	AuthSourceAPIKey                  // API key only
	AuthSourceBearer                  // Bearer token only
	AuthSourceCombined                // both API key and bearer token
)

// AuthSource holds authentication credentials for API requests.
type AuthSource struct {
	Kind        AuthSourceKind
	APIKey      string
	BearerToken string
}

// NoAuth returns an AuthSource with no credentials.
func NoAuth() AuthSource {
	return AuthSource{Kind: AuthSourceNone}
}

// APIKeyAuth returns an AuthSource using an API key.
func APIKeyAuth(key string) AuthSource {
	return AuthSource{Kind: AuthSourceAPIKey, APIKey: key}
}

// BearerAuth returns an AuthSource using a bearer token.
func BearerAuth(token string) AuthSource {
	return AuthSource{Kind: AuthSourceBearer, BearerToken: token}
}

// CombinedAuth returns an AuthSource using both API key and bearer token.
func CombinedAuth(apiKey, bearerToken string) AuthSource {
	return AuthSource{Kind: AuthSourceCombined, APIKey: apiKey, BearerToken: bearerToken}
}

// ResolveStartupAuth resolves auth from environment variables.
// Priority: ANTHROPIC_API_KEY + ANTHROPIC_AUTH_TOKEN (combined if both present),
// then ANTHROPIC_API_KEY alone, then ANTHROPIC_AUTH_TOKEN alone.
func ResolveStartupAuth() AuthSource {
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	bearerToken := os.Getenv("ANTHROPIC_AUTH_TOKEN")

	switch {
	case apiKey != "" && bearerToken != "":
		return CombinedAuth(apiKey, bearerToken)
	case apiKey != "":
		return APIKeyAuth(apiKey)
	case bearerToken != "":
		return BearerAuth(bearerToken)
	default:
		return NoAuth()
	}
}

// ApplyToRequest sets appropriate headers on an HTTP request.
func (a AuthSource) ApplyToRequest(req *http.Request) {
	switch a.Kind {
	case AuthSourceAPIKey:
		req.Header.Set("x-api-key", a.APIKey)
	case AuthSourceBearer:
		req.Header.Set("Authorization", "Bearer "+a.BearerToken)
	case AuthSourceCombined:
		req.Header.Set("x-api-key", a.APIKey)
		req.Header.Set("Authorization", "Bearer "+a.BearerToken)
	case AuthSourceNone:
		// no-op
	}
}

// HasAPIKey returns true if an API key is present.
func (a AuthSource) HasAPIKey() bool {
	return a.APIKey != ""
}

// HasBearerToken returns true if a bearer token is present.
func (a AuthSource) HasBearerToken() bool {
	return a.BearerToken != ""
}

// MaskedAuthorizationHeader returns a redacted representation of the auth header.
// Matches Rust's masked_authorization_header() for safe logging.
func (a AuthSource) MaskedAuthorizationHeader() string {
	if a.HasBearerToken() {
		return "Bearer [REDACTED]"
	}
	return "<absent>"
}

// ResolveStartupAuthWithOAuth resolves auth from environment variables, falling
// back to a saved OAuth token if available. The loadOAuth callback loads and
// optionally refreshes a persisted token set. Matches Rust's
// resolve_startup_auth_source() precedence chain.
func ResolveStartupAuthWithOAuth(loadOAuth func() (*OAuthTokenSet, error)) (AuthSource, error) {
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	bearerToken := os.Getenv("ANTHROPIC_AUTH_TOKEN")

	// Env vars take highest priority.
	if apiKey != "" && bearerToken != "" {
		return CombinedAuth(apiKey, bearerToken), nil
	}
	if apiKey != "" {
		return APIKeyAuth(apiKey), nil
	}
	if bearerToken != "" {
		return BearerAuth(bearerToken), nil
	}

	// Fallback to saved OAuth token.
	if loadOAuth != nil {
		token, err := loadOAuth()
		if err != nil {
			return NoAuth(), err
		}
		if token != nil && token.AccessToken != "" {
			return BearerAuth(token.AccessToken), nil
		}
	}

	return NoAuth(), nil
}

// OAuthTokenSet represents a saved OAuth token set for auth resolution.
type OAuthTokenSet struct {
	AccessToken  string   `json:"access_token"`
	RefreshToken string   `json:"refresh_token,omitempty"`
	ExpiresAt    *uint64  `json:"expires_at,omitempty"`
	Scopes       []string `json:"scopes,omitempty"`
}

// IsExpired returns true if the token has an expiration time that has passed.
func (t *OAuthTokenSet) IsExpired() bool {
	if t.ExpiresAt == nil {
		return false
	}
	return *t.ExpiresAt <= uint64(time.Now().Unix())
}

// skAntBearerHint matches Rust's SK_ANT_BEARER_HINT constant.
const skAntBearerHint = "sk-ant-* keys go in ANTHROPIC_API_KEY (x-api-key header), not ANTHROPIC_AUTH_TOKEN (Bearer header). Move your key to ANTHROPIC_API_KEY."

// EnrichBearerAuthError adds a helpful hint when an API key (sk-ant-*) is
// mistakenly used as a Bearer token on a 401 response.
//
// Matches Rust's enrich_bearer_auth_error() which:
//  1. Only enriches API errors (non-API errors pass through unchanged)
//  2. Only enriches 401 status codes
//  3. Only enriches pure Bearer auth (not Combined, since x-api-key is already sent)
//  4. Only enriches sk-ant-* prefixed tokens
//
// The statusCode parameter gates enrichment — only 401 triggers the hint.
func EnrichBearerAuthError(errMsg string, statusCode int, auth AuthSource) string {
	// Only enrich 401 errors.
	if statusCode != 401 {
		return errMsg
	}
	// Only enrich pure Bearer auth — if API key is also present (Combined),
	// the x-api-key header is already being sent and the 401 is from a
	// different cause; adding the hint would be misleading.
	if !auth.HasBearerToken() || auth.HasAPIKey() {
		return errMsg
	}
	if !strings.HasPrefix(auth.BearerToken, "sk-ant-") {
		return errMsg
	}
	if errMsg != "" {
		return fmt.Sprintf("%s — hint: %s", errMsg, skAntBearerHint)
	}
	return fmt.Sprintf("hint: %s", skAntBearerHint)
}

// ForeignProviderEnvVar describes a non-Anthropic provider's env var and
// routing hint, used to suggest model-prefix fixes when Anthropic auth fails.
// Matches Rust's FOREIGN_PROVIDER_ENV_VARS.
type ForeignProviderEnvVar struct {
	EnvVar       string
	ProviderName string
	Hint         string
}

// ForeignProviderEnvVars lists provider env vars to check when Anthropic
// credentials are missing. If one is set, the user likely intends to use a
// different provider and needs model-prefix routing.
var ForeignProviderEnvVars = []ForeignProviderEnvVar{
	{
		EnvVar:       "OPENAI_API_KEY",
		ProviderName: "OpenAI-compat",
		Hint:         "prefix your model name with `openai/` (e.g. `--model openai/gpt-4.1-mini`) so prefix routing selects the OpenAI-compatible provider, and set `OPENAI_BASE_URL` if you are pointing at OpenRouter/Ollama/a local server",
	},
	{
		EnvVar:       "XAI_API_KEY",
		ProviderName: "xAI",
		Hint:         "use an xAI model alias (e.g. `--model grok` or `--model grok-mini`) so the prefix router selects the xAI backend",
	},
	{
		EnvVar:       "DASHSCOPE_API_KEY",
		ProviderName: "Alibaba DashScope",
		Hint:         "prefix your model name with `qwen/` or `qwen-` (e.g. `--model qwen-plus`) so prefix routing selects the DashScope backend",
	},
}

// SuggestForeignProvider checks whether a foreign provider's API key is set
// and returns a hint string suggesting model-prefix routing. Returns "" if
// no foreign credentials are detected.
func SuggestForeignProvider() string {
	for _, fp := range ForeignProviderEnvVars {
		if os.Getenv(fp.EnvVar) != "" {
			return fmt.Sprintf("found %s set for %s — %s", fp.EnvVar, fp.ProviderName, fp.Hint)
		}
	}
	return ""
}

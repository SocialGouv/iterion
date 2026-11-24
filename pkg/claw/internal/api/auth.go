package api

import (
	"fmt"
	"net/http"
	"os"
	"strings"
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

// skAntBearerHint matches Rust's SK_ANT_BEARER_HINT constant.
const skAntBearerHint = "sk-ant-* keys go in ANTHROPIC_API_KEY (x-api-key header), not ANTHROPIC_AUTH_TOKEN (Bearer header). Move your key to ANTHROPIC_API_KEY."

package api

import "context"

// AuthMethod describes how a provider authenticates.
type AuthMethod string

const (
	AuthMethodOAuth         AuthMethod = "oauth"
	AuthMethodAPIKey        AuthMethod = "api_key"
	AuthMethodIAM           AuthMethod = "iam"            // AWS IAM (Bedrock)
	AuthMethodADC           AuthMethod = "adc"            // GCP Application Default Credentials (Vertex)
	AuthMethodAzureIdentity AuthMethod = "azure_identity" // Azure Managed Identity (Foundry)
)

// ProviderConfig holds the credentials and settings needed to create a provider client.
type ProviderConfig struct {
	APIKey     string // API key (Anthropic direct, Azure Foundry)
	OAuthToken string // OAuth 2.0 access token
	BaseURL    string // Override base URL (empty = provider default)
	Model      string // Model ID in the provider's native format
	MaxTokens  int

	// OpenAIChatGPTAccountID, when set together with OAuthToken, routes the
	// OpenAI provider through the ChatGPT-Codex backend (forfait) instead of
	// the paid api.openai.com endpoint. The account_id is read from the Codex
	// CLI's auth.json (`tokens.account_id`) and sent verbatim in the
	// `ChatGPT-Account-ID` header — without it the backend rejects the call.
	OpenAIChatGPTAccountID string

	// OpenAIClientVersion is the version string sent in both the `version:`
	// HTTP header and the User-Agent when the OpenAI provider operates in
	// ChatGPT-OAuth mode. OpenAI's backend gates model availability on this
	// value (e.g. gpt-5.5 requires codex-cli >= 0.130). Callers should pass
	// the locally installed Codex CLI version. Empty defaults to a baseline
	// version embedded in the provider.
	OpenAIClientVersion string
}

// APIClient is the interface all provider clients must implement.
type APIClient interface {
	StreamResponse(ctx context.Context, req CreateMessageRequest) (<-chan StreamEvent, error)
}

// Provider is the interface all AI providers must implement.
type Provider interface {
	// Name returns the provider identifier (e.g., "anthropic", "bedrock").
	Name() string
	// NewClient creates an API client configured for this provider.
	NewClient(cfg ProviderConfig) (APIClient, error)
	// AuthMethod returns the primary authentication method used by this provider.
	AuthMethod() AuthMethod
}

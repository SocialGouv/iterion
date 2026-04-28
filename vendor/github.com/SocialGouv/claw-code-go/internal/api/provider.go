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

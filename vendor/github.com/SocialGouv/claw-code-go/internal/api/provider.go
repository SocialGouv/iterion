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

// ChatGPTClientVersion is the codex-cli release claw presents on the
// ChatGPT-Codex wire (`version:` header + User-Agent) when the caller passes
// no OpenAIClientVersion. The backend gates model availability on it: a model
// newer than the release claw claims is refused with
//
//	400 {"detail":"The '<model>' model requires a newer version of Codex.
//	     Please upgrade to the latest app or CLI and try again."}
//
// The gate is PER MODEL, and each model line raises it. Measured against the
// live endpoint on 2026-09-08, one request per cell, body held identical and
// only the `version:` header varied:
//
//	                0.130.0  0.139.0  0.144.6  0.150.0  0.152.0  0.153.0
//	gpt-5.6-sol     refused  refused  served   served   served   served
//	gpt-6-astra     refused  refused  refused  refused  refused  served
//
// So a value that unlocks one line says nothing about the next: 0.144.6 was
// enough for gpt-5.6-sol and is refused for gpt-6-astra, whose floor is
// exactly 0.153.0.
//
// Measure before bumping. The body must carry `store: false`, or the endpoint
// rejects it on that first and never reaches the model gate — a probe without
// it reports every version as equally "passing", which is how the previous
// value in this comment came to be wrong.
const ChatGPTClientVersion = "0.153.4"

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
	// value (e.g. gpt-5.5 requires codex-cli >= 0.130). Callers that can
	// probe a real Codex CLI should pass the newer of its version and
	// ChatGPTClientVersion. Empty defaults to ChatGPTClientVersion.
	OpenAIClientVersion string

	// UserAgent overrides the User-Agent header sent on every request.
	// Empty = the CLAW_USER_AGENT environment variable, then the honest
	// default "claw-code-go/<version>" (except the ChatGPT-OAuth path,
	// whose protocol-required default is the codex_cli_rs identity). See
	// identity.go for the rationale and the operator-override contract.
	UserAgent string

	// ExtraHeaders are arbitrary headers applied LAST on every request,
	// so they can override any default (including User-Agent) — same
	// semantics as Claude Code's ANTHROPIC_CUSTOM_HEADERS environment
	// variable, which is merged underneath these (explicit wins).
	ExtraHeaders map[string]string
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

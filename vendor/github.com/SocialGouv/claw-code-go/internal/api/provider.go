package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

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
	// CodexAuthFile, when set, makes the OpenAI client re-read Codex's OAuth
	// access token and account ID for each request. Codex owns this file.
	CodexAuthFile string

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

	// NoAmbientHeaders ignores the process environment's identity overrides
	// (ANTHROPIC_CUSTOM_HEADERS, CLAW_USER_AGENT): only UserAgent and
	// ExtraHeaders above reach the wire. For endpoints chosen by someone
	// other than the operator, where ambient headers may carry secrets.
	// Honoured by the OpenAI provider.
	NoAmbientHeaders bool

	// HTTPClient, when set, carries every request of the OpenAI provider
	// instead of its default clients — the hook an embedder uses to apply
	// its own egress policy (dial guard, redirect refusal, proxy choice).
	// Nil keeps the defaults.
	HTTPClient *http.Client

	// OpenAIWireAPI selects the OpenAI endpoint family. Empty keeps the
	// automatic dispatch on every host (Responses for GPT-6 and for
	// reasoning effort with tools, chat completions otherwise).
	// OpenAIWireChat forces /v1/chat/completions — for OpenAI-compatible
	// gateways that serve no /v1/responses — and cannot be combined with
	// the ChatGPT forfait, whose backend has no chat endpoint.
	// OpenAIWireResponses forces the Responses API.
	OpenAIWireAPI string

	// OpenAIStreamUsage requests stream_options.include_usage on chat
	// completions whatever the host; api.openai.com always gets it. An
	// OpenAI-compatible gateway reports token usage only when asked.
	OpenAIStreamUsage bool

	// OpenAIModelVerbatim sends the model id exactly as given: no routing
	// prefix stripped ("openai/", "qwen/", …) and no substitution of a
	// "claude…" id by the default OpenAI model. A gateway's model ids are
	// its own namespace.
	OpenAIModelVerbatim bool

	// OpenAIGenericRequest shapes chat requests the way a generic
	// OpenAI-compatible endpoint takes them, whatever the model id spells:
	// max_tokens, and tuning parameters sent as given. Without it the
	// provider reads the id as api.openai.com's models need it
	// (max_completion_tokens for the GPT-5/6 and o-series, no temperature or
	// top_p for a reasoning model) — wrong for a gateway alias that only
	// shares the spelling. It needs OpenAIWireAPI = OpenAIWireChat (so not
	// the ChatGPT forfait), and is not for api.openai.com, whose GPT-5/6
	// and o-series models refuse max_tokens and tuning parameters.
	OpenAIGenericRequest bool
}

// OpenAI endpoint families accepted by ProviderConfig.OpenAIWireAPI.
const (
	OpenAIWireChat      = "chat"
	OpenAIWireResponses = "responses"
)

// RefuseOpenAIOnlyOptions returns an error when cfg sets an option that only
// the OpenAI provider honours. A caller relying on one of them — an
// egress-policy client, a no-ambient-headers guarantee — learns that it did
// not apply, instead of silently getting the provider's defaults.
func RefuseOpenAIOnlyOptions(provider string, cfg ProviderConfig) error {
	var set []string
	if cfg.NoAmbientHeaders {
		set = append(set, "NoAmbientHeaders")
	}
	if cfg.HTTPClient != nil {
		set = append(set, "HTTPClient")
	}
	if cfg.OpenAIWireAPI != "" {
		set = append(set, "OpenAIWireAPI")
	}
	if cfg.OpenAIStreamUsage {
		set = append(set, "OpenAIStreamUsage")
	}
	if cfg.OpenAIModelVerbatim {
		set = append(set, "OpenAIModelVerbatim")
	}
	if cfg.OpenAIGenericRequest {
		set = append(set, "OpenAIGenericRequest")
	}
	if len(set) == 0 {
		return nil
	}
	return fmt.Errorf("%s provider: %s is honoured only by the OpenAI provider", provider, strings.Join(set, ", "))
}

// APIClient is the interface all provider clients must implement.
type APIClient interface {
	StreamResponse(ctx context.Context, req CreateMessageRequest) (<-chan StreamEvent, error)
}

// GeneratedImage is an image returned by a provider's separate image endpoint.
// Data is bounded by the provider and must be validated before persistence.
type GeneratedImage struct {
	Data          []byte
	RevisedPrompt string
}

// ImageGenerator is an optional provider capability. Text-only providers do
// not need to implement it.
type ImageGenerator interface {
	GenerateImage(ctx context.Context, prompt string) (GeneratedImage, error)
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

// Package foundry implements the Microsoft Azure AI Foundry / Azure OpenAI
// Service provider. The wire format is the OpenAI Chat Completions API; the
// only Azure-specific bits are:
//
//   - URL shape:
//     {endpoint}/openai/deployments/{deployment}/chat/completions?api-version={apiVersion}
//   - Auth: api-key header (AZURE_OPENAI_API_KEY) OR Azure AD bearer token via
//     azidentity.DefaultAzureCredential.
//   - The deployment name (NOT the underlying model) routes the request, so we
//     treat cfg.Model as the deployment name. Users configure
//     `model: "foundry/<deployment>"` in iterion and we strip the prefix.
//
// Required environment variables:
//   - AZURE_OPENAI_ENDPOINT:   resource URL, e.g. "https://my-resource.openai.azure.com"
//   - AZURE_OPENAI_DEPLOYMENT: deployment name (also accepted via cfg.Model)
//   - AZURE_OPENAI_API_KEY:    optional; if unset, we fall back to DefaultAzureCredential.
//   - AZURE_OPENAI_API_VERSION: optional; defaults to "2024-08-01-preview".
package foundry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/SocialGouv/claw-code-go/internal/api"
	"github.com/SocialGouv/claw-code-go/internal/api/httputil"
	"github.com/SocialGouv/claw-code-go/internal/api/providers/openaiwire"
)

const (
	defaultAPIVersion = "2024-08-01-preview"

	// azureScope is the OAuth scope used when authenticating via DefaultAzureCredential.
	azureScope = "https://cognitiveservices.azure.com/.default"
)

// Provider implements api.Provider for Azure AI Foundry / Azure OpenAI.
type Provider struct{}

// New returns a new Foundry Provider.
func New() *Provider { return &Provider{} }

// Name returns the provider identifier.
func (p *Provider) Name() string { return "foundry" }

// AuthMethod returns the Azure Identity auth method (api-key is also supported).
func (p *Provider) AuthMethod() api.AuthMethod { return api.AuthMethodAzureIdentity }

// NewClient creates a Foundry HTTP client.
//
// Endpoint precedence: cfg.BaseURL > AZURE_OPENAI_ENDPOINT.
// Deployment precedence: stripped cfg.Model > AZURE_OPENAI_DEPLOYMENT.
// Auth precedence: cfg.APIKey > AZURE_OPENAI_API_KEY > azidentity.DefaultAzureCredential.
func (p *Provider) NewClient(cfg api.ProviderConfig) (api.APIClient, error) {
	endpoint := strings.TrimSpace(cfg.BaseURL)
	if endpoint == "" {
		endpoint = strings.TrimSpace(os.Getenv("AZURE_OPENAI_ENDPOINT"))
	}
	if endpoint == "" {
		return nil, fmt.Errorf("foundry provider: AZURE_OPENAI_ENDPOINT is not set (e.g. https://my-resource.openai.azure.com)")
	}
	endpoint = strings.TrimRight(endpoint, "/")

	deployment := MapModelID(cfg.Model)
	if deployment == "" {
		deployment = strings.TrimSpace(os.Getenv("AZURE_OPENAI_DEPLOYMENT"))
	}
	if deployment == "" {
		return nil, fmt.Errorf("foundry provider: deployment name not set (configure `model: \"foundry/<deployment>\"` or AZURE_OPENAI_DEPLOYMENT)")
	}

	apiVersion := strings.TrimSpace(os.Getenv("AZURE_OPENAI_API_VERSION"))
	if apiVersion == "" {
		apiVersion = defaultAPIVersion
	}

	apiKey := strings.TrimSpace(cfg.APIKey)
	if apiKey == "" {
		apiKey = strings.TrimSpace(os.Getenv("AZURE_OPENAI_API_KEY"))
	}

	c := &Client{
		Endpoint:   endpoint,
		Deployment: deployment,
		APIVersion: apiVersion,
		APIKey:     apiKey,
		MaxTokens:  cfg.MaxTokens,
		HTTPClient: api.NewStreamingHTTPClient(),
	}

	// If no API key, prepare an Azure AD credential. We acquire the bearer
	// token lazily on each request to honour expiry/refresh semantics.
	if apiKey == "" {
		cred, err := azidentity.NewDefaultAzureCredential(nil)
		if err != nil {
			return nil, fmt.Errorf("foundry provider: AZURE_OPENAI_API_KEY not set and DefaultAzureCredential unavailable: %w", err)
		}
		c.Credential = cred
	}

	return c, nil
}

// MapModelID returns the Azure deployment name for cfg.Model, stripping a
// leading "foundry/" routing prefix when present. Azure routes by deployment,
// not by model — users typically name their deployment after the model
// (e.g. "gpt-5.4-mini-prod").
func MapModelID(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}
	if idx := strings.Index(model, "/"); idx >= 0 && model[:idx] == "foundry" {
		return model[idx+1:]
	}
	return model
}

// ----- Client ----------------------------------------------------------------

// Client is the Azure OpenAI / Foundry HTTP API client. It implements api.APIClient.
type Client struct {
	Endpoint   string // e.g. "https://my-resource.openai.azure.com"
	Deployment string // e.g. "gpt-5.4-mini-prod"
	APIVersion string // e.g. "2024-08-01-preview"

	APIKey string // when set, sent as `api-key` header; takes precedence over Credential.

	// Credential is used to mint Azure AD bearer tokens when APIKey is empty.
	Credential azcore.TokenCredential

	MaxTokens  int
	HTTPClient *http.Client
}

// endpoint returns the chat completions URL for this client.
func (c *Client) endpoint() string {
	q := url.Values{}
	q.Set("api-version", c.APIVersion)
	return fmt.Sprintf(
		"%s/openai/deployments/%s/chat/completions?%s",
		c.Endpoint,
		url.PathEscape(c.Deployment),
		q.Encode(),
	)
}

// applyAuth attaches either the api-key header or an Azure AD bearer token.
func (c *Client) applyAuth(ctx context.Context, req *http.Request) error {
	if c.APIKey != "" {
		req.Header.Set("api-key", c.APIKey)
		return nil
	}
	if c.Credential == nil {
		return fmt.Errorf("foundry: no API key and no Azure credential configured")
	}
	tok, err := c.Credential.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{azureScope}})
	if err != nil {
		return fmt.Errorf("foundry: GetToken: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	return nil
}

// ----- Wire request shape ----------------------------------------------------
//
// We share the per-message and per-tool wire types with the openai provider
// via internal/api/providers/openaiwire. The top-level request envelope
// stays foundry-local because Azure does not expect the `model` field
// (deployment is encoded in the URL) and uses *int rather than int for
// max_tokens so it can be omitted.

type oaiRequest struct {
	Messages         []openaiwire.Message   `json:"messages"`
	Tools            []openaiwire.Tool      `json:"tools,omitempty"`
	Stream           bool                   `json:"stream"`
	StreamOptions    *openaiwire.StreamOpts `json:"stream_options,omitempty"`
	MaxTokens        *int                   `json:"max_tokens,omitempty"`
	Temperature      *float64               `json:"temperature,omitempty"`
	TopP             *float64               `json:"top_p,omitempty"`
	FrequencyPenalty *float64               `json:"frequency_penalty,omitempty"`
	PresencePenalty  *float64               `json:"presence_penalty,omitempty"`
	Stop             []string               `json:"stop,omitempty"`
	ReasoningEffort  string                 `json:"reasoning_effort,omitempty"`
}

// ----- buildRequest ----------------------------------------------------------

func (c *Client) buildRequest(req api.CreateMessageRequest) ([]byte, error) {
	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = c.MaxTokens
	}

	tools, err := openaiwire.ConvertTools("foundry", req.Tools)
	if err != nil {
		return nil, err
	}

	r := oaiRequest{
		Messages:         openaiwire.ConvertMessages(req.System, req.Messages),
		Tools:            tools,
		Stream:           true,
		StreamOptions:    &openaiwire.StreamOpts{IncludeUsage: true},
		Temperature:      req.Temperature,
		TopP:             req.TopP,
		FrequencyPenalty: req.FrequencyPenalty,
		PresencePenalty:  req.PresencePenalty,
		Stop:             req.Stop,
		ReasoningEffort:  req.ReasoningEffort,
	}
	if maxTokens > 0 {
		r.MaxTokens = &maxTokens
	}
	return json.Marshal(r)
}

// ----- StreamResponse --------------------------------------------------------

// StreamResponse sends a streaming chat completions request to the Azure
// OpenAI / Foundry deployment and emits api.StreamEvents in the same shape as
// the OpenAI provider. The SSE → api.StreamEvent translation is delegated to
// openaiwire.StreamEvents, which both providers share.
func (c *Client) StreamResponse(ctx context.Context, req api.CreateMessageRequest) (<-chan api.StreamEvent, error) {
	body, err := c.buildRequest(req)
	if err != nil {
		return nil, fmt.Errorf("foundry: build request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("foundry: create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if err := c.applyAuth(ctx, httpReq); err != nil {
		return nil, err
	}

	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("foundry: do request: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		errBody, _ := io.ReadAll(resp.Body)
		bodyStr := string(errBody)
		return nil, &api.APIError{
			Provider:   "foundry",
			StatusCode: resp.StatusCode,
			Message:    extractFoundryErrorMessage(bodyStr),
			Body:       httputil.TruncateBody(bodyStr, httputil.BodyTruncateForLog),
			Retryable:  api.IsRetryableStatus(resp.StatusCode),
		}
	}

	ch := make(chan api.StreamEvent, 64)
	go openaiwire.StreamEvents(ctx, resp, ch)
	return ch, nil
}

// ----- helpers --------------------------------------------------------------

// extractFoundryErrorMessage best-effort decodes the standard OpenAI error
// envelope ({"error":{"message":"...","code":"..."}}) used by Azure OpenAI.
func extractFoundryErrorMessage(body string) string {
	var parsed struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err == nil && parsed.Error.Message != "" {
		return parsed.Error.Message
	}
	return httputil.TruncateBody(body, httputil.BodyTruncateForMessage)
}

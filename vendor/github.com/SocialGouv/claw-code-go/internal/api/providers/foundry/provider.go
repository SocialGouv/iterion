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
	"bufio"
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
		HTTPClient: &http.Client{},
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

// ----- Wire types (OpenAI-compatible) ----------------------------------------
//
// We duplicate the OpenAI wire types here rather than importing the openai
// package to keep providers loosely coupled. The shape mirrors what Azure
// OpenAI accepts and matches the OpenAI Chat Completions API.

type oaiRequest struct {
	Messages         []oaiMessage `json:"messages"`
	Tools            []oaiTool    `json:"tools,omitempty"`
	Stream           bool         `json:"stream"`
	StreamOptions    *streamOpts  `json:"stream_options,omitempty"`
	MaxTokens        *int         `json:"max_tokens,omitempty"`
	Temperature      *float64     `json:"temperature,omitempty"`
	TopP             *float64     `json:"top_p,omitempty"`
	FrequencyPenalty *float64     `json:"frequency_penalty,omitempty"`
	PresencePenalty  *float64     `json:"presence_penalty,omitempty"`
	Stop             []string     `json:"stop,omitempty"`
	ReasoningEffort  string       `json:"reasoning_effort,omitempty"`
}

type streamOpts struct {
	IncludeUsage bool `json:"include_usage"`
}

type oaiMessage struct {
	Role       string        `json:"role"`
	Content    *string       `json:"content,omitempty"`
	ToolCalls  []oaiToolCall `json:"tool_calls,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
}

type oaiTool struct {
	Type     string      `json:"type"`
	Function oaiFunction `json:"function"`
}

type oaiFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type oaiToolCall struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	Function oaiFunctionCall `json:"function"`
}

type oaiFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type oaiChunk struct {
	Choices []oaiChoice `json:"choices"`
	Usage   *oaiUsage   `json:"usage"`
}

type oaiChoice struct {
	Index        int      `json:"index"`
	Delta        oaiDelta `json:"delta"`
	FinishReason *string  `json:"finish_reason"`
}

type oaiDelta struct {
	Content   *string            `json:"content"`
	ToolCalls []oaiToolCallDelta `json:"tool_calls"`
}

type oaiToolCallDelta struct {
	Index    int              `json:"index"`
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function oaiFunctionDelta `json:"function"`
}

type oaiFunctionDelta struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type oaiUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

// ----- buildRequest ----------------------------------------------------------

func (c *Client) buildRequest(req api.CreateMessageRequest) ([]byte, error) {
	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = c.MaxTokens
	}

	r := oaiRequest{
		Messages:         convertMessages(req.System, req.Messages),
		Tools:            convertTools(req.Tools),
		Stream:           true,
		StreamOptions:    &streamOpts{IncludeUsage: true},
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

func convertMessages(system string, messages []api.Message) []oaiMessage {
	var result []oaiMessage
	if system != "" {
		result = append(result, oaiMessage{Role: "system", Content: strPtr(system)})
	}
	for _, msg := range messages {
		switch msg.Role {
		case "user":
			var texts []string
			for _, block := range msg.Content {
				switch block.Type {
				case "text":
					if block.Text != "" {
						texts = append(texts, block.Text)
					}
				case "tool_result":
					content := extractText(block.Content)
					result = append(result, oaiMessage{
						Role:       "tool",
						Content:    strPtr(content),
						ToolCallID: block.ToolUseID,
					})
				}
			}
			if len(texts) > 0 {
				result = append(result, oaiMessage{
					Role:    "user",
					Content: strPtr(strings.Join(texts, "\n")),
				})
			}
		case "assistant":
			var texts []string
			var toolCalls []oaiToolCall
			for _, block := range msg.Content {
				switch block.Type {
				case "text":
					if block.Text != "" {
						texts = append(texts, block.Text)
					}
				case "tool_use":
					args, _ := json.Marshal(block.Input)
					toolCalls = append(toolCalls, oaiToolCall{
						ID:   block.ID,
						Type: "function",
						Function: oaiFunctionCall{
							Name:      block.Name,
							Arguments: string(args),
						},
					})
				}
			}
			var contentPtr *string
			if len(texts) > 0 {
				contentPtr = strPtr(strings.Join(texts, "\n"))
			}
			result = append(result, oaiMessage{
				Role:      "assistant",
				Content:   contentPtr,
				ToolCalls: toolCalls,
			})
		}
	}
	return result
}

func convertTools(tools []api.Tool) []oaiTool {
	result := make([]oaiTool, 0, len(tools))
	for _, t := range tools {
		params, err := json.Marshal(map[string]interface{}{
			"type":       t.InputSchema.Type,
			"properties": t.InputSchema.Properties,
			"required":   t.InputSchema.Required,
		})
		if err != nil {
			continue
		}
		result = append(result, oaiTool{
			Type: "function",
			Function: oaiFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  json.RawMessage(params),
			},
		})
	}
	return result
}

// ----- StreamResponse --------------------------------------------------------

// StreamResponse sends a streaming chat completions request to the Azure
// OpenAI / Foundry deployment and emits api.StreamEvents in the same shape as
// the OpenAI provider.
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
			Body:       truncateBody(bodyStr, 1000),
			Retryable:  api.IsRetryableStatus(resp.StatusCode),
		}
	}

	ch := make(chan api.StreamEvent, 64)
	go c.streamEvents(ctx, resp, ch)
	return ch, nil
}

// ----- Streaming event conversion --------------------------------------------

type pendingToolCall struct {
	id           string
	name         string
	startEmitted bool
	blockIndex   int
}

func (c *Client) streamEvents(ctx context.Context, resp *http.Response, ch chan<- api.StreamEvent) {
	defer close(ch)
	defer resp.Body.Close()

	send := func(ev api.StreamEvent) bool {
		select {
		case ch <- ev:
			return true
		case <-ctx.Done():
			return false
		}
	}

	if !send(api.StreamEvent{Type: api.EventMessageStart}) {
		return
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	var (
		textStarted  bool
		toolCalls    = make(map[int]*pendingToolCall)
		finishReason string
		outputTokens int
	)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, "event:") {
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		var chunk oaiChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}

		if chunk.Usage != nil {
			outputTokens = chunk.Usage.CompletionTokens
		}

		for _, choice := range chunk.Choices {
			delta := choice.Delta

			if delta.Content != nil && *delta.Content != "" {
				if !textStarted {
					textStarted = true
					if !send(api.StreamEvent{
						Type:         api.EventContentBlockStart,
						Index:        0,
						ContentBlock: api.ContentBlockInfo{Type: "text", Index: 0},
					}) {
						return
					}
				}
				if !send(api.StreamEvent{
					Type:  api.EventContentBlockDelta,
					Index: 0,
					Delta: api.Delta{Type: "text_delta", Text: *delta.Content},
				}) {
					return
				}
			}

			for _, tc := range delta.ToolCalls {
				idx := tc.Index
				if _, ok := toolCalls[idx]; !ok {
					toolCalls[idx] = &pendingToolCall{blockIndex: 1 + idx}
				}
				pending := toolCalls[idx]

				if tc.ID != "" {
					pending.id = tc.ID
				}
				if tc.Function.Name != "" {
					pending.name = tc.Function.Name
				}

				if !pending.startEmitted && pending.id != "" && pending.name != "" {
					pending.startEmitted = true
					if !send(api.StreamEvent{
						Type:  api.EventContentBlockStart,
						Index: pending.blockIndex,
						ContentBlock: api.ContentBlockInfo{
							Type:  "tool_use",
							Index: pending.blockIndex,
							ID:    pending.id,
							Name:  pending.name,
						},
					}) {
						return
					}
				}
				if pending.startEmitted && tc.Function.Arguments != "" {
					if !send(api.StreamEvent{
						Type:  api.EventContentBlockDelta,
						Index: pending.blockIndex,
						Delta: api.Delta{Type: "input_json_delta", PartialJSON: tc.Function.Arguments},
					}) {
						return
					}
				}
			}

			if choice.FinishReason != nil && *choice.FinishReason != "" {
				finishReason = *choice.FinishReason
			}
		}
	}

	if textStarted {
		if !send(api.StreamEvent{Type: api.EventContentBlockStop, Index: 0}) {
			return
		}
	}
	for i := 0; i < len(toolCalls); i++ {
		if tc, ok := toolCalls[i]; ok && tc.startEmitted {
			if !send(api.StreamEvent{Type: api.EventContentBlockStop, Index: tc.blockIndex}) {
				return
			}
		}
	}

	stopReason := "end_turn"
	if finishReason == "tool_calls" {
		stopReason = "tool_use"
	}
	send(api.StreamEvent{
		Type:       api.EventMessageDelta,
		StopReason: stopReason,
		Usage:      api.UsageDelta{OutputTokens: outputTokens},
	})
	send(api.StreamEvent{Type: api.EventMessageStop})
}

// ----- helpers --------------------------------------------------------------

func strPtr(s string) *string { return &s }

func extractText(blocks []api.ContentBlock) string {
	var parts []string
	for _, b := range blocks {
		if b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

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
	return truncateBody(body, 200)
}

func truncateBody(body string, maxRunes int) string {
	r := []rune(body)
	if len(r) <= maxRunes {
		return body
	}
	return string(r[:maxRunes]) + "…"
}


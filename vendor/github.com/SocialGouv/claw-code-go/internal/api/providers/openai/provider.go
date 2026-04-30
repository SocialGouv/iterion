// Package openai implements the api.Provider and api.APIClient interfaces
// for OpenAI's chat completions API with streaming support.
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/SocialGouv/claw-code-go/internal/api"
	"github.com/SocialGouv/claw-code-go/internal/api/httputil"
	"github.com/SocialGouv/claw-code-go/internal/api/providers/openaiwire"
	"github.com/SocialGouv/claw-code-go/internal/strutil"
)

const (
	defaultBaseURL     = "https://api.openai.com"
	DefaultOpenAIModel = "gpt-4o"
)

// Provider implements api.Provider for OpenAI.
type Provider struct{}

// New returns a new OpenAI Provider.
func New() *Provider { return &Provider{} }

func (p *Provider) Name() string               { return "openai" }
func (p *Provider) AuthMethod() api.AuthMethod { return api.AuthMethodAPIKey }

// NewClient creates an OpenAI API client from the given config.
func (p *Provider) NewClient(cfg api.ProviderConfig) (api.APIClient, error) {
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("API key required for OpenAI-compatible provider (set the appropriate API key env var or run /login)")
	}
	model := cfg.Model
	// If the configured model is an Anthropic model name, use the default OpenAI model.
	if model == "" || strings.HasPrefix(model, "claude") {
		model = DefaultOpenAIModel
	}
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	return &Client{
		APIKey:     cfg.APIKey,
		BaseURL:    baseURL,
		Model:      model,
		MaxTokens:  cfg.MaxTokens,
		HTTPClient: api.NewStreamingHTTPClient(),
	}, nil
}

// ----- Client ----------------------------------------------------------------

// Client is the OpenAI HTTP API client.
type Client struct {
	APIKey     string
	BaseURL    string
	Model      string
	MaxTokens  int
	HTTPClient *http.Client
}

// ----- Request types ---------------------------------------------------------

// oaiRequest is the request body sent to the OpenAI chat completions endpoint.
// It uses openaiwire.Message and openaiwire.Tool for the per-message and
// per-tool wire shapes, but the envelope itself stays openai-local because
// of the reasoning-model gymnastics in MarshalJSON below — Foundry does not
// need them, and Bedrock targets a different wire entirely.
type oaiRequest struct {
	Model            string                 `json:"model"`
	Messages         []openaiwire.Message   `json:"messages"`
	Tools            []openaiwire.Tool      `json:"tools,omitempty"`
	Stream           bool                   `json:"stream"`
	StreamOptions    *openaiwire.StreamOpts `json:"stream_options,omitempty"`
	MaxTokens        int                    `json:"-"` // written conditionally in MarshalJSON
	Temperature      *float64               `json:"-"` // omitted for reasoning models
	TopP             *float64               `json:"-"` // omitted for reasoning models
	FrequencyPenalty *float64               `json:"-"` // omitted for reasoning models
	PresencePenalty  *float64               `json:"-"` // omitted for reasoning models
	Stop             []string               `json:"-"` // always safe
	ReasoningEffort  string                 `json:"-"` // reasoning models only
	isReasoningModel bool                   // controls conditional field inclusion
	useMaxCompTokens bool                   // gpt-5* uses max_completion_tokens
}

// MarshalJSON implements custom marshaling to conditionally include/exclude
// fields based on model capabilities.
func (r oaiRequest) MarshalJSON() ([]byte, error) {
	// Use an alias to avoid infinite recursion.
	type Alias oaiRequest
	type wire struct {
		Alias
		MaxTokens           *int     `json:"max_tokens,omitempty"`
		MaxCompletionTokens *int     `json:"max_completion_tokens,omitempty"`
		Temperature         *float64 `json:"temperature,omitempty"`
		TopP                *float64 `json:"top_p,omitempty"`
		FrequencyPenalty    *float64 `json:"frequency_penalty,omitempty"`
		PresencePenalty     *float64 `json:"presence_penalty,omitempty"`
		Stop                []string `json:"stop,omitempty"`
		ReasoningEffort     string   `json:"reasoning_effort,omitempty"`
	}
	w := wire{Alias: Alias(r)}

	// max_tokens vs max_completion_tokens
	if r.MaxTokens > 0 {
		if r.useMaxCompTokens {
			w.MaxCompletionTokens = &r.MaxTokens
		} else {
			w.MaxTokens = &r.MaxTokens
		}
	}

	// Tuning params: only for non-reasoning models
	if !r.isReasoningModel {
		w.Temperature = r.Temperature
		w.TopP = r.TopP
		w.FrequencyPenalty = r.FrequencyPenalty
		w.PresencePenalty = r.PresencePenalty
	}

	// Stop and reasoning_effort are safe for all models
	if len(r.Stop) > 0 {
		w.Stop = r.Stop
	}
	w.ReasoningEffort = r.ReasoningEffort

	return json.Marshal(w)
}

// ----- StreamResponse --------------------------------------------------------

// StreamResponse sends a streaming request to OpenAI and emits StreamEvents
// that are compatible with the conversation loop's event processing.
//
// Dispatch: when reasoning_effort and tools are both present, we route to
// /v1/responses because /v1/chat/completions rejects that combination on
// gpt-5.5+. Every other request keeps using the well-tested chat
// completions path.
func (c *Client) StreamResponse(ctx context.Context, req api.CreateMessageRequest) (<-chan api.StreamEvent, error) {
	if shouldUseResponsesAPI(req) {
		return c.streamResponses(ctx, req)
	}

	oaiReq, err := c.buildRequest(req)
	if err != nil {
		return nil, fmt.Errorf("openai: build request: %w", err)
	}

	body, err := json.Marshal(oaiReq)
	if err != nil {
		return nil, fmt.Errorf("openai: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("openai: create request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openai: request failed: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		errBody, _ := io.ReadAll(resp.Body)
		bodyStr := string(errBody)
		return nil, &api.APIError{
			Provider:   "openai",
			StatusCode: resp.StatusCode,
			Message:    extractOpenAIErrorMessage(bodyStr),
			Body:       httputil.TruncateBody(bodyStr, httputil.BodyTruncateForLog),
			Retryable:  api.IsRetryableStatus(resp.StatusCode),
		}
	}

	ch := make(chan api.StreamEvent, 64)
	go openaiwire.StreamEvents(ctx, resp, ch)
	return ch, nil
}

// ----- Request conversion ----------------------------------------------------

func (c *Client) buildRequest(req api.CreateMessageRequest) (*oaiRequest, error) {
	// Honour the model from the request only when it's an OpenAI model name.
	model := c.Model
	if req.Model != "" && !strings.HasPrefix(req.Model, "claude") {
		model = req.Model
	}

	wireModel := stripRoutingPrefix(model)
	reasoning := isReasoningModel(model)

	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = c.MaxTokens
	}

	tools, err := openaiwire.ConvertTools("openai", req.Tools)
	if err != nil {
		return nil, err
	}

	r := &oaiRequest{
		Model:            wireModel,
		Messages:         openaiwire.ConvertMessages(req.System, req.Messages),
		Tools:            tools,
		Stream:           true,
		MaxTokens:        maxTokens,
		Temperature:      req.Temperature,
		TopP:             req.TopP,
		FrequencyPenalty: req.FrequencyPenalty,
		PresencePenalty:  req.PresencePenalty,
		Stop:             req.Stop,
		ReasoningEffort:  req.ReasoningEffort,
		isReasoningModel: reasoning,
		useMaxCompTokens: strings.HasPrefix(wireModel, "gpt-5"),
	}

	// Only request stream usage for providers that support it (matches Rust's
	// should_request_stream_usage which gates on provider_name == "OpenAI").
	if r.Stream && c.shouldRequestStreamUsage() {
		r.StreamOptions = &openaiwire.StreamOpts{IncludeUsage: true}
	}

	return r, nil
}

// shouldRequestStreamUsage returns true when the client targets the default
// OpenAI endpoint. XAI, DashScope, and other OpenAI-compatible providers
// may not support the stream_options parameter, so we only include it for
// the canonical OpenAI API. This matches Rust's should_request_stream_usage()
// which gates on provider_name == "OpenAI".
func (c *Client) shouldRequestStreamUsage() bool {
	return c.BaseURL == defaultBaseURL
}

// isReasoningModel returns true for models known to reject tuning parameters
// like temperature, top_p, frequency_penalty, and presence_penalty. These are
// typically reasoning/chain-of-thought models with fixed sampling.
func isReasoningModel(model string) bool {
	lowered := strutil.ASCIIToLower(model)
	// Strip provider prefix: e.g. "qwen/qwen-qwq" → "qwen-qwq"
	canonical := lowered
	if idx := strings.LastIndex(lowered, "/"); idx >= 0 {
		canonical = lowered[idx+1:]
	}
	// OpenAI reasoning models
	if strings.HasPrefix(canonical, "o1") ||
		strings.HasPrefix(canonical, "o3") ||
		strings.HasPrefix(canonical, "o4") {
		return true
	}
	// xAI reasoning: grok-3-mini always uses reasoning mode
	if canonical == "grok-3-mini" {
		return true
	}
	// Alibaba DashScope reasoning variants (QwQ + Qwen3-Thinking family)
	if strings.HasPrefix(canonical, "qwen-qwq") ||
		strings.HasPrefix(canonical, "qwq") ||
		strings.Contains(canonical, "thinking") {
		return true
	}
	return false
}

// stripRoutingPrefix removes known routing prefixes from model names.
// e.g. "openai/gpt-4" → "gpt-4", "xai/grok-3" → "grok-3".
// Unknown prefixes are left intact.
func stripRoutingPrefix(model string) string {
	idx := strings.Index(model, "/")
	if idx < 0 {
		return model
	}
	prefix := model[:idx]
	switch prefix {
	case "openai", "xai", "grok", "qwen":
		return model[idx+1:]
	}
	return model
}

// ----- Helpers ---------------------------------------------------------------

// extractOpenAIErrorMessage best-effort decodes the standard OpenAI error
// envelope ({"error":{"message":"...","type":"...","code":"..."}}) and
// returns just the human-readable message. Falls back to the raw body
// (truncated) when the envelope shape doesn't match.
func extractOpenAIErrorMessage(body string) string {
	var parsed struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err == nil && parsed.Error.Message != "" {
		return parsed.Error.Message
	}
	return httputil.TruncateBody(body, httputil.BodyTruncateForMessage)
}

// Package openai implements the api.Provider and api.APIClient interfaces
// for OpenAI's chat completions API with streaming support.
package openai

import (
	"bufio"
	"bytes"
	"claw-code-go/internal/api"
	"claw-code-go/internal/strutil"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
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
		HTTPClient: &http.Client{},
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
// Fields that must be conditionally omitted for reasoning models use custom
// MarshalJSON rather than map[string]interface{} to preserve compile-time type safety.
type oaiRequest struct {
	Model            string       `json:"model"`
	Messages         []oaiMessage `json:"messages"`
	Tools            []oaiTool    `json:"tools,omitempty"`
	Stream           bool         `json:"stream"`
	StreamOptions    *streamOpts  `json:"stream_options,omitempty"`
	MaxTokens        int          `json:"-"` // written conditionally in MarshalJSON
	Temperature      *float64     `json:"-"` // omitted for reasoning models
	TopP             *float64     `json:"-"` // omitted for reasoning models
	FrequencyPenalty *float64     `json:"-"` // omitted for reasoning models
	PresencePenalty  *float64     `json:"-"` // omitted for reasoning models
	Stop             []string     `json:"-"` // always safe
	ReasoningEffort  string       `json:"-"` // reasoning models only
	isReasoningModel bool         // controls conditional field inclusion
	useMaxCompTokens bool         // gpt-5* uses max_completion_tokens
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

// ----- Response / streaming types --------------------------------------------

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

// ----- StreamResponse --------------------------------------------------------

// StreamResponse sends a streaming request to OpenAI and emits StreamEvents
// that are compatible with the conversation loop's event processing.
func (c *Client) StreamResponse(ctx context.Context, req api.CreateMessageRequest) (<-chan api.StreamEvent, error) {
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
		return nil, fmt.Errorf("openai: API error %d: %s", resp.StatusCode, string(errBody))
	}

	ch := make(chan api.StreamEvent, 64)
	go c.streamEvents(ctx, resp, ch)
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

	r := &oaiRequest{
		Model:            wireModel,
		Messages:         convertMessages(req.System, req.Messages),
		Tools:            convertTools(req.Tools),
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
		r.StreamOptions = &streamOpts{IncludeUsage: true}
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

// convertMessages maps our Anthropic-style messages to the OpenAI message format.
//
// Key differences:
//   - System prompt → role "system" message prepended
//   - User tool_result blocks → separate role "tool" messages (one per result)
//   - Assistant tool_use blocks → tool_calls array on the assistant message
func convertMessages(system string, messages []api.Message) []oaiMessage {
	var result []oaiMessage

	if system != "" {
		result = append(result, oaiMessage{Role: "system", Content: strPtr(system)})
	}

	for _, msg := range messages {
		switch msg.Role {
		case "user":
			// Split text content from tool results; tool results become role "tool".
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

// convertTools maps our Tool definitions to OpenAI function-calling format.
// The key difference is that OpenAI uses "parameters" instead of "input_schema".
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

// ----- Streaming event conversion --------------------------------------------

type pendingToolCall struct {
	id           string
	name         string
	startEmitted bool
	blockIndex   int
}

// streamEvents reads OpenAI SSE chunks from resp and emits api.StreamEvent
// values compatible with the conversation loop's runOneTurnStreaming.
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

	// Emit a placeholder message start (token counts filled in at the end).
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
		inputTokens  int
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

		// Capture usage from the final usage chunk (choices will be empty there).
		if chunk.Usage != nil {
			inputTokens = chunk.Usage.PromptTokens
			outputTokens = chunk.Usage.CompletionTokens
		}

		for _, choice := range chunk.Choices {
			delta := choice.Delta

			// -- Text content delta --
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

			// -- Tool call deltas --
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

				// Emit ContentBlockStart once we have both id and name.
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

				// Stream argument fragments after the start event is sent.
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

			// Remember finish reason for after the loop.
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				finishReason = *choice.FinishReason
			}
		}
	}

	// Close the text block.
	if textStarted {
		if !send(api.StreamEvent{Type: api.EventContentBlockStop, Index: 0}) {
			return
		}
	}

	// Close all tool blocks (in index order to keep things tidy).
	for i := 0; i < len(toolCalls); i++ {
		if tc, ok := toolCalls[i]; ok && tc.startEmitted {
			if !send(api.StreamEvent{Type: api.EventContentBlockStop, Index: tc.blockIndex}) {
				return
			}
		}
	}

	// Map OpenAI finish_reason to our stop_reason vocabulary.
	stopReason := "end_turn"
	if finishReason == "tool_calls" {
		stopReason = "tool_use"
	}

	_ = inputTokens // reported via MessageStart (sent with 0 above; usage is informational)
	send(api.StreamEvent{
		Type:       api.EventMessageDelta,
		StopReason: stopReason,
		Usage:      api.UsageDelta{OutputTokens: outputTokens},
	})
	send(api.StreamEvent{Type: api.EventMessageStop})
}

// ----- Helpers ---------------------------------------------------------------

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

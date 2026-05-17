package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/SocialGouv/claw-code-go/internal/apikit"
)

const (
	defaultBaseURL         = "https://api.anthropic.com"
	anthropicVersion       = "2023-06-01"
	anthropicBetaHeader = "anthropic-beta"
	// Comma-separated beta flags. The prompt-caching-2024-07-31 token
	// enables the per-block cache_control marker; pinning the
	// caching-scope token additionally lets the API recognise scope
	// directives on individual system blocks instead of treating
	// system as a monolithic cache zone. Without scope, long-lived
	// iterion server prompts miss the cache after the smallest
	// rotating field flips and pay full input-token cost every call.
	anthropicBetaValue     = "prompt-caching-2024-07-31,prompt-caching-scope-2026-01-05"
	anthropicVersionHeader = "anthropic-version"

	// defaultMaxRetries is the maximum number of retry attempts for retryable
	// HTTP errors (429, 5xx). The first attempt is attempt 1.
	defaultMaxRetries = 3

	// retryBaseDelay is the initial backoff delay between retries.
	retryBaseDelay = 500 * time.Millisecond
)

// Client is the Anthropic HTTP API client.
// It implements the APIClient interface.
type Client struct {
	APIKey      string // API key for x-api-key header auth (legacy; prefer Auth)
	OAuthToken  string // OAuth access token; takes precedence over APIKey when set (legacy; prefer Auth)
	BaseURL     string
	Model       string
	HTTPClient  *http.Client
	Auth        AuthSource            // structured auth; when Kind != AuthSourceNone, takes precedence over APIKey/OAuthToken
	Tracer      *apikit.SessionTracer // optional HTTP lifecycle telemetry
	PromptCache *apikit.PromptCache   // optional prompt cache for break telemetry
}

// NewClient creates a new API client with the given API key and model.
//
// The default HTTPClient is built via httputil.NewStreamingHTTPClient so
// that connect/TLS/response-header stages have bounded timeouts. A
// half-open peer therefore cannot pin a goroutine + FD until the caller's
// run-level deadline fires, which under provider incidents would
// otherwise translate into process-level resource exhaustion across
// fan-out branches.
func NewClient(apiKey, model string) *Client {
	return &Client{
		APIKey:     apiKey,
		BaseURL:    defaultBaseURL,
		Model:      model,
		HTTPClient: NewStreamingHTTPClient(),
	}
}

// WithTracer returns the client with the given session tracer attached.
func (c *Client) WithTracer(tracer *apikit.SessionTracer) *Client {
	c.Tracer = tracer
	return c
}

// StreamResponse sends a streaming message request and returns a channel of StreamEvents.
// The channel is closed when the stream ends or an error occurs. Retryable
// failures (429, 5xx) are retried up to defaultMaxRetries times with
// exponential backoff. Each attempt is tracked in telemetry.
func (c *Client) StreamResponse(ctx context.Context, req CreateMessageRequest) (<-chan StreamEvent, error) {
	req.Stream = true

	// Preflight: reject requests that would exceed the context window.
	maxOutput := uint32(req.MaxTokens)
	if maxOutput == 0 {
		maxOutput = 8096 // default
	}
	if err := apikit.PreflightMessageRequest(c.Model, req.Messages, maxOutput); err != nil {
		return nil, err
	}

	body, err := marshalAnthropicRequest(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	var resp *http.Response
	var lastErr error

	for attempt := uint32(1); attempt <= defaultMaxRetries; attempt++ {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
			c.BaseURL+"/v1/messages", bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("create request: %w", err)
		}

		// Apply authentication headers. Prefer structured Auth; fall back to legacy fields.
		if c.Auth.Kind != AuthSourceNone {
			c.Auth.ApplyToRequest(httpReq)
		} else if c.OAuthToken != "" {
			httpReq.Header.Set("authorization", "Bearer "+c.OAuthToken)
		} else {
			httpReq.Header.Set("x-api-key", c.APIKey)
		}
		httpReq.Header.Set(anthropicVersionHeader, anthropicVersion)
		httpReq.Header.Set(anthropicBetaHeader, anthropicBetaValue)
		httpReq.Header.Set("content-type", "application/json")
		httpReq.Header.Set("accept", "text/event-stream")

		// Telemetry: record request started
		if c.Tracer != nil {
			c.Tracer.RecordHTTPRequestStarted(attempt, "POST", "/v1/messages", nil)
		}

		resp, lastErr = c.HTTPClient.Do(httpReq)
		if lastErr != nil {
			// Transport errors (DNS flutter, dropped TCP, TLS handshake
			// flap, captive-portal handoff, etc.) are surprisingly
			// retryable: in practice they recover within seconds-to-
			// minutes when the local network blip clears. The previous
			// "transport errors are not retryable" assumption made
			// long-running unattended pipelines fragile to occasional
			// network outages — a 5-second ISP hiccup mid-request
			// would surface as a hard run failure even though a single
			// retry would have succeeded. Now we treat them like a 5xx
			// and ride the same exponential-backoff loop, capped at
			// defaultMaxRetries so we never block forever on a real
			// outage. The iterion runtime layer adds another 6-attempt
			// network-transient recipe on top of this for true multi-
			// minute outages (see pkg/runtime/recovery).
			retryable := true
			if c.Tracer != nil {
				c.Tracer.RecordHTTPRequestFailed(attempt, "POST", "/v1/messages", lastErr.Error(), retryable, nil)
			}
			if attempt == defaultMaxRetries {
				// Wrap as a typed *APIError so callers using
				// errors.As(*APIError) can classify (Retryable=true,
				// StatusCode=0 to signal "no HTTP response"). The
				// non-OK-status branch below already returns a typed
				// APIError; the transport-error branch had been
				// returning a plain fmt.Errorf, breaking iterion's
				// runtime recovery classifier (commit c1cdea5
				// motivated typed APIError exactly for this kind of
				// downstream classification).
				return nil, &APIError{
					Provider:   "anthropic",
					StatusCode: 0,
					Message:    lastErr.Error(),
					Retryable:  true,
				}
			}
			delay := retryBaseDelay * time.Duration(1<<(attempt-1))
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			continue
		}

		if resp.StatusCode == http.StatusOK {
			// Telemetry: record request succeeded
			requestID := resp.Header.Get("x-request-id")
			if c.Tracer != nil {
				c.Tracer.RecordHTTPRequestSucceeded(attempt, "POST", "/v1/messages", uint16(resp.StatusCode), requestID, nil)
			}
			break
		}

		// Non-OK status: read error body and check retryability.
		errBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		errMsg := fmt.Sprintf("API error %d: %s", resp.StatusCode, string(errBody))
		retryable := isRetryableStatus(resp.StatusCode)

		if c.Tracer != nil {
			c.Tracer.RecordHTTPRequestFailed(attempt, "POST", "/v1/messages", errMsg, retryable, nil)
		}

		if !retryable || attempt == defaultMaxRetries {
			// Enrich 401 errors when sk-ant-* is used as Bearer token.
			enriched := EnrichBearerAuthError(errMsg, resp.StatusCode, c.Auth)
			// Return a typed APIError so callers' errors.As checks pick
			// up StatusCode + Retryable instead of having to parse the
			// free-form message. Without this, iterion's retry
			// classification falls through to the generic "unknown"
			// path on every non-retryable upstream failure.
			return nil, &APIError{
				Provider:   "anthropic",
				StatusCode: resp.StatusCode,
				Message:    enriched,
				Body:       string(errBody),
				Retryable:  false,
			}
		}

		// Exponential backoff before next attempt.
		delay := retryBaseDelay * time.Duration(1<<(attempt-1))
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		// Preserve typed retryable shape on the loop carry so the final
		// "all retries exhausted" return surfaces an APIError too.
		lastErr = &APIError{
			Provider:   "anthropic",
			StatusCode: resp.StatusCode,
			Message:    errMsg,
			Body:       string(errBody),
			Retryable:  true,
		}
	}

	ch := make(chan StreamEvent, 64)

	go func() {
		defer close(ch)
		defer resp.Body.Close()

		parser := NewSseParser().WithContext("anthropic", c.Model)
		buf := make([]byte, 64*1024)

		for {
			n, readErr := resp.Body.Read(buf)
			if n > 0 {
				events, parseErr := parser.Push(buf[:n])
				if parseErr != nil {
					select {
					case ch <- StreamEvent{
						Type:         EventError,
						ErrorMessage: fmt.Sprintf("parse SSE: %v", parseErr),
					}:
					case <-ctx.Done():
						return
					}
					break
				}
				for _, event := range events {
					select {
					case ch <- event:
					case <-ctx.Done():
						return
					}
				}
			}
			if readErr != nil {
				if readErr != io.EOF {
					select {
					case ch <- StreamEvent{
						Type:         EventError,
						ErrorMessage: fmt.Sprintf("read stream: %v", readErr),
					}:
					case <-ctx.Done():
					}
				}
				break
			}
		}

		// Flush any trailing data from the parser
		events, parseErr := parser.Finish()
		if parseErr != nil {
			select {
			case ch <- StreamEvent{
				Type:         EventError,
				ErrorMessage: fmt.Sprintf("parse SSE finish: %v", parseErr),
			}:
			case <-ctx.Done():
			}
		}
		for _, event := range events {
			select {
			case ch <- event:
			case <-ctx.Done():
				return
			}
		}
	}()

	return ch, nil
}

// parseSSEData parses a single SSE data line into a StreamEvent.
func parseSSEData(data string) (StreamEvent, error) {
	// Parse into a raw map first to handle the varying structure
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(data), &raw); err != nil {
		return StreamEvent{}, fmt.Errorf("unmarshal raw: %w", err)
	}

	var event StreamEvent

	// Parse "type"
	if typeRaw, ok := raw["type"]; ok {
		var t string
		if err := json.Unmarshal(typeRaw, &t); err == nil {
			event.Type = StreamEventType(t)
		}
	}

	// Parse "index"
	if indexRaw, ok := raw["index"]; ok {
		json.Unmarshal(indexRaw, &event.Index) //nolint:errcheck
	}

	// Parse "delta" — used in content_block_delta and message_delta
	if deltaRaw, ok := raw["delta"]; ok {
		var deltaMap map[string]json.RawMessage
		if err := json.Unmarshal(deltaRaw, &deltaMap); err == nil {
			if typeRaw, ok := deltaMap["type"]; ok {
				json.Unmarshal(typeRaw, &event.Delta.Type) //nolint:errcheck
			}
			if textRaw, ok := deltaMap["text"]; ok {
				json.Unmarshal(textRaw, &event.Delta.Text) //nolint:errcheck
			}
			if partialRaw, ok := deltaMap["partial_json"]; ok {
				json.Unmarshal(partialRaw, &event.Delta.PartialJSON) //nolint:errcheck
			}
			// For message_delta, delta contains stop_reason
			if stopRaw, ok := deltaMap["stop_reason"]; ok {
				var stopReason string
				if err := json.Unmarshal(stopRaw, &stopReason); err == nil {
					event.StopReason = stopReason
				}
			}
		}
	}

	// Parse "content_block" for content_block_start events
	if cbRaw, ok := raw["content_block"]; ok {
		var cbMap map[string]json.RawMessage
		if err := json.Unmarshal(cbRaw, &cbMap); err == nil {
			if typeRaw, ok := cbMap["type"]; ok {
				json.Unmarshal(typeRaw, &event.ContentBlock.Type) //nolint:errcheck
			}
			if idRaw, ok := cbMap["id"]; ok {
				json.Unmarshal(idRaw, &event.ContentBlock.ID) //nolint:errcheck
			}
			if nameRaw, ok := cbMap["name"]; ok {
				json.Unmarshal(nameRaw, &event.ContentBlock.Name) //nolint:errcheck
			}
		}
		event.ContentBlock.Index = event.Index
	}

	// Parse "usage" for message_delta events
	if usageRaw, ok := raw["usage"]; ok {
		json.Unmarshal(usageRaw, &event.Usage) //nolint:errcheck
	}

	// Parse "message.usage" for message_start events (input tokens + cache tokens)
	if messageRaw, ok := raw["message"]; ok {
		var msgMap map[string]json.RawMessage
		if err := json.Unmarshal(messageRaw, &msgMap); err == nil {
			if usageRaw, ok := msgMap["usage"]; ok {
				var usage struct {
					InputTokens              int `json:"input_tokens"`
					CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
					CacheReadInputTokens     int `json:"cache_read_input_tokens"`
				}
				if err := json.Unmarshal(usageRaw, &usage); err == nil {
					event.InputTokens = usage.InputTokens
					event.CacheCreationInputTokens = usage.CacheCreationInputTokens
					event.CacheReadInputTokens = usage.CacheReadInputTokens
				}
			}
		}
	}

	// Parse error details
	if event.Type == EventError {
		if errRaw, ok := raw["error"]; ok {
			var errMap map[string]json.RawMessage
			if err := json.Unmarshal(errRaw, &errMap); err == nil {
				if msgRaw, ok := errMap["message"]; ok {
					json.Unmarshal(msgRaw, &event.ErrorMessage) //nolint:errcheck
				}
			}
		}
	}

	return event, nil
}

// marshalAnthropicRequest serializes a CreateMessageRequest for the Anthropic API.
// When SystemBlocks is populated, it marshals the "system" field as an array of
// content blocks (required for prompt caching) instead of a plain string.
func marshalAnthropicRequest(req CreateMessageRequest) ([]byte, error) {
	if len(req.SystemBlocks) == 0 {
		return json.Marshal(req)
	}

	// Wire type with System as json.RawMessage for the array form.
	// SYNC: fields must match CreateMessageRequest (except System type).
	type wireRequest struct {
		Model            string          `json:"model"`
		MaxTokens        int             `json:"max_tokens"`
		System           json.RawMessage `json:"system,omitempty"`
		Messages         []Message       `json:"messages"`
		Tools            []Tool          `json:"tools,omitempty"`
		ToolChoice       *ToolChoice     `json:"tool_choice,omitempty"`
		Stream           bool            `json:"stream"`
		Temperature      *float64        `json:"temperature,omitempty"`
		TopP             *float64        `json:"top_p,omitempty"`
		FrequencyPenalty *float64        `json:"frequency_penalty,omitempty"`
		PresencePenalty  *float64        `json:"presence_penalty,omitempty"`
		Stop             []string        `json:"stop,omitempty"`
		ReasoningEffort  string          `json:"reasoning_effort,omitempty"`
	}

	systemJSON, err := json.Marshal(req.SystemBlocks)
	if err != nil {
		return nil, fmt.Errorf("marshal system blocks: %w", err)
	}

	wire := wireRequest{
		Model:            req.Model,
		MaxTokens:        req.MaxTokens,
		System:           systemJSON,
		Messages:         req.Messages,
		Tools:            req.Tools,
		ToolChoice:       req.ToolChoice,
		Stream:           req.Stream,
		Temperature:      req.Temperature,
		TopP:             req.TopP,
		FrequencyPenalty: req.FrequencyPenalty,
		PresencePenalty:  req.PresencePenalty,
		Stop:             req.Stop,
		ReasoningEffort:  req.ReasoningEffort,
	}
	return json.Marshal(wire)
}

// isRetryableStatus returns true for HTTP status codes that indicate a
// transient error suitable for retry (408, 429, and 5xx).
func isRetryableStatus(code int) bool {
	return code == 408 || code == 429 || code >= 500
}

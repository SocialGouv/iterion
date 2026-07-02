package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/SocialGouv/claw-code-go/internal/apikit"
)

const (
	defaultBaseURL      = "https://api.anthropic.com"
	anthropicVersion    = "2023-06-01"
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
	// anthropicOAuthBetaValue is the beta token the Anthropic API requires to
	// accept an OAuth *bearer* token on /v1/messages (the Claude Code
	// subscription "forfait" path). Without it an OAuth request 401s with
	// "x-api-key header is required". It is sent INSTEAD OF anthropicBetaValue
	// for bearer sessions — OAuth sessions reject the caching betas (see the
	// beta-header branch in StreamResponse).
	//
	// DEV-PURPOSE ONLY, AND EFFECTIVELY UNUSABLE. Reusing the Claude Code
	// subscription token from this (non-Claude-Code) client is outside
	// Anthropic's Consumer Terms. In practice it authenticates but the forfait
	// rate-limiter throttles non-Claude-Code clients to ~zero: requests 429
	// immediately (rate_limit_error, no Retry-After), even with fresh daily
	// quota, whereas the official `claude` CLI on the same token+model works.
	// The gap is the Claude Code client identity (User-Agent / client headers),
	// which this client deliberately does NOT spoof BY DEFAULT (it sends the
	// honest "claw-code-go/<version>" identity; see identity.go — the
	// operator can override it via UserAgent/CLAW_USER_AGENT/
	// ANTHROPIC_CUSTOM_HEADERS, a decision between the operator and the
	// endpoint they target). Use an API key or another provider for real
	// work; this path exists only so a local dev box without an
	// ANTHROPIC_API_KEY can still authenticate for experiments.
	anthropicOAuthBetaValue = "oauth-2025-04-20"

	// defaultMaxRetries is the maximum number of retry attempts for retryable
	// HTTP errors (429, 5xx). The first attempt is attempt 1.
	defaultMaxRetries = 3

	// retryBaseDelay is the initial backoff delay between retries.
	retryBaseDelay = 500 * time.Millisecond

	// maxRetryDelay caps how long a single retry wait can be, so honoring a
	// server-sent Retry-After can never block one request for minutes. The
	// documented, service-respecting behavior on 429/503 is to wait the delay
	// the server returns; this cap just bounds a pathological value.
	maxRetryDelay = 60 * time.Second
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

	// UserAgent overrides the User-Agent header; empty resolves via
	// ResolveUserAgent (CLAW_USER_AGENT env, then "claw-code-go/<version>").
	UserAgent string
	// ExtraHeaders are applied last on every request (override wins),
	// merged over the ANTHROPIC_CUSTOM_HEADERS environment variable.
	ExtraHeaders map[string]string
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

	// Resolve the client identity once per call (not per retry attempt).
	// Resolution is lazy — here rather than in NewClient — so callers that
	// construct a Client directly (bypassing the provider adapters) still
	// get the identity + env-override behaviour.
	identity, err := ResolveIdentity(c.UserAgent, DefaultUserAgent(), c.ExtraHeaders)
	if err != nil {
		return nil, err
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
		// Only send the comma-joined anthropic-beta header on direct
		// API-key sessions. OAuth-authenticated sessions (Claude Code
		// subscription path) historically reject newer / unrecognised
		// beta tokens with 400, and a single rejected token causes
		// the whole header to fail — every request 400s instead of
		// degrading gracefully to no caching. The features the betas
		// gate (prompt-caching) are still available via the
		// `cache_control` block on individual content items; turning
		// off the global opt-in just disables the new caching scope
		// the more-recent beta enables.
		if c.Auth.Kind != AuthSourceBearer && c.OAuthToken == "" {
			httpReq.Header.Set(anthropicBetaHeader, anthropicBetaValue)
		} else {
			// OAuth/Bearer session (Claude Code subscription forfait,
			// dev-purpose): the API only accepts the bearer token on
			// /v1/messages when the oauth beta header is present — omitting
			// it 401s with "x-api-key header is required". Send ONLY the
			// oauth token, not the caching betas (which OAuth sessions
			// reject with 400, per the comment above).
			httpReq.Header.Set(anthropicBetaHeader, anthropicOAuthBetaValue)
		}
		httpReq.Header.Set("content-type", "application/json")
		httpReq.Header.Set("accept", "text/event-stream")
		// Identity last, so ExtraHeaders (ANTHROPIC_CUSTOM_HEADERS parity)
		// can override any default header, including the User-Agent.
		identity.Apply(httpReq.Header)

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

		// Backoff before next attempt. Honor a server-sent Retry-After
		// header (429/503) — the documented, service-respecting behavior —
		// falling back to exponential backoff when absent. Capped by
		// maxRetryDelay so a pathological value can't block indefinitely.
		delay := retryBaseDelay * time.Duration(1<<(attempt-1))
		if ra := retryAfterDelay(resp.Header); ra > 0 {
			delay = ra
		}
		if delay > maxRetryDelay {
			delay = maxRetryDelay
		}
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
			if thinkingRaw, ok := deltaMap["thinking"]; ok {
				json.Unmarshal(thinkingRaw, &event.Delta.Thinking) //nolint:errcheck
			}
			if sigRaw, ok := deltaMap["signature"]; ok {
				json.Unmarshal(sigRaw, &event.Delta.Signature) //nolint:errcheck
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
//
// Strips Message.IsInjected from the wire payload — that field is the
// internal-only marker used by CompactSession's turn counter; sending
// it to Anthropic risks future strict-mode rejection of unknown fields
// (F-CC-7). Persistence still keeps the field since on-disk Session
// blobs are serialised separately by callers.
// marshalAnthropicRequest serializes a CreateMessageRequest into the Anthropic
// Messages API wire body, shaping it per the model's capabilities (registry-
// driven, see apikit.AnthropicProfile):
//   - effort is sent as output_config.effort — Anthropic does not read the
//     top-level reasoning_effort field (that is for OpenAI/Foundry);
//   - extended thinking follows the model's ThinkingMode (adaptive by default
//     on Opus 4.8/4.7/4.6 and Sonnet 4.6), with manual budgets coerced to
//     adaptive where required and an "off" sentinel suppressing the default;
//   - temperature/top_p are omitted on models that reject them (400), and
//     frequency/presence penalties are never forwarded (not Messages API params).
func marshalAnthropicRequest(req CreateMessageRequest) ([]byte, error) {
	wireMessages := stripInternalFields(req.Messages)

	// System: array form when SystemBlocks carry cache markers, else the
	// plain string. Empty when neither is set (omitted by omitempty).
	var systemJSON json.RawMessage
	if len(req.SystemBlocks) > 0 {
		b, err := json.Marshal(req.SystemBlocks)
		if err != nil {
			return nil, fmt.Errorf("marshal system blocks: %w", err)
		}
		systemJSON = b
	} else if req.System != "" {
		b, err := json.Marshal(req.System)
		if err != nil {
			return nil, fmt.Errorf("marshal system: %w", err)
		}
		systemJSON = b
	}

	profile := apikit.AnthropicProfile(req.Model)

	// Effort → output_config.effort, only when the model accepts this exact
	// level. AcceptsEffort checks the model's matrix (resolved in the single
	// AnthropicProfile lookup above), so a non-effort token — e.g. an
	// on/off/stream reasoning *mode* that shares the ReasoningEffort field in
	// the interactive loop — never reaches the wire.
	var outputConfig *OutputConfig
	if req.ReasoningEffort != "" && profile.AcceptsEffort(req.ReasoningEffort) {
		outputConfig = &OutputConfig{Effort: req.ReasoningEffort}
	}

	// Thinking: explicit request wins; otherwise apply the model default.
	thinking := req.Thinking
	if thinking == nil && profile.ThinkingMode == "adaptive" {
		thinking = &ThinkingConfig{Type: "adaptive"}
	}
	if thinking != nil {
		switch {
		case thinking.Type == "off":
			thinking = nil // sentinel: suppress the model default
		case profile.ThinkingMode == "adaptive" && thinking.Type == "enabled":
			thinking = &ThinkingConfig{Type: "adaptive"} // manual budgets 400 here
		}
	}

	// Sampling params the model rejects must be omitted.
	temperature := req.Temperature
	topP := req.TopP
	if profile.RejectsSampling {
		temperature = nil
		topP = nil
	}

	type wireRequest struct {
		Model        string          `json:"model"`
		MaxTokens    int             `json:"max_tokens"`
		System       json.RawMessage `json:"system,omitempty"`
		Messages     []Message       `json:"messages"`
		Tools        []Tool          `json:"tools,omitempty"`
		ToolChoice   *ToolChoice     `json:"tool_choice,omitempty"`
		Stream       bool            `json:"stream"`
		Temperature  *float64        `json:"temperature,omitempty"`
		TopP         *float64        `json:"top_p,omitempty"`
		Stop         []string        `json:"stop,omitempty"`
		OutputConfig *OutputConfig   `json:"output_config,omitempty"`
		Thinking     *ThinkingConfig `json:"thinking,omitempty"`
	}

	wire := wireRequest{
		Model:        req.Model,
		MaxTokens:    req.MaxTokens,
		System:       systemJSON,
		Messages:     wireMessages,
		Tools:        req.Tools,
		ToolChoice:   req.ToolChoice,
		Stream:       req.Stream,
		Temperature:  temperature,
		TopP:         topP,
		Stop:         req.Stop,
		OutputConfig: outputConfig,
		Thinking:     thinking,
	}
	return json.Marshal(wire)
}

// isRetryableStatus returns true for HTTP status codes that indicate a
// transient error suitable for retry (408, 429, and 5xx).
func isRetryableStatus(code int) bool {
	return code == 408 || code == 429 || code >= 500
}

// retryAfterDelay parses a Retry-After response header into a wait duration.
// Per RFC 7231 the value is either an integer count of seconds or an HTTP-date;
// both forms are honored. Returns 0 when the header is absent or unparseable
// (caller then uses its own backoff). A past HTTP-date yields 0.
func retryAfterDelay(h http.Header) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// stripInternalFields returns a shallow copy of in with internal-only
// Message fields cleared so they don't leak onto the Anthropic wire
// (F-CC-7). The original slice is not mutated, so callers retain the
// persisted shape with IsInjected for their own bookkeeping.
//
// Returns nil for a nil input (preserving omitempty semantics on the
// caller side).
func stripInternalFields(in []Message) []Message {
	if in == nil {
		return nil
	}
	out := make([]Message, len(in))
	for i, m := range in {
		m.IsInjected = false
		out[i] = m
	}
	return out
}

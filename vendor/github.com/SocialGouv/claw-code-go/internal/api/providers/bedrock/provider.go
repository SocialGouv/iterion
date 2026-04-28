// Package bedrock implements the AWS Bedrock provider using the Anthropic
// "Messages" payload shape via InvokeModelWithResponseStream.
//
// Authentication: standard AWS SDK chain — env vars (AWS_ACCESS_KEY_ID,
// AWS_SECRET_ACCESS_KEY, AWS_SESSION_TOKEN, AWS_REGION), shared profile
// (~/.aws/credentials), and EC2/ECS instance metadata.
//
// Scope: only Anthropic-family Bedrock model IDs are supported; the wire
// payload mirrors Anthropic's direct API (same fields, but the request
// drops "model" — the model goes in the URL — and adds "anthropic_version").
// Cross-region inference profile IDs (e.g. "us.anthropic...") and provisioned
// throughput ARNs both work because they are passed through as the model ID.
package bedrock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/SocialGouv/claw-code-go/internal/api"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	bedrocktypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	smithy "github.com/aws/smithy-go"
)

// bedrockAnthropicVersion is the value Bedrock requires inside the body for
// Anthropic models. It is a Bedrock-flavoured constant, distinct from the
// "anthropic-version" HTTP header used by the direct API.
const bedrockAnthropicVersion = "bedrock-2023-05-31"

// Provider implements api.Provider for AWS Bedrock.
type Provider struct{}

// New returns a new Bedrock Provider.
func New() *Provider { return &Provider{} }

// Name returns the provider identifier.
func (p *Provider) Name() string { return "bedrock" }

// AuthMethod returns the AWS IAM auth method used by Bedrock.
func (p *Provider) AuthMethod() api.AuthMethod { return api.AuthMethodIAM }

// NewClient creates a Bedrock streaming client. cfg.Model must be a Bedrock
// model identifier — typically an Anthropic ID like
// "anthropic.claude-sonnet-4-20250514-v1:0" or a cross-region inference
// profile such as "us.anthropic.claude-sonnet-4-20250514-v1:0". A leading
// "bedrock/" prefix (used by some routing layers) is stripped.
//
// Authentication is delegated to the AWS SDK default credentials chain.
// cfg.APIKey/OAuthToken/BaseURL are ignored — there is no API key for
// Bedrock. cfg.BaseURL is ignored because the SDK builds the endpoint from
// the resolved region.
func (p *Provider) NewClient(cfg api.ProviderConfig) (api.APIClient, error) {
	model := normalizeModelID(cfg.Model)
	if model == "" {
		return nil, fmt.Errorf("bedrock provider: cfg.Model is required (e.g. anthropic.claude-sonnet-4-20250514-v1:0)")
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background())
	if err != nil {
		return nil, fmt.Errorf("bedrock: load AWS config: %w", err)
	}

	return &Client{
		Model:     model,
		MaxTokens: cfg.MaxTokens,
		Bedrock:   bedrockruntime.NewFromConfig(awsCfg),
	}, nil
}

// MapModelID is the identity for Bedrock — the caller is expected to pass a
// fully qualified Bedrock model ID. We only strip a leading "bedrock/"
// prefix that some provider-routing schemes prepend.
func MapModelID(model string) string { return normalizeModelID(model) }

// normalizeModelID strips a "bedrock/" prefix and trims surrounding spaces.
func normalizeModelID(model string) string {
	model = strings.TrimSpace(model)
	model = strings.TrimPrefix(model, "bedrock/")
	return model
}

// invoker is the subset of bedrockruntime.Client used by Client. Allowing
// callers to substitute a fake makes unit tests possible without an AWS
// account.
type invoker interface {
	InvokeModelWithResponseStream(
		ctx context.Context,
		params *bedrockruntime.InvokeModelWithResponseStreamInput,
		optFns ...func(*bedrockruntime.Options),
	) (*bedrockruntime.InvokeModelWithResponseStreamOutput, error)
}

// Client is the Bedrock streaming client. It implements api.APIClient.
type Client struct {
	Model     string
	MaxTokens int
	Bedrock   invoker
}

// StreamResponse marshals req into a Bedrock-flavoured Anthropic body, calls
// InvokeModelWithResponseStream, and translates the resulting EventStream
// into Anthropic-shaped api.StreamEvent values.
func (c *Client) StreamResponse(ctx context.Context, req api.CreateMessageRequest) (<-chan api.StreamEvent, error) {
	req.Stream = true

	body, err := MarshalRequest(req)
	if err != nil {
		return nil, fmt.Errorf("bedrock: marshal request: %w", err)
	}

	out, err := c.Bedrock.InvokeModelWithResponseStream(ctx, &bedrockruntime.InvokeModelWithResponseStreamInput{
		ModelId:     aws.String(c.Model),
		ContentType: aws.String("application/json"),
		Accept:      aws.String("application/json"),
		Body:        body,
	})
	if err != nil {
		return nil, mapAWSError(err)
	}

	ch := make(chan api.StreamEvent, 64)

	go func() {
		defer close(ch)
		stream := out.GetStream()
		defer stream.Close()

		for ev := range stream.Events() {
			payload, errEvent := translateBedrockEvent(ev)
			if errEvent != nil {
				select {
				case ch <- api.StreamEvent{Type: api.EventError, ErrorMessage: errEvent.Error()}:
				case <-ctx.Done():
					return
				}
				continue
			}
			if payload == nil {
				continue
			}
			select {
			case <-ctx.Done():
				return
			case ch <- *payload:
			}
		}

		// Surface any error the stream accumulated mid-flight (network drop,
		// validation reported via ResponseStreamGetReader, etc.).
		if err := stream.Err(); err != nil {
			mapped := mapAWSError(err)
			select {
			case ch <- api.StreamEvent{Type: api.EventError, ErrorMessage: mapped.Error()}:
			case <-ctx.Done():
			}
		}
	}()

	return ch, nil
}

// translateBedrockEvent decodes a single ResponseStream member into an
// api.StreamEvent. Bedrock wraps each Anthropic event JSON inside a
// PayloadPart for Anthropic models. Non-payload members carry typed
// modelling errors — we surface those as EventError frames.
func translateBedrockEvent(ev bedrocktypes.ResponseStream) (*api.StreamEvent, error) {
	switch v := ev.(type) {
	case *bedrocktypes.ResponseStreamMemberChunk:
		if len(v.Value.Bytes) == 0 {
			return nil, nil
		}
		return decodeAnthropicJSON(v.Value.Bytes)
	default:
		// types.UnknownUnionMember or future variants
		return nil, fmt.Errorf("bedrock: unsupported event variant %T", ev)
	}
}

// decodeAnthropicJSON parses a single Anthropic-shape event JSON object.
// It mirrors the field handling in api.parseSSEData — kept private to that
// package — so this is a pragmatic duplicate. Keep them in sync.
func decodeAnthropicJSON(data []byte) (*api.StreamEvent, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("bedrock: decode event: %w", err)
	}

	var event api.StreamEvent

	if typeRaw, ok := raw["type"]; ok {
		var t string
		if err := json.Unmarshal(typeRaw, &t); err == nil {
			event.Type = api.StreamEventType(t)
		}
	}

	// Filter ping events as the SSE parser does.
	if event.Type == api.EventPing {
		return nil, nil
	}

	if indexRaw, ok := raw["index"]; ok {
		_ = json.Unmarshal(indexRaw, &event.Index)
	}

	if deltaRaw, ok := raw["delta"]; ok {
		var deltaMap map[string]json.RawMessage
		if err := json.Unmarshal(deltaRaw, &deltaMap); err == nil {
			if t, ok := deltaMap["type"]; ok {
				_ = json.Unmarshal(t, &event.Delta.Type)
			}
			if t, ok := deltaMap["text"]; ok {
				_ = json.Unmarshal(t, &event.Delta.Text)
			}
			if t, ok := deltaMap["partial_json"]; ok {
				_ = json.Unmarshal(t, &event.Delta.PartialJSON)
			}
			if t, ok := deltaMap["stop_reason"]; ok {
				var stop string
				if err := json.Unmarshal(t, &stop); err == nil {
					event.StopReason = stop
				}
			}
		}
	}

	if cbRaw, ok := raw["content_block"]; ok {
		var cbMap map[string]json.RawMessage
		if err := json.Unmarshal(cbRaw, &cbMap); err == nil {
			if t, ok := cbMap["type"]; ok {
				_ = json.Unmarshal(t, &event.ContentBlock.Type)
			}
			if t, ok := cbMap["id"]; ok {
				_ = json.Unmarshal(t, &event.ContentBlock.ID)
			}
			if t, ok := cbMap["name"]; ok {
				_ = json.Unmarshal(t, &event.ContentBlock.Name)
			}
		}
		event.ContentBlock.Index = event.Index
	}

	if usageRaw, ok := raw["usage"]; ok {
		_ = json.Unmarshal(usageRaw, &event.Usage)
	}

	if msgRaw, ok := raw["message"]; ok {
		var msgMap map[string]json.RawMessage
		if err := json.Unmarshal(msgRaw, &msgMap); err == nil {
			if u, ok := msgMap["usage"]; ok {
				var usage struct {
					InputTokens              int `json:"input_tokens"`
					CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
					CacheReadInputTokens     int `json:"cache_read_input_tokens"`
				}
				if err := json.Unmarshal(u, &usage); err == nil {
					event.InputTokens = usage.InputTokens
					event.CacheCreationInputTokens = usage.CacheCreationInputTokens
					event.CacheReadInputTokens = usage.CacheReadInputTokens
				}
			}
		}
	}

	if event.Type == api.EventError {
		if errRaw, ok := raw["error"]; ok {
			var errMap map[string]json.RawMessage
			if err := json.Unmarshal(errRaw, &errMap); err == nil {
				if m, ok := errMap["message"]; ok {
					_ = json.Unmarshal(m, &event.ErrorMessage)
				}
			}
		}
	}

	return &event, nil
}

// MarshalRequest serialises a CreateMessageRequest into the Bedrock JSON body
// required for Anthropic models. The function is exported for tests.
//
// Differences vs the Anthropic direct body:
//   - "model" is omitted (the model goes in the InvokeModel URL).
//   - "stream" is omitted (streaming is implicit for the Stream API).
//   - "anthropic_version" is required.
func MarshalRequest(req api.CreateMessageRequest) ([]byte, error) {
	type wire struct {
		AnthropicVersion string             `json:"anthropic_version"`
		MaxTokens        int                `json:"max_tokens"`
		System           json.RawMessage    `json:"system,omitempty"`
		Messages         []api.Message      `json:"messages"`
		Tools            []api.Tool         `json:"tools,omitempty"`
		ToolChoice       *api.ToolChoice    `json:"tool_choice,omitempty"`
		Temperature      *float64           `json:"temperature,omitempty"`
		TopP             *float64           `json:"top_p,omitempty"`
		Stop             []string           `json:"stop_sequences,omitempty"`
	}

	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = 8096
	}

	w := wire{
		AnthropicVersion: bedrockAnthropicVersion,
		MaxTokens:        maxTokens,
		Messages:         req.Messages,
		Tools:            req.Tools,
		ToolChoice:       req.ToolChoice,
		Temperature:      req.Temperature,
		TopP:             req.TopP,
		Stop:             req.Stop,
	}

	switch {
	case len(req.SystemBlocks) > 0:
		raw, err := json.Marshal(req.SystemBlocks)
		if err != nil {
			return nil, fmt.Errorf("marshal system blocks: %w", err)
		}
		w.System = raw
	case req.System != "":
		raw, err := json.Marshal(req.System)
		if err != nil {
			return nil, fmt.Errorf("marshal system: %w", err)
		}
		w.System = raw
	}

	return json.Marshal(w)
}

// mapAWSError converts an SDK error into *api.APIError with provider/status
// classification matching the documented Bedrock error taxonomy. The mapping
// is conservative: anything we cannot identify becomes a 500 (retryable).
func mapAWSError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, io.EOF) {
		return err
	}

	provider := "bedrock"

	// Typed Bedrock modelling errors — these carry the most precise signal.
	{
		var e *bedrocktypes.ThrottlingException
		if errors.As(err, &e) {
			return &api.APIError{Provider: provider, StatusCode: 429, Message: e.ErrorMessage(), Body: e.Error(), Retryable: true}
		}
	}
	{
		var e *bedrocktypes.ValidationException
		if errors.As(err, &e) {
			return &api.APIError{Provider: provider, StatusCode: 400, Message: e.ErrorMessage(), Body: e.Error(), Retryable: false}
		}
	}
	{
		var e *bedrocktypes.AccessDeniedException
		if errors.As(err, &e) {
			return &api.APIError{Provider: provider, StatusCode: 403, Message: e.ErrorMessage(), Body: e.Error(), Retryable: false}
		}
	}
	{
		var e *bedrocktypes.ResourceNotFoundException
		if errors.As(err, &e) {
			return &api.APIError{Provider: provider, StatusCode: 404, Message: e.ErrorMessage(), Body: e.Error(), Retryable: false}
		}
	}
	{
		var e *bedrocktypes.ServiceQuotaExceededException
		if errors.As(err, &e) {
			return &api.APIError{Provider: provider, StatusCode: 429, Message: e.ErrorMessage(), Body: e.Error(), Retryable: true}
		}
	}
	{
		var e *bedrocktypes.ModelTimeoutException
		if errors.As(err, &e) {
			return &api.APIError{Provider: provider, StatusCode: 504, Message: e.ErrorMessage(), Body: e.Error(), Retryable: true}
		}
	}
	{
		var e *bedrocktypes.ModelStreamErrorException
		if errors.As(err, &e) {
			return &api.APIError{Provider: provider, StatusCode: 500, Message: e.ErrorMessage(), Body: e.Error(), Retryable: true}
		}
	}
	{
		var e *bedrocktypes.ModelErrorException
		if errors.As(err, &e) {
			return &api.APIError{Provider: provider, StatusCode: 500, Message: e.ErrorMessage(), Body: e.Error(), Retryable: true}
		}
	}
	{
		var e *bedrocktypes.ModelNotReadyException
		if errors.As(err, &e) {
			return &api.APIError{Provider: provider, StatusCode: 503, Message: e.ErrorMessage(), Body: e.Error(), Retryable: true}
		}
	}
	{
		var e *bedrocktypes.InternalServerException
		if errors.As(err, &e) {
			return &api.APIError{Provider: provider, StatusCode: 500, Message: e.ErrorMessage(), Body: e.Error(), Retryable: true}
		}
	}

	// Generic smithy API error — classify by code where possible.
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		code := apiErr.ErrorCode()
		status := classifySmithyCode(code)
		return &api.APIError{
			Provider:   provider,
			StatusCode: status,
			Message:    apiErr.ErrorMessage(),
			Body:       apiErr.Error(),
			Retryable:  api.IsRetryableStatus(status),
		}
	}

	// Unknown — surface as a retryable 500 so callers don't lose retry hints.
	return &api.APIError{
		Provider:   provider,
		StatusCode: 500,
		Message:    err.Error(),
		Body:       err.Error(),
		Retryable:  true,
	}
}

// classifySmithyCode maps a Bedrock/AWS error code string to an HTTP status.
func classifySmithyCode(code string) int {
	switch code {
	case "ThrottlingException", "TooManyRequestsException", "ServiceQuotaExceededException":
		return 429
	case "ValidationException", "InvalidRequestException":
		return 400
	case "AccessDeniedException", "UnrecognizedClientException":
		return 403
	case "ResourceNotFoundException":
		return 404
	case "ModelTimeoutException", "RequestTimeoutException":
		return 504
	case "ModelNotReadyException", "ServiceUnavailableException":
		return 503
	case "ModelStreamErrorException", "ModelErrorException", "InternalServerException":
		return 500
	}
	return 500
}

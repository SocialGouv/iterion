package bedrock

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/internal/api"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	bedrocktypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
)

func TestNormalizeModelID(t *testing.T) {
	cases := map[string]string{
		"  bedrock/anthropic.claude-sonnet-4-20250514-v1:0  ": "anthropic.claude-sonnet-4-20250514-v1:0",
		"anthropic.claude-sonnet-4-20250514-v1:0":             "anthropic.claude-sonnet-4-20250514-v1:0",
		"us.anthropic.claude-sonnet-4-20250514-v1:0":          "us.anthropic.claude-sonnet-4-20250514-v1:0",
		"": "",
	}
	for in, want := range cases {
		if got := normalizeModelID(in); got != want {
			t.Errorf("normalizeModelID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMarshalRequestBasic(t *testing.T) {
	temp := 0.4
	req := api.CreateMessageRequest{
		Model:     "anthropic.claude-sonnet-4-20250514-v1:0", // ignored — model goes in URL
		MaxTokens: 1234,
		System:    "You are concise.",
		Messages: []api.Message{
			{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "hello"}}},
		},
		Temperature: &temp,
		Stop:        []string{"</done>"},
		Stream:      true, // must not appear in body
	}

	body, err := MarshalRequest(req)
	if err != nil {
		t.Fatalf("MarshalRequest: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}

	if v, _ := got["anthropic_version"].(string); v != bedrockAnthropicVersion {
		t.Errorf("anthropic_version = %q, want %q", v, bedrockAnthropicVersion)
	}
	if v, _ := got["max_tokens"].(float64); int(v) != 1234 {
		t.Errorf("max_tokens = %v, want 1234", got["max_tokens"])
	}
	if v, _ := got["system"].(string); v != "You are concise." {
		t.Errorf("system = %v, want \"You are concise.\"", got["system"])
	}
	if _, ok := got["model"]; ok {
		t.Errorf("body must not contain \"model\" — bedrock takes it from the URL")
	}
	if _, ok := got["stream"]; ok {
		t.Errorf("body must not contain \"stream\" — implicit for InvokeModelWithResponseStream")
	}
	if v, _ := got["temperature"].(float64); v != 0.4 {
		t.Errorf("temperature = %v, want 0.4", got["temperature"])
	}
	stopSeqs, _ := got["stop_sequences"].([]any)
	if len(stopSeqs) != 1 || stopSeqs[0] != "</done>" {
		t.Errorf("stop_sequences = %v, want [</done>]", got["stop_sequences"])
	}
}

func TestMarshalRequestSystemBlocksTakePrecedence(t *testing.T) {
	req := api.CreateMessageRequest{
		MaxTokens: 100,
		System:    "ignored when SystemBlocks is set",
		SystemBlocks: []api.ContentBlock{
			{Type: "text", Text: "cached preamble", CacheControl: api.EphemeralCacheControl()},
		},
		Messages: []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "hi"}}}},
	}
	body, err := MarshalRequest(req)
	if err != nil {
		t.Fatalf("MarshalRequest: %v", err)
	}
	var got struct {
		System []map[string]any `json:"system"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.System) != 1 {
		t.Fatalf("system blocks length = %d, want 1; body=%s", len(got.System), body)
	}
	if got.System[0]["text"] != "cached preamble" {
		t.Errorf("system block text = %v, want \"cached preamble\"", got.System[0]["text"])
	}
	if cc, ok := got.System[0]["cache_control"].(map[string]any); !ok || cc["type"] != "ephemeral" {
		t.Errorf("cache_control = %v, want ephemeral marker", got.System[0]["cache_control"])
	}
}

func TestMarshalRequestDefaultMaxTokens(t *testing.T) {
	body, err := MarshalRequest(api.CreateMessageRequest{
		Messages: []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "x"}}}},
	})
	if err != nil {
		t.Fatalf("MarshalRequest: %v", err)
	}
	var got map[string]any
	_ = json.Unmarshal(body, &got)
	if v, _ := got["max_tokens"].(float64); int(v) == 0 {
		t.Errorf("max_tokens must default when zero, got %v", got["max_tokens"])
	}
}

func TestDecodeAnthropicJSONMessageStart(t *testing.T) {
	raw := []byte(`{
		"type": "message_start",
		"message": {
			"id": "msg_01",
			"usage": {
				"input_tokens": 42,
				"cache_creation_input_tokens": 7,
				"cache_read_input_tokens": 3
			}
		}
	}`)
	ev, err := decodeAnthropicJSON(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ev == nil || ev.Type != api.EventMessageStart {
		t.Fatalf("type = %v, want message_start", ev)
	}
	if ev.InputTokens != 42 || ev.CacheCreationInputTokens != 7 || ev.CacheReadInputTokens != 3 {
		t.Errorf("usage tokens mis-parsed: %+v", ev)
	}
}

func TestDecodeAnthropicJSONContentBlockDelta(t *testing.T) {
	raw := []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello "}}`)
	ev, err := decodeAnthropicJSON(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ev.Type != api.EventContentBlockDelta || ev.Index != 0 || ev.Delta.Text != "hello " || ev.Delta.Type != "text_delta" {
		t.Errorf("unexpected event: %+v", ev)
	}
}

func TestDecodeAnthropicJSONMessageDeltaStopReason(t *testing.T) {
	raw := []byte(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":17}}`)
	ev, err := decodeAnthropicJSON(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ev.Type != api.EventMessageDelta || ev.StopReason != "end_turn" || ev.Usage.OutputTokens != 17 {
		t.Errorf("unexpected event: %+v", ev)
	}
}

func TestDecodeAnthropicJSONPingFiltered(t *testing.T) {
	ev, err := decodeAnthropicJSON([]byte(`{"type":"ping"}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ev != nil {
		t.Errorf("ping must be filtered, got %+v", ev)
	}
}

func TestDecodeAnthropicJSONErrorEvent(t *testing.T) {
	raw := []byte(`{"type":"error","error":{"type":"overloaded_error","message":"backoff"}}`)
	ev, err := decodeAnthropicJSON(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ev.Type != api.EventError || ev.ErrorMessage != "backoff" {
		t.Errorf("unexpected event: %+v", ev)
	}
}

func TestTranslateBedrockEventChunk(t *testing.T) {
	chunk := &bedrocktypes.ResponseStreamMemberChunk{
		Value: bedrocktypes.PayloadPart{
			Bytes: []byte(`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"shell"}}`),
		},
	}
	ev, err := translateBedrockEvent(chunk)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if ev.Type != api.EventContentBlockStart || ev.ContentBlock.Name != "shell" || ev.ContentBlock.ID != "toolu_1" || ev.ContentBlock.Index != 1 {
		t.Errorf("unexpected event: %+v", ev)
	}
}

// fakeInvoker fails — used to confirm error mapping plumbing.
type fakeInvoker struct{ err error }

func (f *fakeInvoker) InvokeModelWithResponseStream(
	_ context.Context,
	_ *bedrockruntime.InvokeModelWithResponseStreamInput,
	_ ...func(*bedrockruntime.Options),
) (*bedrockruntime.InvokeModelWithResponseStreamOutput, error) {
	return nil, f.err
}

func TestStreamResponseMapsValidationException(t *testing.T) {
	c := &Client{
		Model:   "anthropic.claude-sonnet-4-20250514-v1:0",
		Bedrock: &fakeInvoker{err: &bedrocktypes.ValidationException{Message: stringPtr("bad input")}},
	}
	_, err := c.StreamResponse(context.Background(), api.CreateMessageRequest{
		MaxTokens: 10,
		Messages:  []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	var apiErr *api.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err is not *api.APIError: %T %v", err, err)
	}
	if apiErr.Provider != "bedrock" || apiErr.StatusCode != 400 || apiErr.Retryable {
		t.Errorf("unexpected APIError: %+v", apiErr)
	}
}

func TestStreamResponseMapsThrottling(t *testing.T) {
	c := &Client{
		Model:   "anthropic.claude-sonnet-4-20250514-v1:0",
		Bedrock: &fakeInvoker{err: &bedrocktypes.ThrottlingException{Message: stringPtr("slow down")}},
	}
	_, err := c.StreamResponse(context.Background(), api.CreateMessageRequest{
		MaxTokens: 10,
		Messages:  []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	var apiErr *api.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 429 || !apiErr.Retryable {
		t.Fatalf("unexpected: %+v", apiErr)
	}
	if !strings.Contains(apiErr.Error(), "bedrock") {
		t.Errorf("error string missing provider: %q", apiErr.Error())
	}
}

func stringPtr(s string) *string { return &s }

func TestProviderMetadata(t *testing.T) {
	p := New()
	if p.Name() != "bedrock" {
		t.Errorf("Name = %q, want bedrock", p.Name())
	}
	if p.AuthMethod() != api.AuthMethodIAM {
		t.Errorf("AuthMethod = %q, want iam", p.AuthMethod())
	}
}

func TestNewClientRequiresModel(t *testing.T) {
	// LoadDefaultConfig needs a region to mint a sensible client; force one
	// here so the test runs hermetically on environments without AWS at all.
	t.Setenv("AWS_REGION", "us-east-1")
	p := New()
	if _, err := p.NewClient(api.ProviderConfig{}); err == nil {
		t.Errorf("expected error for empty model")
	}
}

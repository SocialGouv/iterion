package openai

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/internal/api"
)

func TestIsReasoningModel(t *testing.T) {
	positive := []string{
		"o1", "o1-mini", "o3", "o3-mini", "o4-mini",
		"grok-3-mini",
		"qwen-qwq-32b", "qwq-32b", "qwq-plus",
		"qwen3-30b-a3b-thinking",
		"qwen/qwen-qwq-32b", // with provider prefix
		"qwen/qwen3-30b-a3b-thinking",
	}
	negative := []string{
		"gpt-4o", "gpt-5", "gpt-5.2",
		"claude-sonnet-4-6",
		"grok-3",
		"qwen-max", "qwen/qwen-plus", "qwen-turbo",
	}

	for _, model := range positive {
		if !isReasoningModel(model) {
			t.Errorf("isReasoningModel(%q) = false, want true", model)
		}
	}
	for _, model := range negative {
		if isReasoningModel(model) {
			t.Errorf("isReasoningModel(%q) = true, want false", model)
		}
	}
}

func TestStripRoutingPrefix(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"openai/gpt-4", "gpt-4"},
		{"xai/grok-3", "grok-3"},
		{"grok/grok-3-mini", "grok-3-mini"},
		{"qwen/qwen-plus", "qwen-plus"},
		{"unknown/model", "unknown/model"}, // unknown prefix: no strip
		{"gpt-4o", "gpt-4o"},               // no prefix
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := stripRoutingPrefix(tt.input)
			if got != tt.want {
				t.Errorf("stripRoutingPrefix(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestBuildRequest_ReasoningModelOmitsTuningParams(t *testing.T) {
	temp := 0.7
	topP := 0.9
	freqP := 0.5
	presP := 0.3

	client := &Client{Model: "o3-mini", MaxTokens: 1024}
	req := api.CreateMessageRequest{
		Model:            "o3-mini",
		MaxTokens:        1024,
		Temperature:      &temp,
		TopP:             &topP,
		FrequencyPenalty: &freqP,
		PresencePenalty:  &presP,
		Stop:             []string{"\n"},
	}

	oaiReq, err := client.buildRequest(req)
	if err != nil {
		t.Fatal(err)
	}

	data, err := json.Marshal(oaiReq)
	if err != nil {
		t.Fatal(err)
	}

	var payload map[string]interface{}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}

	// Reasoning model: tuning params must be absent
	for _, key := range []string{"temperature", "top_p", "frequency_penalty", "presence_penalty"} {
		if _, ok := payload[key]; ok {
			t.Errorf("reasoning model payload should not contain %q", key)
		}
	}

	// Stop is safe for all models
	if _, ok := payload["stop"]; !ok {
		t.Error("stop should be present even for reasoning models")
	}
}

func TestBuildRequest_NonReasoningModelIncludesTuningParams(t *testing.T) {
	temp := 0.7
	topP := 0.9
	freqP := 0.5
	presP := 0.3

	client := &Client{Model: "gpt-4o", MaxTokens: 1024}
	req := api.CreateMessageRequest{
		Model:            "gpt-4o",
		MaxTokens:        1024,
		Temperature:      &temp,
		TopP:             &topP,
		FrequencyPenalty: &freqP,
		PresencePenalty:  &presP,
		Stop:             []string{"\n"},
	}

	oaiReq, err := client.buildRequest(req)
	if err != nil {
		t.Fatal(err)
	}

	data, err := json.Marshal(oaiReq)
	if err != nil {
		t.Fatal(err)
	}

	var payload map[string]interface{}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}

	// Non-reasoning: all tuning params present
	for _, key := range []string{"temperature", "top_p", "frequency_penalty", "presence_penalty", "stop"} {
		if _, ok := payload[key]; !ok {
			t.Errorf("non-reasoning model payload should contain %q", key)
		}
	}

	// Verify values
	if v, ok := payload["temperature"].(float64); !ok || v != 0.7 {
		t.Errorf("temperature = %v, want 0.7", payload["temperature"])
	}
}

func TestBuildRequest_TuningParamsOmittedWhenNil(t *testing.T) {
	client := &Client{Model: "gpt-4o", MaxTokens: 1024}
	req := api.CreateMessageRequest{
		Model:     "gpt-4o",
		MaxTokens: 1024,
	}

	oaiReq, err := client.buildRequest(req)
	if err != nil {
		t.Fatal(err)
	}

	data, err := json.Marshal(oaiReq)
	if err != nil {
		t.Fatal(err)
	}

	var payload map[string]interface{}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}

	for _, key := range []string{"temperature", "top_p", "frequency_penalty", "presence_penalty", "stop"} {
		if _, ok := payload[key]; ok {
			t.Errorf("nil tuning param %q should be absent from payload", key)
		}
	}
}

func TestBuildRequest_GPT5UsesMaxCompletionTokens(t *testing.T) {
	client := &Client{Model: "gpt-5.2", MaxTokens: 512}
	req := api.CreateMessageRequest{
		Model:     "gpt-5.2",
		MaxTokens: 512,
	}

	oaiReq, err := client.buildRequest(req)
	if err != nil {
		t.Fatal(err)
	}

	data, err := json.Marshal(oaiReq)
	if err != nil {
		t.Fatal(err)
	}

	var payload map[string]interface{}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}

	if _, ok := payload["max_completion_tokens"]; !ok {
		t.Error("gpt-5.2 should use max_completion_tokens")
	}
	if v := payload["max_completion_tokens"].(float64); v != 512 {
		t.Errorf("max_completion_tokens = %v, want 512", v)
	}
	if _, ok := payload["max_tokens"]; ok {
		t.Error("gpt-5.2 must not emit max_tokens")
	}
}

func TestBuildRequest_NonGPT5UsesMaxTokens(t *testing.T) {
	client := &Client{Model: "gpt-4o", MaxTokens: 512}
	req := api.CreateMessageRequest{
		Model:     "gpt-4o",
		MaxTokens: 512,
	}

	oaiReq, err := client.buildRequest(req)
	if err != nil {
		t.Fatal(err)
	}

	data, err := json.Marshal(oaiReq)
	if err != nil {
		t.Fatal(err)
	}

	var payload map[string]interface{}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}

	if _, ok := payload["max_tokens"]; !ok {
		t.Error("gpt-4o should use max_tokens")
	}
	if _, ok := payload["max_completion_tokens"]; ok {
		t.Error("gpt-4o must not emit max_completion_tokens")
	}
}

func TestBuildRequest_StripsRoutingPrefix(t *testing.T) {
	client := &Client{Model: "openai/gpt-4", MaxTokens: 1024}
	req := api.CreateMessageRequest{
		Model:     "openai/gpt-4",
		MaxTokens: 1024,
	}

	oaiReq, err := client.buildRequest(req)
	if err != nil {
		t.Fatal(err)
	}

	if oaiReq.Model != "gpt-4" {
		t.Errorf("model = %q, want %q", oaiReq.Model, "gpt-4")
	}
}

func TestBuildRequest_ReasoningEffort(t *testing.T) {
	client := &Client{Model: "o4-mini", MaxTokens: 1024}
	req := api.CreateMessageRequest{
		Model:           "o4-mini",
		MaxTokens:       1024,
		ReasoningEffort: "high",
	}

	oaiReq, err := client.buildRequest(req)
	if err != nil {
		t.Fatal(err)
	}

	data, err := json.Marshal(oaiReq)
	if err != nil {
		t.Fatal(err)
	}

	var payload map[string]interface{}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}

	if v, ok := payload["reasoning_effort"]; !ok || v != "high" {
		t.Errorf("reasoning_effort = %v, want %q", v, "high")
	}
}

func TestStreamOptionsSkippedForXAI(t *testing.T) {
	// XAI uses a non-default base URL → stream_options must be absent.
	client := &Client{
		Model:   "grok-3",
		BaseURL: "https://api.x.ai",
	}
	req := api.CreateMessageRequest{
		Model:     "grok-3",
		MaxTokens: 1024,
	}

	oaiReq, err := client.buildRequest(req)
	if err != nil {
		t.Fatal(err)
	}

	data, err := json.Marshal(oaiReq)
	if err != nil {
		t.Fatal(err)
	}

	var payload map[string]interface{}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}

	if _, ok := payload["stream_options"]; ok {
		t.Error("XAI request should NOT include stream_options")
	}

	// Also verify the request struct directly
	if oaiReq.StreamOptions != nil {
		t.Error("StreamOptions should be nil for XAI")
	}
}

func TestStreamOptionsIncludedForOpenAI(t *testing.T) {
	// Default OpenAI base URL → stream_options must be present.
	client := &Client{
		Model:   "gpt-4o",
		BaseURL: "https://api.openai.com",
	}
	req := api.CreateMessageRequest{
		Model:     "gpt-4o",
		MaxTokens: 1024,
	}

	oaiReq, err := client.buildRequest(req)
	if err != nil {
		t.Fatal(err)
	}

	data, err := json.Marshal(oaiReq)
	if err != nil {
		t.Fatal(err)
	}

	var payload map[string]interface{}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}

	so, ok := payload["stream_options"]
	if !ok {
		t.Fatal("OpenAI request must include stream_options")
	}

	soMap, ok := so.(map[string]interface{})
	if !ok {
		t.Fatalf("stream_options should be object, got %T", so)
	}

	if v, ok := soMap["include_usage"]; !ok || v != true {
		t.Errorf("stream_options.include_usage = %v, want true", v)
	}
}

// TestConvertTools_PropagatesMarshalError pins the contract that tool
// conversion bubbles up json.Marshal failures rather than silently dropping
// the offending tool. Silently skipping a tool causes the model to emit
// tool_use calls for an undeclared name, which derails the conversation.
//
// We trigger a marshal failure by stuffing a chan into the Property.Enum
// slice (chan values are not JSON-marshallable). The chat-completions
// buildRequest path is exercised here.
func TestConvertTools_PropagatesMarshalError(t *testing.T) {
	client := &Client{Model: "gpt-4o", APIKey: "stub"}
	req := api.CreateMessageRequest{
		Model:     "gpt-4o",
		MaxTokens: 16,
		Messages: []api.Message{
			{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "hi"}}},
		},
		Tools: []api.Tool{
			{
				Name:        "broken",
				Description: "schema with unmarshallable enum",
				InputSchema: api.InputSchema{
					Type: "object",
					Properties: map[string]api.Property{
						"x": {Type: "string", Enum: []any{make(chan int)}},
					},
				},
			},
		},
	}

	if _, err := client.buildRequest(req); err == nil {
		t.Fatal("expected buildRequest to fail when a tool's input schema cannot be marshalled")
	} else if !strings.Contains(err.Error(), "broken") {
		t.Errorf("error %q should mention the offending tool name %q", err.Error(), "broken")
	}
}

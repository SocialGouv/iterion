package api

import (
	"encoding/json"
	"testing"
)

// decodeWire marshals a request through marshalAnthropicRequest and decodes
// the JSON body into a generic map for field assertions.
func decodeWire(t *testing.T, req CreateMessageRequest) map[string]any {
	t.Helper()
	body, err := marshalAnthropicRequest(req)
	if err != nil {
		t.Fatalf("marshalAnthropicRequest: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("unmarshal wire: %v", err)
	}
	return m
}

func TestMarshalAnthropicRequest_EffortAsOutputConfig(t *testing.T) {
	temp := 0.7
	m := decodeWire(t, CreateMessageRequest{
		Model:           "claude-opus-4-8",
		MaxTokens:       4096,
		Messages:        []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "hi"}}}},
		ReasoningEffort: "xhigh",
		Temperature:     &temp, // must be dropped (Opus 4.8 rejects sampling params)
	})

	// effort goes in output_config.effort, never top-level reasoning_effort.
	if _, ok := m["reasoning_effort"]; ok {
		t.Error("top-level reasoning_effort must not be sent to Anthropic")
	}
	oc, ok := m["output_config"].(map[string]any)
	if !ok {
		t.Fatalf("output_config missing or wrong type: %v", m["output_config"])
	}
	if oc["effort"] != "xhigh" {
		t.Errorf("output_config.effort = %v, want xhigh", oc["effort"])
	}

	// adaptive thinking enabled by default on Opus 4.8, with the readable
	// summary requested (Opus 4.8 defaults display to "omitted" — empty
	// thinking text — unless asked).
	th, ok := m["thinking"].(map[string]any)
	if !ok || th["type"] != "adaptive" {
		t.Errorf("thinking = %v, want {type: adaptive}", m["thinking"])
	}
	if th["display"] != "summarized" {
		t.Errorf("thinking.display = %v, want summarized (Opus 4.8 defaults to omitted)", th["display"])
	}

	// sampling params rejected by Opus 4.8 must be omitted.
	if _, ok := m["temperature"]; ok {
		t.Error("temperature must be omitted on Opus 4.8")
	}
}

func TestMarshalAnthropicRequest_ThinkingDisplayCallerAndEnvOverride(t *testing.T) {
	// An explicit caller Display wins over the default.
	m := decodeWire(t, CreateMessageRequest{
		Model:     "claude-opus-4-8",
		MaxTokens: 4096,
		Messages:  []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "hi"}}}},
		Thinking:  &ThinkingConfig{Type: "adaptive", Display: "omitted"},
	})
	th, _ := m["thinking"].(map[string]any)
	if th["display"] != "omitted" {
		t.Errorf("thinking.display = %v, want caller's omitted", th["display"])
	}

	// CLAW_ANTHROPIC_THINKING_DISPLAY=off stops sending the field.
	t.Setenv("CLAW_ANTHROPIC_THINKING_DISPLAY", "off")
	m = decodeWire(t, CreateMessageRequest{
		Model:     "claude-opus-4-8",
		MaxTokens: 4096,
		Messages:  []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	th, _ = m["thinking"].(map[string]any)
	if _, ok := th["display"]; ok {
		t.Errorf("thinking.display = %v, want absent under the off kill-switch", th["display"])
	}
}

func TestMarshalAnthropicRequest_ThinkingOffSentinel(t *testing.T) {
	m := decodeWire(t, CreateMessageRequest{
		Model:     "claude-opus-4-8",
		MaxTokens: 4096,
		Messages:  []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "hi"}}}},
		Thinking:  &ThinkingConfig{Type: "off"},
	})
	if _, ok := m["thinking"]; ok {
		t.Error(`Thinking{Type:"off"} must suppress the thinking field`)
	}
}

func TestMarshalAnthropicRequest_ManualBudgetCoercedToAdaptive(t *testing.T) {
	m := decodeWire(t, CreateMessageRequest{
		Model:     "claude-opus-4-8",
		MaxTokens: 4096,
		Messages:  []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "hi"}}}},
		Thinking:  &ThinkingConfig{Type: "enabled", BudgetTokens: 32000},
	})
	th, ok := m["thinking"].(map[string]any)
	if !ok || th["type"] != "adaptive" {
		t.Fatalf("thinking = %v, want adaptive (manual budgets 400 on Opus 4.8)", m["thinking"])
	}
	if _, ok := th["budget_tokens"]; ok {
		t.Error("budget_tokens must not be sent on an adaptive-only model")
	}
}

func TestMarshalAnthropicRequest_NonEffortModelOmitsOutputConfig(t *testing.T) {
	// Haiku supports neither effort nor adaptive thinking.
	m := decodeWire(t, CreateMessageRequest{
		Model:           "claude-haiku-4-5",
		MaxTokens:       4096,
		Messages:        []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "hi"}}}},
		ReasoningEffort: "high",
	})
	if _, ok := m["output_config"]; ok {
		t.Error("output_config must be omitted for a model without an effort matrix")
	}
	if _, ok := m["thinking"]; ok {
		t.Error("thinking must be omitted for a model with no thinking mode")
	}
}

func TestMarshalAnthropicRequest_ReasoningModeTokenNotLeaked(t *testing.T) {
	// The interactive loop shares the ReasoningEffort field with the
	// on/off/stream reasoning *mode*; those must never reach the wire as
	// an effort value.
	m := decodeWire(t, CreateMessageRequest{
		Model:           "claude-opus-4-8",
		MaxTokens:       4096,
		Messages:        []Message{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "hi"}}}},
		ReasoningEffort: "on",
	})
	if _, ok := m["output_config"]; ok {
		t.Errorf("non-effort token must not produce output_config: %v", m["output_config"])
	}
}

// The exact billed thinking-token count rides message_delta usage as
// output_tokens_details.thinking_tokens; parseSSEData must surface it.
func TestParseSSEData_UsageThinkingTokens(t *testing.T) {
	ev, err := parseSSEData(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":348,"output_tokens_details":{"thinking_tokens":312}}}`)
	if err != nil {
		t.Fatalf("parseSSEData: %v", err)
	}
	if ev.Usage.OutputTokens != 348 {
		t.Errorf("OutputTokens = %d, want 348", ev.Usage.OutputTokens)
	}
	if ev.Usage.OutputTokensDetails.ThinkingTokens != 312 {
		t.Errorf("ThinkingTokens = %d, want 312", ev.Usage.OutputTokensDetails.ThinkingTokens)
	}
}

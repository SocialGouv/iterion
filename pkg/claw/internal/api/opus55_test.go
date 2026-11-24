package api

import "testing"

func TestOpus55WireProfile(t *testing.T) {
	temperature := 0.2
	m := decodeWire(t, CreateMessageRequest{Model: "claude-opus-5-5", MaxTokens: 4096,
		ReasoningEffort: "high", Temperature: &temperature})
	if _, ok := m["temperature"]; ok {
		t.Error("Opus5.5 rejects sampling parameters")
	}
	if th, ok := m["thinking"].(map[string]any); !ok || th["type"] != "adaptive" || th["display"] != "summarized" {
		t.Errorf("thinking = %v, want adaptive with visible summaries", m["thinking"])
	}
	if out, ok := m["output_config"].(map[string]any); !ok || out["effort"] != "high" {
		t.Errorf("output_config = %v, lost explicit effort", m["output_config"])
	}
}

func TestOpus55RejectsIncompatibleControls(t *testing.T) {
	for _, choice := range []string{"any", "tool"} {
		if _, err := marshalAnthropicRequest(CreateMessageRequest{Model: "claude-opus-5-5", ToolChoice: &ToolChoice{Type: choice, Name: "output"}}); err == nil {
			t.Errorf("forced tool choice %q should be refused before sending", choice)
		}
	}
	for _, thinking := range []string{"off", "disabled", "enabled"} {
		if _, err := marshalAnthropicRequest(CreateMessageRequest{Model: "claude-opus-5-5", Thinking: &ThinkingConfig{Type: thinking}}); err == nil {
			t.Errorf("thinking=%s should be refused before sending", thinking)
		}
	}
	// Explicit older-model choices retain their existing contract.
	if _, err := marshalAnthropicRequest(CreateMessageRequest{Model: "claude-opus-5", Thinking: &ThinkingConfig{Type: "off"}, ToolChoice: &ToolChoice{Type: "tool", Name: "output"}}); err != nil {
		t.Fatal(err)
	}
}

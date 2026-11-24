package tools

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAgentExecute(t *testing.T) {
	input := map[string]any{
		"description": "Fix the login bug",
		"prompt":      "Find and fix the authentication issue in login.go",
	}
	result, err := ExecuteAgent(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if parsed["agent_id"] == nil || parsed["agent_id"] == "" {
		t.Error("expected non-empty agent_id")
	}
	if parsed["status"] != "running" {
		t.Errorf("expected status 'running', got %v", parsed["status"])
	}
	if parsed["model"] != defaultAgentModel {
		t.Errorf("expected model %q, got %v", defaultAgentModel, parsed["model"])
	}
	if parsed["subagent_type"] != "general-purpose" {
		t.Errorf("expected subagent_type 'general-purpose', got %v", parsed["subagent_type"])
	}
}

func TestAgentExecute_MissingDescription(t *testing.T) {
	input := map[string]any{
		"prompt": "Do something",
	}
	_, err := ExecuteAgent(input)
	if err == nil {
		t.Fatal("expected error for missing description")
	}
	if !strings.Contains(err.Error(), "description") {
		t.Errorf("expected error about 'description', got: %v", err)
	}
}

func TestAgentExecute_MissingPrompt(t *testing.T) {
	input := map[string]any{
		"description": "Some task",
	}
	_, err := ExecuteAgent(input)
	if err == nil {
		t.Fatal("expected error for missing prompt")
	}
	if !strings.Contains(err.Error(), "prompt") {
		t.Errorf("expected error about 'prompt', got: %v", err)
	}
}

func TestAllowedToolsForSubagent_Explore(t *testing.T) {
	tools := AllowedToolsForSubagent("explore")
	if tools == nil {
		t.Fatal("expected non-nil tool set for explore")
	}
	expected := []string{"read_file", "glob", "grep", "tool_search"}
	for _, name := range expected {
		if !tools[name] {
			t.Errorf("expected %q in explore tools", name)
		}
	}
	// Should NOT have write tools
	forbidden := []string{"bash", "todo_write"}
	for _, name := range forbidden {
		if tools[name] {
			t.Errorf("did not expect %q in explore tools", name)
		}
	}
}

func TestAllowedToolsForSubagent_General(t *testing.T) {
	tools := AllowedToolsForSubagent("general-purpose")
	if tools != nil {
		t.Errorf("expected nil (all allowed) for general-purpose, got %v", tools)
	}
}

func TestAllowedToolsForSubagent_PlanNoConfig(t *testing.T) {
	tools := AllowedToolsForSubagent("plan")
	if tools == nil {
		t.Fatal("expected non-nil tool set for plan")
	}
	// Plan should NOT include "config" (Rust parity).
	if tools["config"] {
		t.Error("plan subagent should NOT include 'config' tool (Rust parity)")
	}
	// Should include these.
	expected := []string{"read_file", "todo_write", "send_user_message"}
	for _, name := range expected {
		if !tools[name] {
			t.Errorf("expected %q in plan tools", name)
		}
	}
}

func TestValidateAgentInput(t *testing.T) {
	input := map[string]any{
		"description": "Fix the Login Bug",
		"prompt":      "Fix it",
	}
	spec, err := ValidateAgentInput(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if spec.Name != "fix-the-login-bug" {
		t.Errorf("expected slugified name 'fix-the-login-bug', got %q", spec.Name)
	}
	if spec.AgentID == "" {
		t.Error("expected non-empty agent_id")
	}
	if spec.Model != defaultAgentModel {
		t.Errorf("expected default model %q, got %q", defaultAgentModel, spec.Model)
	}
}

package tools

import (
	"encoding/json"
	"github.com/SocialGouv/iterion/pkg/claw/internal/runtime/task"
	"strings"
	"testing"
)

func TestTaskCreate(t *testing.T) {
	reg := task.NewRegistry()
	input := map[string]any{
		"prompt":      "Build the feature",
		"description": "A cool feature",
	}
	result, err := ExecuteTaskCreate(input, reg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var parsed task.Task
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if parsed.TaskID == "" {
		t.Error("expected non-empty task_id")
	}
	if parsed.Prompt != "Build the feature" {
		t.Errorf("expected prompt 'Build the feature', got %q", parsed.Prompt)
	}
	if parsed.Description == nil || *parsed.Description != "A cool feature" {
		t.Errorf("expected description 'A cool feature', got %v", parsed.Description)
	}
	if parsed.Status != task.StatusCreated {
		t.Errorf("expected status 'created', got %q", parsed.Status)
	}
}

func TestTaskGet(t *testing.T) {
	reg := task.NewRegistry()
	created := reg.Create("test prompt", nil)

	input := map[string]any{"task_id": created.TaskID}
	result, err := ExecuteTaskGet(input, reg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var parsed task.Task
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if parsed.TaskID != created.TaskID {
		t.Errorf("expected task_id %q, got %q", created.TaskID, parsed.TaskID)
	}
}

func TestTaskGet_NotFound(t *testing.T) {
	reg := task.NewRegistry()
	input := map[string]any{"task_id": "nonexistent"}
	_, err := ExecuteTaskGet(input, reg)
	if err == nil {
		t.Fatal("expected error for nonexistent task")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' in error, got: %v", err)
	}
}

func TestTaskList(t *testing.T) {
	reg := task.NewRegistry()
	reg.Create("task 1", nil)
	reg.Create("task 2", nil)

	input := map[string]any{}
	result, err := ExecuteTaskList(input, reg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	count, ok := parsed["count"].(float64)
	if !ok || int(count) != 2 {
		t.Errorf("expected count 2, got %v", parsed["count"])
	}
}

func TestTaskStop(t *testing.T) {
	reg := task.NewRegistry()
	created := reg.Create("stoppable task", nil)

	input := map[string]any{"task_id": created.TaskID}
	result, err := ExecuteTaskStop(input, reg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if parsed["status"] != "stopped" {
		t.Errorf("expected status 'stopped', got %v", parsed["status"])
	}
}

func TestTaskOutput(t *testing.T) {
	reg := task.NewRegistry()
	created := reg.Create("output task", nil)

	input := map[string]any{"task_id": created.TaskID}
	result, err := ExecuteTaskOutput(input, reg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if parsed["has_output"] != false {
		t.Errorf("expected has_output false, got %v", parsed["has_output"])
	}
	if parsed["output"] != "" {
		t.Errorf("expected empty output, got %v", parsed["output"])
	}
}

func TestTaskUpdate(t *testing.T) {
	reg := task.NewRegistry()
	created := reg.Create("updatable task", nil)

	input := map[string]any{
		"task_id": created.TaskID,
		"message": "Here is an update",
	}
	result, err := ExecuteTaskUpdate(input, reg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var parsed task.Task
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if len(parsed.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(parsed.Messages))
	}
	if parsed.Messages[0].Content != "Here is an update" {
		t.Errorf("expected message content 'Here is an update', got %q", parsed.Messages[0].Content)
	}
	if parsed.Messages[0].Role != "user" {
		t.Errorf("expected role 'user', got %q", parsed.Messages[0].Role)
	}
}

func TestRunTaskPacket(t *testing.T) {
	reg := task.NewRegistry()
	input := map[string]any{
		"objective":          "Build API",
		"scope":              "Backend services",
		"repo":               "github.com/test/repo",
		"branch_policy":      "feature branches",
		"commit_policy":      "conventional commits",
		"reporting_contract": "daily updates",
		"escalation_policy":  "escalate to lead",
	}
	result, err := ExecuteRunTaskPacket(input, reg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var parsed task.Task
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if parsed.TaskID == "" {
		t.Error("expected non-empty task_id")
	}
	if parsed.Prompt != "Build API" {
		t.Errorf("expected prompt 'Build API', got %q", parsed.Prompt)
	}
	if parsed.TaskPacket == nil {
		t.Fatal("expected task_packet to be set")
	}
	if parsed.TaskPacket.Repo != "github.com/test/repo" {
		t.Errorf("expected repo 'github.com/test/repo', got %q", parsed.TaskPacket.Repo)
	}
}

func TestRunTaskPacket_Invalid(t *testing.T) {
	reg := task.NewRegistry()
	input := map[string]any{
		"objective": "Build API",
		// missing all other required fields
	}
	_, err := ExecuteRunTaskPacket(input, reg)
	if err == nil {
		t.Fatal("expected error for invalid packet")
	}
}

func TestTaskCreate_NilRegistry(t *testing.T) {
	input := map[string]any{"prompt": "test"}
	_, err := ExecuteTaskCreate(input, nil)
	if err == nil {
		t.Fatal("expected error for nil registry")
	}
	if !strings.Contains(err.Error(), "not available") {
		t.Errorf("expected 'not available' in error, got: %v", err)
	}
}

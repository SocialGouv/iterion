package tools

import (
	"encoding/json"
	"github.com/SocialGouv/iterion/pkg/claw/internal/runtime/team"
	"strings"
	"testing"
)

func TestTeamCreate(t *testing.T) {
	reg := team.NewTeamRegistry()
	input := map[string]any{
		"name":     "Alpha Team",
		"task_ids": []any{"task-1", "task-2"},
	}
	result, err := ExecuteTeamCreate(input, reg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if parsed["name"] != "Alpha Team" {
		t.Errorf("expected name 'Alpha Team', got %v", parsed["name"])
	}
	if parsed["team_id"] == nil || parsed["team_id"] == "" {
		t.Error("expected non-empty team_id")
	}
	taskCount, ok := parsed["task_count"].(float64)
	if !ok || int(taskCount) != 2 {
		t.Errorf("expected task_count 2, got %v", parsed["task_count"])
	}
	if parsed["status"] != "created" {
		t.Errorf("expected status 'created', got %v", parsed["status"])
	}
}

func TestTeamDelete(t *testing.T) {
	reg := team.NewTeamRegistry()
	created := reg.Create("Delete Me", []string{"t1"})

	input := map[string]any{"team_id": created.TeamID}
	result, err := ExecuteTeamDelete(input, reg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if parsed["status"] != "deleted" {
		t.Errorf("expected status 'deleted', got %v", parsed["status"])
	}
	if parsed["message"] != "Team deleted" {
		t.Errorf("expected message 'Team deleted', got %v", parsed["message"])
	}
}

func TestTeamDelete_NotFound(t *testing.T) {
	reg := team.NewTeamRegistry()
	input := map[string]any{"team_id": "nonexistent"}
	_, err := ExecuteTeamDelete(input, reg)
	if err == nil {
		t.Fatal("expected error for nonexistent team")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' in error, got: %v", err)
	}
}

func TestTeamCreate_NilRegistry(t *testing.T) {
	input := map[string]any{
		"name":     "Test",
		"task_ids": []any{"t1"},
	}
	_, err := ExecuteTeamCreate(input, nil)
	if err == nil {
		t.Fatal("expected error for nil registry")
	}
	if !strings.Contains(err.Error(), "not available") {
		t.Errorf("expected 'not available' in error, got: %v", err)
	}
}

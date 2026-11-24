package tools

import (
	"encoding/json"
	"github.com/SocialGouv/iterion/pkg/claw/internal/runtime/team"
	"strings"
	"testing"
)

func TestCronCreate(t *testing.T) {
	reg := team.NewCronRegistry()
	input := map[string]any{
		"schedule": "*/5 * * * *",
		"prompt":   "Run health check",
	}
	result, err := ExecuteCronCreate(input, reg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var parsed team.CronEntry
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("failed to parse result: %v", err)
	}
	if parsed.CronID == "" {
		t.Error("expected non-empty cron_id")
	}
	if parsed.Schedule != "*/5 * * * *" {
		t.Errorf("expected schedule '*/5 * * * *', got %q", parsed.Schedule)
	}
	if parsed.Prompt != "Run health check" {
		t.Errorf("expected prompt 'Run health check', got %q", parsed.Prompt)
	}
	if !parsed.Enabled {
		t.Error("expected cron entry to be enabled")
	}
}

func TestCronList(t *testing.T) {
	reg := team.NewCronRegistry()
	reg.Create("*/5 * * * *", "job 1", nil)
	reg.Create("0 * * * *", "job 2", nil)

	input := map[string]any{}
	result, err := ExecuteCronList(input, reg)
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

func TestCronDelete(t *testing.T) {
	reg := team.NewCronRegistry()
	created := reg.Create("*/10 * * * *", "deletable job", nil)

	input := map[string]any{"cron_id": created.CronID}
	result, err := ExecuteCronDelete(input, reg)
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

	// Verify it's gone
	if !reg.IsEmpty() {
		t.Error("expected registry to be empty after delete")
	}
}

func TestCronDelete_NotFound(t *testing.T) {
	reg := team.NewCronRegistry()
	input := map[string]any{"cron_id": "nonexistent"}
	_, err := ExecuteCronDelete(input, reg)
	if err == nil {
		t.Fatal("expected error for nonexistent cron entry")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' in error, got: %v", err)
	}
}

func TestCronCreate_NilRegistry(t *testing.T) {
	input := map[string]any{
		"schedule": "*/5 * * * *",
		"prompt":   "test",
	}
	_, err := ExecuteCronCreate(input, nil)
	if err == nil {
		t.Fatal("expected error for nil registry")
	}
	if !strings.Contains(err.Error(), "not available") {
		t.Errorf("expected 'not available' in error, got: %v", err)
	}
}

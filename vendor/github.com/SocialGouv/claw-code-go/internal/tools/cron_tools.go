package tools

import (
	"github.com/SocialGouv/claw-code-go/internal/api"
	"github.com/SocialGouv/claw-code-go/internal/runtime/team"
	"encoding/json"
	"fmt"
)

// --- CronCreate ---

func CronCreateTool() api.Tool {
	return api.Tool{
		Name:        "cron_create",
		Description: "Create a new cron scheduled task.",
		InputSchema: api.InputSchema{
			Type: "object",
			Properties: map[string]api.Property{
				"schedule":    {Type: "string", Description: "Cron expression (5-field format)."},
				"prompt":      {Type: "string", Description: "The task prompt to run on schedule."},
				"description": {Type: "string", Description: "Optional description."},
			},
			Required: []string{"schedule", "prompt"},
		},
	}
}

func ExecuteCronCreate(input map[string]any, reg *team.CronRegistry) (string, error) {
	if reg == nil {
		return "", fmt.Errorf("cron_create: cron registry not available")
	}
	schedule, ok := input["schedule"].(string)
	if !ok || schedule == "" {
		return "", fmt.Errorf("cron_create: 'schedule' is required")
	}
	prompt, ok := input["prompt"].(string)
	if !ok || prompt == "" {
		return "", fmt.Errorf("cron_create: 'prompt' is required")
	}
	var desc *string
	if d, ok := input["description"].(string); ok && d != "" {
		desc = &d
	}
	entry := reg.Create(schedule, prompt, desc)
	out, _ := json.MarshalIndent(entry, "", "  ")
	return string(out), nil
}

// --- CronGet ---

func CronGetTool() api.Tool {
	return api.Tool{
		Name:        "cron_get",
		Description: "Get a cron entry by its ID.",
		InputSchema: api.InputSchema{
			Type: "object",
			Properties: map[string]api.Property{
				"cron_id": {Type: "string", Description: "The cron entry ID."},
			},
			Required: []string{"cron_id"},
		},
	}
}

func ExecuteCronGet(input map[string]any, reg *team.CronRegistry) (string, error) {
	if reg == nil {
		return "", fmt.Errorf("cron_get: cron registry not available")
	}
	cronID, ok := input["cron_id"].(string)
	if !ok || cronID == "" {
		return "", fmt.Errorf("cron_get: 'cron_id' is required")
	}
	entry, found := reg.Get(cronID)
	if !found {
		return "", fmt.Errorf("cron_get: cron entry not found: %s", cronID)
	}
	out, _ := json.MarshalIndent(entry, "", "  ")
	return string(out), nil
}

// --- CronList ---

func CronListTool() api.Tool {
	return api.Tool{
		Name:        "cron_list",
		Description: "List all cron entries.",
		InputSchema: api.InputSchema{
			Type:       "object",
			Properties: map[string]api.Property{},
		},
	}
}

func ExecuteCronList(input map[string]any, reg *team.CronRegistry) (string, error) {
	if reg == nil {
		return "", fmt.Errorf("cron_list: cron registry not available")
	}
	entries := reg.List(false)
	result := map[string]any{
		"crons": entries,
		"count": len(entries),
	}
	out, _ := json.MarshalIndent(result, "", "  ")
	return string(out), nil
}

// --- CronDelete ---

func CronDeleteTool() api.Tool {
	return api.Tool{
		Name:        "cron_delete",
		Description: "Delete a cron entry by its ID.",
		InputSchema: api.InputSchema{
			Type: "object",
			Properties: map[string]api.Property{
				"cron_id": {Type: "string", Description: "The cron entry ID to delete."},
			},
			Required: []string{"cron_id"},
		},
	}
}

func ExecuteCronDelete(input map[string]any, reg *team.CronRegistry) (string, error) {
	if reg == nil {
		return "", fmt.Errorf("cron_delete: cron registry not available")
	}
	cronID, ok := input["cron_id"].(string)
	if !ok || cronID == "" {
		return "", fmt.Errorf("cron_delete: 'cron_id' is required")
	}
	entry, err := reg.Delete(cronID)
	if err != nil {
		return "", fmt.Errorf("cron_delete: %w", err)
	}
	result := map[string]any{
		"cron_id":  entry.CronID,
		"schedule": entry.Schedule,
		"status":   "deleted",
		"message":  "Cron entry removed",
	}
	out, _ := json.MarshalIndent(result, "", "  ")
	return string(out), nil
}

package tools

import (
	"github.com/SocialGouv/claw-code-go/internal/api"
	"github.com/SocialGouv/claw-code-go/internal/runtime/team"
	"encoding/json"
	"fmt"
)

// --- TeamCreate ---

func TeamCreateTool() api.Tool {
	return api.Tool{
		Name:        "team_create",
		Description: "Create a new team with a name and list of task IDs.",
		InputSchema: api.InputSchema{
			Type: "object",
			Properties: map[string]api.Property{
				"name":     {Type: "string", Description: "The team name."},
				"task_ids": {Type: "array", Description: "Array of task IDs (strings) to assign to the team."},
			},
			Required: []string{"name", "task_ids"},
		},
	}
}

func ExecuteTeamCreate(input map[string]any, reg *team.TeamRegistry) (string, error) {
	if reg == nil {
		return "", fmt.Errorf("team_create: team registry not available")
	}
	name, ok := input["name"].(string)
	if !ok || name == "" {
		return "", fmt.Errorf("team_create: 'name' is required")
	}
	rawIDs, ok := input["task_ids"].([]any)
	if !ok {
		return "", fmt.Errorf("team_create: 'task_ids' must be an array")
	}
	taskIDs := make([]string, 0, len(rawIDs))
	for _, raw := range rawIDs {
		if s, ok := raw.(string); ok {
			taskIDs = append(taskIDs, s)
		}
	}
	t := reg.Create(name, taskIDs)
	result := map[string]any{
		"team_id":    t.TeamID,
		"name":       t.Name,
		"task_count": len(t.TaskIDs),
		"task_ids":   t.TaskIDs,
		"status":     t.Status.String(),
	}
	out, _ := json.MarshalIndent(result, "", "  ")
	return string(out), nil
}

// --- TeamGet ---

func TeamGetTool() api.Tool {
	return api.Tool{
		Name:        "team_get",
		Description: "Get a team by its ID.",
		InputSchema: api.InputSchema{
			Type: "object",
			Properties: map[string]api.Property{
				"team_id": {Type: "string", Description: "The team ID."},
			},
			Required: []string{"team_id"},
		},
	}
}

func ExecuteTeamGet(input map[string]any, reg *team.TeamRegistry) (string, error) {
	if reg == nil {
		return "", fmt.Errorf("team_get: team registry not available")
	}
	teamID, ok := input["team_id"].(string)
	if !ok || teamID == "" {
		return "", fmt.Errorf("team_get: 'team_id' is required")
	}
	t, found := reg.Get(teamID)
	if !found {
		return "", fmt.Errorf("team_get: team not found: %s", teamID)
	}
	out, _ := json.MarshalIndent(t, "", "  ")
	return string(out), nil
}

// --- TeamList ---

func TeamListTool() api.Tool {
	return api.Tool{
		Name:        "team_list",
		Description: "List all teams.",
		InputSchema: api.InputSchema{
			Type:       "object",
			Properties: map[string]api.Property{},
		},
	}
}

func ExecuteTeamList(input map[string]any, reg *team.TeamRegistry) (string, error) {
	if reg == nil {
		return "", fmt.Errorf("team_list: team registry not available")
	}
	teams := reg.List()
	result := map[string]any{
		"teams": teams,
		"count": len(teams),
	}
	out, _ := json.MarshalIndent(result, "", "  ")
	return string(out), nil
}

// --- TeamDelete ---

func TeamDeleteTool() api.Tool {
	return api.Tool{
		Name:        "team_delete",
		Description: "Delete a team by its ID.",
		InputSchema: api.InputSchema{
			Type: "object",
			Properties: map[string]api.Property{
				"team_id": {Type: "string", Description: "The team ID to delete."},
			},
			Required: []string{"team_id"},
		},
	}
}

func ExecuteTeamDelete(input map[string]any, reg *team.TeamRegistry) (string, error) {
	if reg == nil {
		return "", fmt.Errorf("team_delete: team registry not available")
	}
	teamID, ok := input["team_id"].(string)
	if !ok || teamID == "" {
		return "", fmt.Errorf("team_delete: 'team_id' is required")
	}
	t, err := reg.Delete(teamID)
	if err != nil {
		return "", fmt.Errorf("team_delete: %w", err)
	}
	result := map[string]any{
		"team_id": t.TeamID,
		"name":    t.Name,
		"status":  t.Status.String(),
		"message": "Team deleted",
	}
	out, _ := json.MarshalIndent(result, "", "  ")
	return string(out), nil
}

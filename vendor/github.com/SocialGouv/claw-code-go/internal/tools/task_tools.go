package tools

import (
	"encoding/json"
	"fmt"
	"github.com/SocialGouv/claw-code-go/internal/api"
	"github.com/SocialGouv/claw-code-go/internal/runtime/task"
)

// --- TaskCreate ---

func TaskCreateTool() api.Tool {
	return api.Tool{
		Name:        "task_create",
		Description: "Create a new task with a prompt and optional description.",
		InputSchema: api.InputSchema{
			Type: "object",
			Properties: map[string]api.Property{
				"prompt":      {Type: "string", Description: "The task prompt."},
				"description": {Type: "string", Description: "Optional task description."},
			},
			Required: []string{"prompt"},
		},
	}
}

func ExecuteTaskCreate(input map[string]any, reg *task.Registry) (string, error) {
	if reg == nil {
		return "", fmt.Errorf("task_create: task registry not available")
	}
	prompt, ok := input["prompt"].(string)
	if !ok || prompt == "" {
		return "", fmt.Errorf("task_create: 'prompt' is required")
	}
	var desc *string
	if d, ok := input["description"].(string); ok && d != "" {
		desc = &d
	}
	t := reg.Create(prompt, desc)
	out, _ := json.MarshalIndent(t, "", "  ")
	return string(out), nil
}

// --- TaskGet ---

func TaskGetTool() api.Tool {
	return api.Tool{
		Name:        "task_get",
		Description: "Get a task by its ID.",
		InputSchema: api.InputSchema{
			Type: "object",
			Properties: map[string]api.Property{
				"task_id": {Type: "string", Description: "The task ID."},
			},
			Required: []string{"task_id"},
		},
	}
}

func ExecuteTaskGet(input map[string]any, reg *task.Registry) (string, error) {
	if reg == nil {
		return "", fmt.Errorf("task_get: task registry not available")
	}
	taskID, ok := input["task_id"].(string)
	if !ok || taskID == "" {
		return "", fmt.Errorf("task_get: 'task_id' is required")
	}
	t, found := reg.Get(taskID)
	if !found {
		return "", fmt.Errorf("task_get: task not found: %s", taskID)
	}
	out, _ := json.MarshalIndent(t, "", "  ")
	return string(out), nil
}

// --- TaskList ---

func TaskListTool() api.Tool {
	return api.Tool{
		Name:        "task_list",
		Description: "List all tasks, optionally filtered by status.",
		InputSchema: api.InputSchema{
			Type: "object",
			Properties: map[string]api.Property{
				"status": {Type: "string", Description: "Optional status filter: created, running, completed, failed, stopped."},
			},
		},
	}
}

func ExecuteTaskList(input map[string]any, reg *task.Registry) (string, error) {
	if reg == nil {
		return "", fmt.Errorf("task_list: task registry not available")
	}
	var statusFilter *task.TaskStatus
	if s, ok := input["status"].(string); ok && s != "" {
		var ts task.TaskStatus
		if err := json.Unmarshal([]byte(`"`+s+`"`), &ts); err != nil {
			return "", fmt.Errorf("task_list: invalid status filter: %s", s)
		}
		statusFilter = &ts
	}
	tasks := reg.List(statusFilter)
	result := map[string]any{
		"tasks": tasks,
		"count": len(tasks),
	}
	out, _ := json.MarshalIndent(result, "", "  ")
	return string(out), nil
}

// --- TaskOutput ---

func TaskOutputTool() api.Tool {
	return api.Tool{
		Name:        "task_output",
		Description: "Get the output of a task.",
		InputSchema: api.InputSchema{
			Type: "object",
			Properties: map[string]api.Property{
				"task_id": {Type: "string", Description: "The task ID."},
			},
			Required: []string{"task_id"},
		},
	}
}

func ExecuteTaskOutput(input map[string]any, reg *task.Registry) (string, error) {
	if reg == nil {
		return "", fmt.Errorf("task_output: task registry not available")
	}
	taskID, ok := input["task_id"].(string)
	if !ok || taskID == "" {
		return "", fmt.Errorf("task_output: 'task_id' is required")
	}
	output, err := reg.Output(taskID)
	if err != nil {
		return "", fmt.Errorf("task_output: %w", err)
	}
	result := map[string]any{
		"task_id":    taskID,
		"output":     output,
		"has_output": output != "",
	}
	out, _ := json.MarshalIndent(result, "", "  ")
	return string(out), nil
}

// --- TaskStop ---

func TaskStopTool() api.Tool {
	return api.Tool{
		Name:        "task_stop",
		Description: "Stop a running task.",
		InputSchema: api.InputSchema{
			Type: "object",
			Properties: map[string]api.Property{
				"task_id": {Type: "string", Description: "The task ID to stop."},
			},
			Required: []string{"task_id"},
		},
	}
}

func ExecuteTaskStop(input map[string]any, reg *task.Registry) (string, error) {
	if reg == nil {
		return "", fmt.Errorf("task_stop: task registry not available")
	}
	taskID, ok := input["task_id"].(string)
	if !ok || taskID == "" {
		return "", fmt.Errorf("task_stop: 'task_id' is required")
	}
	t, err := reg.Stop(taskID)
	if err != nil {
		return "", fmt.Errorf("task_stop: %w", err)
	}
	result := map[string]any{
		"task_id": t.TaskID,
		"status":  t.Status.String(),
		"message": "Task stopped",
	}
	out, _ := json.MarshalIndent(result, "", "  ")
	return string(out), nil
}

// --- TaskUpdate ---

func TaskUpdateTool() api.Tool {
	return api.Tool{
		Name:        "task_update",
		Description: "Add a message to a task.",
		InputSchema: api.InputSchema{
			Type: "object",
			Properties: map[string]api.Property{
				"task_id": {Type: "string", Description: "The task ID."},
				"message": {Type: "string", Description: "The message to add."},
			},
			Required: []string{"task_id", "message"},
		},
	}
}

func ExecuteTaskUpdate(input map[string]any, reg *task.Registry) (string, error) {
	if reg == nil {
		return "", fmt.Errorf("task_update: task registry not available")
	}
	taskID, ok := input["task_id"].(string)
	if !ok || taskID == "" {
		return "", fmt.Errorf("task_update: 'task_id' is required")
	}
	message, ok := input["message"].(string)
	if !ok || message == "" {
		return "", fmt.Errorf("task_update: 'message' is required")
	}
	t, err := reg.Update(taskID, message)
	if err != nil {
		return "", fmt.Errorf("task_update: %w", err)
	}
	out, _ := json.MarshalIndent(t, "", "  ")
	return string(out), nil
}

// --- RunTaskPacket ---

func RunTaskPacketTool() api.Tool {
	return api.Tool{
		Name:        "run_task_packet",
		Description: "Create a task from a structured task packet specification.",
		InputSchema: api.InputSchema{
			Type: "object",
			Properties: map[string]api.Property{
				"objective":          {Type: "string", Description: "The task objective."},
				"scope":              {Type: "string", Description: "The task scope."},
				"repo":               {Type: "string", Description: "The repository."},
				"branch_policy":      {Type: "string", Description: "Branch policy."},
				"commit_policy":      {Type: "string", Description: "Commit policy."},
				"reporting_contract": {Type: "string", Description: "Reporting contract."},
				"escalation_policy":  {Type: "string", Description: "Escalation policy."},
			},
			Required: []string{"objective", "scope", "repo", "branch_policy", "commit_policy", "reporting_contract", "escalation_policy"},
		},
	}
}

func ExecuteRunTaskPacket(input map[string]any, reg *task.Registry) (string, error) {
	if reg == nil {
		return "", fmt.Errorf("run_task_packet: task registry not available")
	}
	packet := task.TaskPacket{
		Objective:         stringVal(input, "objective"),
		Scope:             stringVal(input, "scope"),
		Repo:              stringVal(input, "repo"),
		BranchPolicy:      stringVal(input, "branch_policy"),
		CommitPolicy:      stringVal(input, "commit_policy"),
		ReportingContract: stringVal(input, "reporting_contract"),
		EscalationPolicy:  stringVal(input, "escalation_policy"),
	}
	if tests, ok := input["acceptance_tests"].([]any); ok {
		for _, t := range tests {
			if s, ok := t.(string); ok {
				packet.AcceptanceTests = append(packet.AcceptanceTests, s)
			}
		}
	}
	t, err := reg.CreateFromPacket(packet)
	if err != nil {
		return "", fmt.Errorf("run_task_packet: %w", err)
	}
	out, _ := json.MarshalIndent(t, "", "  ")
	return string(out), nil
}

func stringVal(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}

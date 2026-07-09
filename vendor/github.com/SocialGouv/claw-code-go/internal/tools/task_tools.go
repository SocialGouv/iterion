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
		Name: "task_create",
		Description: "Create a task in the shared task graph. Give it a subject (or prompt) and, " +
			"when it depends on other tasks, wire the edges with blocks/blocked_by — the registry " +
			"maintains them reciprocally and task_list shows what is currently blocked. " +
			"Use the graph for multi-step work with ordering constraints; keep todo_write for a flat session checklist.",
		InputSchema: api.InputSchema{
			Type: "object",
			Properties: map[string]api.Property{
				"prompt":      {Type: "string", Description: "The task prompt (alias of subject; provide at least one)."},
				"subject":     {Type: "string", Description: "Short work-item title (alias of prompt)."},
				"description": {Type: "string", Description: "Optional task description."},
				"active_form": {Type: "string", Description: "Present-tense label shown while the task is in progress (e.g. \"Fixing the parser\")."},
				"owner":       {Type: "string", Description: "Optional owner (agent name or 'user')."},
				"blocks": {Type: "array", Description: "Task ids this task blocks (they wait on this one).",
					Items: &api.Property{Type: "string"}},
				"blocked_by": {Type: "array", Description: "Task ids this task waits on.",
					Items: &api.Property{Type: "string"}},
			},
		},
	}
}

func ExecuteTaskCreate(input map[string]any, reg *task.Registry) (string, error) {
	if reg == nil {
		return "", fmt.Errorf("task_create: task registry not available")
	}
	spec := task.TaskSpec{
		Prompt:     stringVal(input, "prompt"),
		Subject:    stringVal(input, "subject"),
		ActiveForm: stringVal(input, "active_form"),
		Owner:      stringVal(input, "owner"),
		Blocks:     stringSlice(input, "blocks"),
		BlockedBy:  stringSlice(input, "blocked_by"),
	}
	if spec.Prompt == "" && spec.Subject == "" {
		return "", fmt.Errorf("task_create: 'prompt' or 'subject' is required")
	}
	if d, ok := input["description"].(string); ok && d != "" {
		spec.Description = &d
	}
	t, err := reg.CreateWithSpec(spec)
	if err != nil {
		return "", fmt.Errorf("task_create: %w", err)
	}
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
		Name: "task_list",
		Description: "List tasks in the shared graph with their dependency state (blocked flags, blocks/blocked_by edges). " +
			"Optionally filter by status — both vocabularies are accepted (pending/in_progress/completed and created/running/completed/failed/stopped).",
		InputSchema: api.InputSchema{
			Type: "object",
			Properties: map[string]api.Property{
				"status": {Type: "string", Description: "Optional status filter: pending|in_progress|completed (aliases: created, running) or failed|stopped."},
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
		ts, ok := task.ParseStatusAlias(s)
		if !ok {
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
		Name: "task_update",
		Description: "Update a task in the shared graph: flip its status (pending|in_progress|completed — " +
			"mark completed the moment it is done; \"deleted\" removes it and cleans its edges), edit its " +
			"subject/description/active_form/owner, append dependency edges with add_blocks/add_blocked_by, " +
			"and/or append a message to its transcript. Provide task_id plus at least one change.",
		InputSchema: api.InputSchema{
			Type: "object",
			Properties: map[string]api.Property{
				"task_id":     {Type: "string", Description: "The task ID."},
				"message":     {Type: "string", Description: "Optional message to append to the task transcript."},
				"status":      {Type: "string", Description: "New status: pending|in_progress|completed|failed|stopped, or \"deleted\" to remove the task."},
				"subject":     {Type: "string", Description: "New subject."},
				"description": {Type: "string", Description: "New description."},
				"active_form": {Type: "string", Description: "New present-tense in-progress label."},
				"owner":       {Type: "string", Description: "New owner."},
				"add_blocks": {Type: "array", Description: "Task ids to add to this task's blocks edges.",
					Items: &api.Property{Type: "string"}},
				"add_blocked_by": {Type: "array", Description: "Task ids to add to this task's blocked_by edges.",
					Items: &api.Property{Type: "string"}},
			},
			Required: []string{"task_id"},
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

	var u task.TaskFieldUpdate
	hasFieldChange := false
	strField := func(key string, dst **string) {
		if v, ok := input[key].(string); ok && v != "" {
			*dst = &v
			hasFieldChange = true
		}
	}
	strField("subject", &u.Subject)
	strField("description", &u.Description)
	strField("active_form", &u.ActiveForm)
	strField("owner", &u.Owner)
	if s, ok := input["status"].(string); ok && s != "" {
		if s == "deleted" {
			u.Deleted = true
		} else {
			ts, ok := task.ParseStatusAlias(s)
			if !ok {
				return "", fmt.Errorf("task_update: invalid status: %s", s)
			}
			u.Status = &ts
		}
		hasFieldChange = true
	}
	if edges := stringSlice(input, "add_blocks"); len(edges) > 0 {
		u.AddBlocks = edges
		hasFieldChange = true
	}
	if edges := stringSlice(input, "add_blocked_by"); len(edges) > 0 {
		u.AddBlockedBy = edges
		hasFieldChange = true
	}

	message, _ := input["message"].(string)
	if message == "" && !hasFieldChange {
		return "", fmt.Errorf("task_update: provide at least one change (message, status, subject, description, active_form, owner, add_blocks, add_blocked_by)")
	}

	var t task.Task
	var err error
	if message != "" {
		if t, err = reg.Update(taskID, message); err != nil {
			return "", fmt.Errorf("task_update: %w", err)
		}
	}
	if hasFieldChange {
		if t, err = reg.SetFields(taskID, u); err != nil {
			return "", fmt.Errorf("task_update: %w", err)
		}
	}
	if u.Deleted {
		result := map[string]any{"task_id": taskID, "status": "deleted", "message": "Task removed from the graph"}
		out, _ := json.MarshalIndent(result, "", "  ")
		return string(out), nil
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

// stringSlice extracts a []string from a JSON-decoded []any value.
func stringSlice(m map[string]any, key string) []string {
	raw, ok := m[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

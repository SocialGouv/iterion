package tools

import (
	"encoding/json"
	"fmt"
	"github.com/SocialGouv/claw-code-go/internal/api"
	"os"
	"path/filepath"
)

const todosPath = ".claude/todos.json"

// TodoItem represents a single task in the todo list.
type TodoItem struct {
	ID       string `json:"id"`
	Content  string `json:"content"`
	Status   string `json:"status"`   // "pending" | "in_progress" | "done"
	Priority string `json:"priority"` // "high" | "medium" | "low"
}

// TodoWriteTool returns the tool definition for reading/writing the todo list.
func TodoWriteTool() api.Tool {
	return api.Tool{
		Name: "todo_write",
		Description: "Read or write the session task list persisted in .claude/todos.json (action \"read\" returns it, \"write\" replaces it). " +
			"Use it FREQUENTLY: for any work of three or more steps, record the steps up front, keep exactly one item in_progress, and flip each to done the moment it completes — never batch completions. " +
			"After a context compaction, re-read the list before continuing. " +
			"The list is the source of truth for what remains: before ending a turn with unblocked items left, advance the next pending item instead.",
		InputSchema: api.InputSchema{
			Type: "object",
			Properties: map[string]api.Property{
				"action": {
					Type:        "string",
					Description: `"read" to retrieve the current todo list, "write" to replace it`,
				},
				"todos": {
					Type:        "array",
					Description: `Array of todo items (required for action=write). Each item: {id, content, status: "pending"|"in_progress"|"done", priority: "high"|"medium"|"low"}`,
					// Items is mandatory for OpenAI's strict function-calling
					// schema validator — an "array" property without "items"
					// produces a 400 "array schema missing items" at request
					// time. Anthropic accepts both shapes; spelling out the
					// item schema doesn't penalise Claude.
					Items: &api.Property{
						Type: "object",
						Properties: map[string]api.Property{
							"id":       {Type: "string", Description: "Stable identifier for the todo item."},
							"content":  {Type: "string", Description: "Human-readable task description."},
							"status":   {Type: "string", Enum: []any{"pending", "in_progress", "done"}, Description: "Current state of the item."},
							"priority": {Type: "string", Enum: []any{"high", "medium", "low"}, Description: "Priority tier."},
						},
						Required: []string{"content", "status"},
					},
				},
			},
			Required: []string{"action"},
		},
	}
}

// ExecuteTodoWrite reads or writes the todo list.
func ExecuteTodoWrite(input map[string]any) (string, error) {
	action, ok := input["action"].(string)
	if !ok || action == "" {
		return "", fmt.Errorf("todo_write: 'action' is required (read or write)")
	}

	switch action {
	case "read":
		return readTodos()
	case "write":
		return writeTodos(input)
	default:
		return "", fmt.Errorf("todo_write: unknown action %q (use read or write)", action)
	}
}

func readTodos() (string, error) {
	data, err := os.ReadFile(todosPath)
	if os.IsNotExist(err) {
		return "[]", nil
	}
	if err != nil {
		return "", fmt.Errorf("todo_write: read: %w", err)
	}

	// Validate and pretty-print
	var todos []TodoItem
	if err := json.Unmarshal(data, &todos); err != nil {
		return "", fmt.Errorf("todo_write: parse todos: %w", err)
	}

	out, _ := json.MarshalIndent(todos, "", "  ")
	return string(out), nil
}

func writeTodos(input map[string]any) (string, error) {
	todosRaw, ok := input["todos"]
	if !ok {
		return "", fmt.Errorf("todo_write: 'todos' array is required for action=write")
	}

	// Marshal and unmarshal to validate structure
	raw, err := json.Marshal(todosRaw)
	if err != nil {
		return "", fmt.Errorf("todo_write: encode todos: %w", err)
	}

	var todos []TodoItem
	if err := json.Unmarshal(raw, &todos); err != nil {
		return "", fmt.Errorf("todo_write: validate todos: %w", err)
	}

	// Validate each item
	for i, t := range todos {
		if t.ID == "" {
			return "", fmt.Errorf("todo_write: item %d missing id", i)
		}
		if t.Content == "" {
			return "", fmt.Errorf("todo_write: item %d (%s) missing content", i, t.ID)
		}
		switch t.Status {
		case "pending", "in_progress", "done":
		default:
			return "", fmt.Errorf("todo_write: item %d (%s) invalid status %q", i, t.ID, t.Status)
		}
		switch t.Priority {
		case "high", "medium", "low":
		default:
			return "", fmt.Errorf("todo_write: item %d (%s) invalid priority %q", i, t.ID, t.Priority)
		}
	}

	// Ensure directory exists
	if err := os.MkdirAll(filepath.Dir(todosPath), 0o755); err != nil {
		return "", fmt.Errorf("todo_write: create dir: %w", err)
	}

	out, _ := json.MarshalIndent(todos, "", "  ")
	if err := os.WriteFile(todosPath, out, 0o644); err != nil {
		return "", fmt.Errorf("todo_write: write file: %w", err)
	}

	return fmt.Sprintf("Wrote %d todo item(s) to %s", len(todos), todosPath), nil
}

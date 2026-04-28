package tools

import (
	"github.com/SocialGouv/claw-code-go/internal/api"
	"encoding/json"
	"fmt"
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
		Name:        "todo_write",
		Description: "Read or write the task list stored in .claude/todos.json. Use action=read to retrieve todos, action=write to replace the list.",
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

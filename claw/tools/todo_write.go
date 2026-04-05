package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/zendev-sh/goai"
)

const todosRelPath = ".claude/todos.json"

var todoWriteSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"action": {
			"type": "string",
			"description": "\"read\" to retrieve the current todo list, \"write\" to replace it"
		},
		"todos": {
			"type": "array",
			"description": "Array of todo items (required for action=write). Each item: {id, content, status, priority}",
			"items": {
				"type": "object",
				"properties": {
					"id":       {"type": "string"},
					"content":  {"type": "string"},
					"status":   {"type": "string", "enum": ["pending", "in_progress", "done"]},
					"priority": {"type": "string", "enum": ["high", "medium", "low"]}
				},
				"required": ["id", "content", "status", "priority"]
			}
		}
	},
	"required": ["action"]
}`)

// TodoItem represents a single task.
type TodoItem struct {
	ID       string `json:"id"`
	Content  string `json:"content"`
	Status   string `json:"status"`
	Priority string `json:"priority"`
}

// TodoWrite returns a goai.Tool that reads/writes a todo list.
// The todos file is resolved relative to workDir.
func TodoWrite(workDir string) goai.Tool {
	todosPath := todosRelPath
	if workDir != "" {
		todosPath = filepath.Join(workDir, todosRelPath)
	}

	return goai.Tool{
		Name:        "todo_write",
		Description: "Read or write the task list stored in .claude/todos.json. Use action=read to retrieve todos, action=write to replace the list.",
		InputSchema: todoWriteSchema,
		Execute: func(_ context.Context, input json.RawMessage) (string, error) {
			var params struct {
				Action string          `json:"action"`
				Todos  json.RawMessage `json:"todos"`
			}
			if err := json.Unmarshal(input, &params); err != nil {
				return "", fmt.Errorf("todo_write: parse input: %w", err)
			}

			switch params.Action {
			case "read":
				data, err := os.ReadFile(todosPath)
				if os.IsNotExist(err) {
					return "[]", nil
				}
				if err != nil {
					return "", fmt.Errorf("todo_write: read: %w", err)
				}
				var todos []TodoItem
				if err := json.Unmarshal(data, &todos); err != nil {
					return "", fmt.Errorf("todo_write: parse: %w", err)
				}
				out, _ := json.MarshalIndent(todos, "", "  ")
				return string(out), nil

			case "write":
				if params.Todos == nil {
					return "", fmt.Errorf("todo_write: 'todos' array is required for action=write")
				}
				var todos []TodoItem
				if err := json.Unmarshal(params.Todos, &todos); err != nil {
					return "", fmt.Errorf("todo_write: validate: %w", err)
				}
				for i, t := range todos {
					if t.ID == "" {
						return "", fmt.Errorf("todo_write: item %d missing id", i)
					}
					if t.Content == "" {
						return "", fmt.Errorf("todo_write: item %d (%s) missing content", i, t.ID)
					}
				}
				if err := os.MkdirAll(filepath.Dir(todosPath), 0o755); err != nil {
					return "", fmt.Errorf("todo_write: create dir: %w", err)
				}
				out, _ := json.MarshalIndent(todos, "", "  ")
				if err := os.WriteFile(todosPath, out, 0o644); err != nil {
					return "", fmt.Errorf("todo_write: write: %w", err)
				}
				return fmt.Sprintf("Wrote %d todo item(s) to %s", len(todos), todosPath), nil

			default:
				return "", fmt.Errorf("todo_write: unknown action %q (use read or write)", params.Action)
			}
		},
	}
}

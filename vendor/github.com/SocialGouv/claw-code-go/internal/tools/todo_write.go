package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/SocialGouv/claw-code-go/internal/api"
	clawctx "github.com/SocialGouv/claw-code-go/internal/context"
)

// The todo list is SESSION state, not project state, so it lives OUT of the
// workspace tree (like Claude Code's ~/.claude/todos/). Persisting it inside
// the repo (`.claude/todos.json`, the original location) dirtied git
// status/diff on every agent run: stage-everything flows committed it,
// review loops diffed it, and iterion's worktree finalize wip-banked
// otherwise-clean runs because of it.

// todosLegacyPath is the historical in-workspace location, still read as a
// fallback so existing checklists survive the move. Writes never target it.
const todosLegacyPath = ".claude/todos.json"

// TodosPathForKey returns the out-of-tree todos file for a storage key:
// $CLAW_TODOS_DIR/<key>.json when set, else ~/.claw-code/todos/<key>.json
// (falling back to the OS temp dir when the home directory is unknown).
func TodosPathForKey(key string) string {
	base := os.Getenv("CLAW_TODOS_DIR")
	if base == "" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			base = filepath.Join(home, ".claw-code", "todos")
		} else {
			base = filepath.Join(os.TempDir(), "claw-code-todos")
		}
	}
	return filepath.Join(base, sanitizeTodosKey(key)+".json")
}

// DefaultTodosPath keys the todo file by the working directory's workspace
// fingerprint — every workspace (and thus every iterion worktree run) gets
// its own file. Callers with a session identity (the claw CLI loop) key by
// fingerprint+session instead, via TodosPathForKey.
func DefaultTodosPath() string {
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}
	return TodosPathForKey(clawctx.WorkspaceFingerprint(cwd))
}

// sanitizeTodosKey keeps keys filesystem-safe.
func sanitizeTodosKey(key string) string {
	var b strings.Builder
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	if b.Len() == 0 {
		return "default"
	}
	return b.String()
}

// TodoItem represents a single task in the todo list. The optional
// task-graph fields (active_form, owner, blocks, blocked_by) share the
// task registry's vocabulary and persist as written; the flat todo list
// does not enforce edge reciprocity — use the task_* tools when the
// dependency graph itself must be maintained.
type TodoItem struct {
	ID         string   `json:"id"`
	Content    string   `json:"content"`
	Status     string   `json:"status"`   // "pending" | "in_progress" | "done" ("completed" accepted as alias)
	Priority   string   `json:"priority"` // "high" | "medium" | "low"
	ActiveForm string   `json:"active_form,omitempty"`
	Owner      string   `json:"owner,omitempty"`
	Blocks     []string `json:"blocks,omitempty"`
	BlockedBy  []string `json:"blocked_by,omitempty"`
}

// TodoWriteTool returns the tool definition for reading/writing the todo list.
func TodoWriteTool() api.Tool {
	return api.Tool{
		Name: "todo_write",
		Description: "Read or write the session task list (action \"read\" returns it, \"write\" replaces it; stored per workspace outside the repo, never dirtying git). " +
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
							"id":          {Type: "string", Description: "Stable identifier for the todo item."},
							"content":     {Type: "string", Description: "Human-readable task description."},
							"status":      {Type: "string", Enum: []any{"pending", "in_progress", "done"}, Description: "Current state of the item (\"completed\" is accepted as an alias of done)."},
							"priority":    {Type: "string", Enum: []any{"high", "medium", "low"}, Description: "Priority tier."},
							"active_form": {Type: "string", Description: "Optional present-tense label shown while in progress."},
							"owner":       {Type: "string", Description: "Optional owner (agent name or 'user')."},
							"blocks": {Type: "array", Description: "Optional ids of items this one blocks.",
								Items: &api.Property{Type: "string"}},
							"blocked_by": {Type: "array", Description: "Optional ids of items this one waits on.",
								Items: &api.Property{Type: "string"}},
						},
						Required: []string{"content", "status"},
					},
				},
			},
			Required: []string{"action"},
		},
	}
}

// ExecuteTodoWrite reads or writes the todo list at the default
// per-workspace out-of-tree path (see DefaultTodosPath).
func ExecuteTodoWrite(input map[string]any) (string, error) {
	return ExecuteTodoWriteAt(input, DefaultTodosPath())
}

// ExecuteTodoWriteAt reads or writes the todo list at an explicit path —
// callers with a session identity key the file per session.
func ExecuteTodoWriteAt(input map[string]any, path string) (string, error) {
	action, ok := input["action"].(string)
	if !ok || action == "" {
		return "", fmt.Errorf("todo_write: 'action' is required (read or write)")
	}

	switch action {
	case "read":
		return readTodos(path)
	case "write":
		return writeTodos(input, path)
	default:
		return "", fmt.Errorf("todo_write: unknown action %q (use read or write)", action)
	}
}

func readTodos(path string) (string, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		// Legacy fallback: checklists written before the out-of-tree move
		// still resolve; the next write lands at the new location.
		data, err = os.ReadFile(todosLegacyPath)
		if os.IsNotExist(err) {
			return "[]", nil
		}
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

func writeTodos(input map[string]any, path string) (string, error) {
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
	for i := range todos {
		t := &todos[i]
		if t.ID == "" {
			return "", fmt.Errorf("todo_write: item %d missing id", i)
		}
		if t.Content == "" {
			return "", fmt.Errorf("todo_write: item %d (%s) missing content", i, t.ID)
		}
		if t.Status == "completed" { // task-graph vocabulary alias
			t.Status = "done"
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
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("todo_write: create dir: %w", err)
	}

	out, _ := json.MarshalIndent(todos, "", "  ")
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return "", fmt.Errorf("todo_write: write file: %w", err)
	}

	return fmt.Sprintf("Wrote %d todo item(s)", len(todos)), nil
}

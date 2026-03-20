// Package delegate provides backends for delegating agent/judge node execution
// to external CLI agents (e.g. claude-code, codex) instead of calling LLM APIs directly.
//
// When a node has `delegate: "claude_code"`, the executor bypasses the normal goai
// path and invokes the named Backend which spawns a CLI subprocess.
package delegate

import (
	"context"
	"encoding/json"
)

// Backend is the interface for delegation execution. Each backend wraps
// a CLI agent (e.g. claude, codex) and handles prompt delivery, tool
// forwarding, and output collection.
type Backend interface {
	// Execute runs the CLI agent with the given task and returns structured output.
	Execute(ctx context.Context, task Task) (Result, error)
}

// Task describes the work to delegate to a CLI agent.
type Task struct {
	// SystemPrompt is the fully resolved system prompt text.
	SystemPrompt string

	// UserPrompt is the fully resolved user message text.
	UserPrompt string

	// AllowedTools is the list of tool names the CLI agent may use.
	AllowedTools []string

	// OutputSchema is the JSON Schema for the expected structured output.
	// Nil means free-form text output.
	OutputSchema json.RawMessage

	// WorkDir is the working directory for the CLI subprocess.
	WorkDir string
}

// Result contains the output from a delegation backend.
type Result struct {
	// Output is the parsed structured output from the CLI agent.
	Output map[string]interface{}

	// Tokens is an estimate of total tokens consumed (if available from CLI metadata).
	Tokens int
}

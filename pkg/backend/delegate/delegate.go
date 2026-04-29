// Package delegate provides the Backend interface and types for executing
// agent/judge nodes via pluggable backends (CLI agents like claude-code/codex,
// or API-based backends like claw).
//
// When a node has `backend: "claude_code"`, the executor invokes the named
// Backend which handles execution (subprocess, API call, etc.).
package delegate

import (
	"context"
	"encoding/json"
	"time"
)

// Backend name constants used for registration and dispatch.
const (
	BackendClaw       = "claw"
	BackendClaudeCode = "claude_code"
	BackendCodex      = "codex"
)

// interactionSystemInstruction is appended to the system prompt when
// InteractionEnabled is true, instructing the delegate to signal user
// input needs via reserved output fields.
const interactionSystemInstruction = "\n\n[INTERACTION PROTOCOL]\n" +
	"If at any point you need input, clarification, or approval from a human user " +
	"to proceed with your task, you MUST include these fields in your JSON output:\n" +
	"  \"_needs_interaction\": true,\n" +
	"  \"_interaction_questions\": {\"question_key\": \"your question text\"}\n" +
	"Include as many question keys as needed. If you do NOT need human input, " +
	"do not include these fields and complete your task normally."

// Backend is the interface for delegation execution. Each backend wraps
// a CLI agent (e.g. claude, codex) and handles prompt delivery, tool
// forwarding, and output collection.
type Backend interface {
	// Execute runs the CLI agent with the given task and returns structured output.
	Execute(ctx context.Context, task Task) (Result, error)
}

// ToolDef is a fully resolved tool definition for backends that execute tools
// internally (e.g. claw). CLI-based backends use AllowedTools (string names) instead.
type ToolDef struct {
	Name        string
	Description string
	InputSchema json.RawMessage
	Execute     func(ctx context.Context, input json.RawMessage) (string, error)
}

// Task describes the work to execute on a backend.
type Task struct {
	// NodeID is the IR node identifier, used for observability hooks.
	NodeID string

	// SystemPrompt is the fully resolved system prompt text.
	SystemPrompt string

	// UserPrompt is the fully resolved user message text.
	UserPrompt string

	// AllowedTools is the list of tool names the CLI agent may use.
	// Used by CLI-based backends; API-based backends use ToolDefs instead.
	AllowedTools []string

	// ToolDefs provides full tool definitions for backends that manage tool
	// loops internally (e.g. claw). CLI-based backends ignore this field.
	ToolDefs []ToolDef

	// OutputSchema is the JSON Schema for the expected structured output.
	// Nil means free-form text output.
	OutputSchema json.RawMessage

	// Model is the resolved model spec (e.g. "anthropic/claude-sonnet-4-6").
	// Required for API-based backends; ignored by CLI-based backends.
	Model string

	// HasTools indicates whether the node has tools, enabling backends to
	// choose between structured-output and text-with-tools generation strategies.
	HasTools bool

	// ToolMaxSteps is the maximum number of tool-use iterations (0 = default).
	ToolMaxSteps int

	// MaxTokens caps the LLM response length per call. Honored by API-based
	// backends (claw); CLI-based backends (claude_code, codex) ignore it.
	// Zero means "use the backend default" (typically 8192).
	MaxTokens int

	// WorkDir is the working directory for the CLI subprocess.
	WorkDir string

	// BaseDir is the allowed base directory for WorkDir validation.
	// If set, WorkDir must resolve to a path within BaseDir.
	BaseDir string

	// ReasoningEffort is the reasoning effort level.
	// Valid values: "low", "medium", "high", "extra_high".
	ReasoningEffort string

	// SessionID is an optional session ID to resume (empty = fresh session).
	SessionID string

	// ForkSession, when true, forks from the resumed session instead of
	// continuing it. Requires SessionID to be set. The forked session gets
	// a new ID and does not mutate the original session.
	ForkSession bool

	// InteractionEnabled, when true, instructs the delegate to signal when
	// it needs user input by including _needs_interaction and
	// _interaction_questions fields in its output.
	InteractionEnabled bool
}

// Result contains the output from a delegation backend.
type Result struct {
	// Output is the parsed structured output from the CLI agent.
	Output map[string]interface{}

	// Tokens is an estimate of total tokens consumed (if available from CLI metadata).
	Tokens int

	// Duration is the wall-clock time of the subprocess execution.
	Duration time.Duration

	// ExitCode is the process exit code (0 on success).
	ExitCode int

	// Stderr contains captured stderr output (warnings, progress info).
	Stderr string

	// BackendName identifies which backend produced this result (e.g. "claude_code", "codex").
	BackendName string

	// RawOutputLen is the byte length of raw stdout before parsing.
	RawOutputLen int

	// ParseFallback is true when structured output was expected (OutputSchema set)
	// but JSON parsing fell back to wrapping plain text as {"text": "..."}.
	ParseFallback bool

	// FormattingPassUsed is true when a two-pass execution was performed:
	// Pass 1 with tools (no output format), Pass 2 with WithOutputFormat
	// (no tools) to guarantee structured output conforming to the schema.
	FormattingPassUsed bool

	// SessionID is the session ID returned by the CLI agent (empty if unavailable).
	SessionID string
}

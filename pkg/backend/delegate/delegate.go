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

	"github.com/SocialGouv/iterion/pkg/sandbox"
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
	// Valid values: "low", "medium", "high", "xhigh", "max".
	ReasoningEffort string

	// CompactThresholdRatio is the resolved compaction trigger as a
	// fraction of the model's context window (0 = use backend default).
	// Backends that maintain their own session history (claw) honor this;
	// CLI-based backends ignore it (claude_code does its own compaction).
	CompactThresholdRatio float64

	// CompactPreserveRecent is the number of recent messages kept verbatim
	// during compaction (0 = use backend default of 4).
	CompactPreserveRecent int

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

	// ResumeConversation, when non-nil, instructs the backend to skip
	// rendering the system+user prompts from scratch and instead replay
	// the persisted conversation history captured at the previous pause.
	// The backend appends a tool_result content block (tool_use_id =
	// ResumePendingToolUseID, content = ResumeAnswer) to answer the
	// pending ask_user call, then continues the agent loop. The opaque
	// json.RawMessage shape lets each backend choose its own message
	// representation (e.g. claw uses []api.Message).
	ResumeConversation json.RawMessage

	// ResumePendingToolUseID is the ID of the tool_use block waiting
	// for an answer in the persisted conversation. Required when
	// ResumeConversation is set.
	ResumePendingToolUseID string

	// ResumeAnswer is the human-supplied answer to the captured
	// ask_user call, sent back to the LLM as the tool_result content.
	ResumeAnswer string

	// Sandbox is the live sandbox handle for the run, or nil when the
	// workflow runs without isolation. Backends route their CLI
	// subprocess calls through it (via the SDK's CommandBuilder hook
	// for claude_code, or directly via Run.Command for shell-out
	// backends) so the agent's tools execute inside the container.
	//
	// In-process backends (claw) refuse to start when this is set —
	// see runtime.containsClawNode for the compile-time guard.
	Sandbox sandbox.Run
}

// SystemPromptWithInteraction returns the task's SystemPrompt augmented
// with the interaction protocol instructions when InteractionEnabled is
// true. Backends should call this instead of reading SystemPrompt
// directly so the LLM consistently learns how to escalate to a human.
func (t Task) SystemPromptWithInteraction() string {
	if t.InteractionEnabled {
		return t.SystemPrompt + interactionSystemInstruction
	}
	return t.SystemPrompt
}

// ErrAskUser is returned by the iterion-wired `ask_user` tool's handler
// when an LLM calls it during the agent loop. It propagates up through
// the generation layer to the backend, which converts it into a standard
// _needs_interaction Result so iterion's existing pause/resume flow
// surfaces the question to the dev's terminal and re-invokes the node
// with the answer.
//
// Conversation and PendingToolUseID enable mid-tool-loop resume: when set,
// they let the backend rehydrate the LLM's exact pre-pause state on the
// next turn (the persisted message history plus a tool_result block
// answering the captured tool_use). The opaque json.RawMessage type keeps
// the delegate package agnostic of any specific LLM SDK's message shape.
type ErrAskUser struct {
	Question         string
	PendingToolUseID string
	Conversation     json.RawMessage
}

func (e *ErrAskUser) Error() string {
	return "ask_user: " + e.Question
}

// AskUserQuestionKey is the canonical key under which iterion files an
// ask_user question in the Interaction record (and looks up the answer
// on resume). Stable across runs so workflow authors can reference
// {{input.ask_user_response}} in their prompts if they want explicit
// handling beyond the auto-prepended context block.
const AskUserQuestionKey = "ask_user_response"

// Reserved input keys used to relay ask_user pause/resume state across
// runtime → executor → backend. Owned by the delegate package because
// they are part of the ask_user contract and both pkg/runtime and
// pkg/backend/model already import delegate.
//
// PriorAskUser* keys carry the question/answer text for the prompt-side
// fallback (claude_code, codex). Resume* keys carry the persisted backend
// conversation, the pending tool_use ID, and the user's answer for
// in-process backends (claw) that can rehydrate the LLM mid-loop.
const (
	PriorAskUserQuestionKey   = "_prior_ask_user_question"
	PriorAskUserAnswerKey     = "_prior_ask_user_answer"
	ResumeConversationKey     = "_resume_conversation"
	ResumePendingToolUseIDKey = "_resume_pending_tool_use_id"
	ResumeAnswerKey           = "_resume_answer"
)

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

	// PendingConversation is the persisted LLM conversation captured at
	// the moment the agent loop was suspended by an ask_user call. The
	// runtime serializes this opaque blob into the checkpoint so that
	// resume can replay it via Task.ResumeConversation, preserving the
	// LLM's mid-tool-loop state across the pause. Backends that cannot
	// persist conversation state (CLI-based: claude_code, codex) leave
	// this nil and rely on the [PRIOR INTERACTION] prompt-side fallback.
	PendingConversation json.RawMessage

	// PendingToolUseID is the ID of the tool_use block awaiting an
	// answer in PendingConversation. Required when PendingConversation
	// is non-nil.
	PendingToolUseID string
}

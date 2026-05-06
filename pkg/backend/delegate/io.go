package delegate

import (
	"encoding/json"
	"time"
)

// IOTask is the on-the-wire form of a [Task] used by the claw
// runner sub-binary IPC.
//
// It mirrors [Task] with two intentional omissions:
//
//   - Sandbox: the [sandbox.Run] handle is not portable across
//     processes (it wraps a live container). The runner is, by
//     definition, *already inside* the sandbox so it doesn't need it.
//   - ToolDefs: the [ToolDef.Execute] field is a Go closure with
//     captured state (executor, MCP manager, ask_user channel) that
//     cannot be serialized. The runner registers iterion's default
//     tool set on its own — Phase 4 V1 supports the standard claw
//     tools (Bash, FileEdit, …); MCP-routed and ask_user tools land
//     in V2.
//
// The wire format is JSON. The launcher writes one IOTask line on
// stdin; the runner writes one [IORunnerOutput] line per significant
// event (tool call, intermediate message) followed by the final
// [IOResult] line. NDJSON keeps stream parsing simple on both sides.
type IOTask struct {
	NodeID                 string          `json:"node_id"`
	SystemPrompt           string          `json:"system_prompt,omitempty"`
	UserPrompt             string          `json:"user_prompt,omitempty"`
	AllowedTools           []string        `json:"allowed_tools,omitempty"`
	OutputSchema           json.RawMessage `json:"output_schema,omitempty"`
	Model                  string          `json:"model,omitempty"`
	HasTools               bool            `json:"has_tools,omitempty"`
	ToolMaxSteps           int             `json:"tool_max_steps,omitempty"`
	MaxTokens              int             `json:"max_tokens,omitempty"`
	WorkDir                string          `json:"work_dir,omitempty"`
	BaseDir                string          `json:"base_dir,omitempty"`
	ReasoningEffort        string          `json:"reasoning_effort,omitempty"`
	CompactThresholdRatio  float64         `json:"compact_threshold_ratio,omitempty"`
	CompactPreserveRecent  int             `json:"compact_preserve_recent,omitempty"`
	SessionID              string          `json:"session_id,omitempty"`
	ForkSession            bool            `json:"fork_session,omitempty"`
	InteractionEnabled     bool            `json:"interaction_enabled,omitempty"`
	ResumeConversation     json.RawMessage `json:"resume_conversation,omitempty"`
	ResumePendingToolUseID string          `json:"resume_pending_tool_use_id,omitempty"`
	ResumeAnswer           string          `json:"resume_answer,omitempty"`
}

// IOResult is the on-the-wire form of a [Result] returned by the
// claw runner. Carries an explicit Error string for protocol-level
// failures (the runner's exit code is also non-zero in that case,
// but the structured Error lets the launcher surface a typed
// message without re-parsing stderr).
type IOResult struct {
	Output              map[string]interface{} `json:"output,omitempty"`
	Tokens              int                    `json:"tokens,omitempty"`
	DurationMS          int64                  `json:"duration_ms,omitempty"`
	ExitCode            int                    `json:"exit_code,omitempty"`
	Stderr              string                 `json:"stderr,omitempty"`
	BackendName         string                 `json:"backend_name,omitempty"`
	RawOutputLen        int                    `json:"raw_output_len,omitempty"`
	ParseFallback       bool                   `json:"parse_fallback,omitempty"`
	FormattingPassUsed  bool                   `json:"formatting_pass_used,omitempty"`
	SessionID           string                 `json:"session_id,omitempty"`
	PendingConversation json.RawMessage        `json:"pending_conversation,omitempty"`
	PendingToolUseID    string                 `json:"pending_tool_use_id,omitempty"`
	Error               string                 `json:"error,omitempty"`
}

// ToIOTask converts a [Task] to its wire form. The Sandbox handle
// and ToolDefs slice are dropped per the design above.
func ToIOTask(t Task) IOTask {
	return IOTask{
		NodeID:                 t.NodeID,
		SystemPrompt:           t.SystemPrompt,
		UserPrompt:             t.UserPrompt,
		AllowedTools:           t.AllowedTools,
		OutputSchema:           t.OutputSchema,
		Model:                  t.Model,
		HasTools:               t.HasTools,
		ToolMaxSteps:           t.ToolMaxSteps,
		MaxTokens:              t.MaxTokens,
		WorkDir:                t.WorkDir,
		BaseDir:                t.BaseDir,
		ReasoningEffort:        t.ReasoningEffort,
		CompactThresholdRatio:  t.CompactThresholdRatio,
		CompactPreserveRecent:  t.CompactPreserveRecent,
		SessionID:              t.SessionID,
		ForkSession:            t.ForkSession,
		InteractionEnabled:     t.InteractionEnabled,
		ResumeConversation:     t.ResumeConversation,
		ResumePendingToolUseID: t.ResumePendingToolUseID,
		ResumeAnswer:           t.ResumeAnswer,
	}
}

// FromIOTask converts an [IOTask] back to a [Task]. Sandbox is left
// nil (the runner is inside the sandbox already); ToolDefs is left
// nil (the runner registers them on its own).
func FromIOTask(t IOTask) Task {
	return Task{
		NodeID:                 t.NodeID,
		SystemPrompt:           t.SystemPrompt,
		UserPrompt:             t.UserPrompt,
		AllowedTools:           t.AllowedTools,
		OutputSchema:           t.OutputSchema,
		Model:                  t.Model,
		HasTools:               t.HasTools,
		ToolMaxSteps:           t.ToolMaxSteps,
		MaxTokens:              t.MaxTokens,
		WorkDir:                t.WorkDir,
		BaseDir:                t.BaseDir,
		ReasoningEffort:        t.ReasoningEffort,
		CompactThresholdRatio:  t.CompactThresholdRatio,
		CompactPreserveRecent:  t.CompactPreserveRecent,
		SessionID:              t.SessionID,
		ForkSession:            t.ForkSession,
		InteractionEnabled:     t.InteractionEnabled,
		ResumeConversation:     t.ResumeConversation,
		ResumePendingToolUseID: t.ResumePendingToolUseID,
		ResumeAnswer:           t.ResumeAnswer,
	}
}

// ToIOResult converts a [Result] to its wire form.
func ToIOResult(r Result) IOResult {
	return IOResult{
		Output:              r.Output,
		Tokens:              r.Tokens,
		DurationMS:          r.Duration.Milliseconds(),
		ExitCode:            r.ExitCode,
		Stderr:              r.Stderr,
		BackendName:         r.BackendName,
		RawOutputLen:        r.RawOutputLen,
		ParseFallback:       r.ParseFallback,
		FormattingPassUsed:  r.FormattingPassUsed,
		SessionID:           r.SessionID,
		PendingConversation: r.PendingConversation,
		PendingToolUseID:    r.PendingToolUseID,
	}
}

// FromIOResult converts an [IOResult] back to a [Result]. The Error
// field is dropped — the launcher surfaces it via a separate error
// return value, not by populating the Result.
func FromIOResult(r IOResult) Result {
	return Result{
		Output:              r.Output,
		Tokens:              r.Tokens,
		Duration:            time.Duration(r.DurationMS) * time.Millisecond,
		ExitCode:            r.ExitCode,
		Stderr:              r.Stderr,
		BackendName:         r.BackendName,
		RawOutputLen:        r.RawOutputLen,
		ParseFallback:       r.ParseFallback,
		FormattingPassUsed:  r.FormattingPassUsed,
		SessionID:           r.SessionID,
		PendingConversation: r.PendingConversation,
		PendingToolUseID:    r.PendingToolUseID,
	}
}

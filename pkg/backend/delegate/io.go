package delegate

import (
	"encoding/json"
	"time"
)

// IOTask is the on-the-wire form of a [Task] used by the claw
// runner sub-binary IPC.
//
// It mirrors [Task] with one intentional omission:
//
//   - Sandbox: the [sandbox.Run] handle is not portable across
//     processes (it wraps a live container). The runner is, by
//     definition, *already inside* the sandbox so it doesn't need it.
//
// V2-1+ wire format: NDJSON envelopes (see [Envelope]). The launcher
// emits one [EnvelopeTask] wrapping an IOTask; the runner emits any
// number of intermediate envelopes (tool_call / ask_user /
// session_capture / event) and finishes with one [EnvelopeResult]
// wrapping an [IOResult].
//
// V2-2: the [Task.ToolDefs] slice is now carried over the wire as
// [IOToolDef] entries — the [ToolDef.Execute] closure is dropped (it
// captures launcher-side state like the MCP manager) and the runner
// builds proxy ToolDefs whose Execute emits [EnvelopeToolCall] and
// blocks on the matching [EnvelopeToolResult]. This unblocks the
// MCP-tools-in-sandbox path that V1 couldn't support.
type IOTask struct {
	NodeID                 string          `json:"node_id"`
	SystemPrompt           string          `json:"system_prompt,omitempty"`
	UserPrompt             string          `json:"user_prompt,omitempty"`
	AllowedTools           []string        `json:"allowed_tools,omitempty"`
	ToolDefs               []IOToolDef     `json:"tool_defs,omitempty"`
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

// IOToolDef is the wire form of a [ToolDef]. The Execute closure is
// dropped — Go closures don't survive process boundaries — and the
// runner builds proxies whose Execute round-trips through the
// envelope channel back to the launcher's original ToolDef. V2-2.
type IOToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
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

// ToIOTask converts a [Task] to its wire form. The Sandbox handle is
// dropped (the runner is inside the sandbox already); the
// [ToolDef.Execute] closures are dropped and replaced by metadata-only
// [IOToolDef] entries (V2-2 — the runner builds proxy ToolDefs).
func ToIOTask(t Task) IOTask {
	var ioToolDefs []IOToolDef
	if len(t.ToolDefs) > 0 {
		ioToolDefs = make([]IOToolDef, len(t.ToolDefs))
		for i, td := range t.ToolDefs {
			ioToolDefs[i] = IOToolDef{
				Name:        td.Name,
				Description: td.Description,
				InputSchema: td.InputSchema,
			}
		}
	}
	return IOTask{
		NodeID:                 t.NodeID,
		SystemPrompt:           t.SystemPrompt,
		UserPrompt:             t.UserPrompt,
		AllowedTools:           t.AllowedTools,
		ToolDefs:               ioToolDefs,
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

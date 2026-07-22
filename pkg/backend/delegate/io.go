package delegate

import (
	"encoding/json"
	"time"

	"github.com/SocialGouv/iterion/pkg/plugin"
)

// IOTask is the on-the-wire form of a [Task] used by the claw
// runner sub-binary IPC.
//
// IOTask carries the serializable Task value fields that the runner may
// need after reconstruction, including prompt-composition inputs,
// multimodal user content, board/session routing, memory routing, and
// tool metadata. The following Task fields are intentionally NOT carried
// over the wire:
//
//   - Sandbox: the [sandbox.Run] handle is not portable across
//     processes (it wraps a live container). The runner is, by
//     definition, *already inside* the sandbox so it doesn't need it.
//   - Hooks: a [TaskHooks] struct of closures. Go closures don't
//     survive process boundaries; intermediate observability events
//     are surfaced via the envelope channel (see [Envelope]) and the
//     launcher fires the hooks on the parent side after demuxing.
//   - MaterializeSecrets: a closure bound to launcher-side secretguard
//     state. Secret materialisation happens on the parent side for
//     host-executed tools; closures cannot be serialized into the runner.
//   - InboxDrain: a launcher-side closure used to drain queued operator
//     messages; closures cannot cross process boundaries.
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
	NodeID                 string                `json:"node_id"`
	Iteration              int                   `json:"iteration,omitempty"`
	SystemPrompt           string                `json:"system_prompt,omitempty"`
	SystemPromptMode       SystemPromptMode      `json:"system_prompt_mode,omitempty"`
	UserPrompt             string                `json:"user_prompt,omitempty"`
	UserContent            []ContentBlock        `json:"user_content,omitempty"`
	AllowedTools           []string              `json:"allowed_tools,omitempty"`
	Readonly               bool                  `json:"readonly,omitempty"`
	Capabilities           []string              `json:"capabilities,omitempty"`
	StoreDir               string                `json:"store_dir,omitempty"`
	BoardHTTPEndpoint      string                `json:"board_http_endpoint,omitempty"`
	BoardRunToken          string                `json:"board_run_token,omitempty"`
	AskUserHTTPEndpoint    string                `json:"ask_user_http_endpoint,omitempty"`
	AskUserRunToken        string                `json:"ask_user_run_token,omitempty"`
	ToolDefs               []IOToolDef           `json:"tool_defs,omitempty"`
	OutputSchema           json.RawMessage       `json:"output_schema,omitempty"`
	Model                  string                `json:"model,omitempty"`
	HasTools               bool                  `json:"has_tools,omitempty"`
	ToolMaxSteps           int                   `json:"tool_max_steps,omitempty"`
	MaxTokens              int                   `json:"max_tokens,omitempty"`
	WorkDir                string                `json:"work_dir,omitempty"`
	BaseDir                string                `json:"base_dir,omitempty"`
	RepoRoot               string                `json:"repo_root,omitempty"`
	ReasoningEffort        string                `json:"reasoning_effort,omitempty"`
	Ultracode              bool                  `json:"ultracode,omitempty"`
	SecretsHygiene         bool                  `json:"secrets_hygiene,omitempty"`
	SecretFiles            []SecretFileHint      `json:"secret_files,omitempty"`
	CursorFragments        []string              `json:"cursor_fragments,omitempty"`
	SkillHints             []SkillHint           `json:"skill_hints,omitempty"`
	PresetFragment         string                `json:"preset_fragment,omitempty"`
	CompactThresholdRatio  float64               `json:"compact_threshold_ratio,omitempty"`
	CompactPreserveRecent  int                   `json:"compact_preserve_recent,omitempty"`
	SessionID              string                `json:"session_id,omitempty"`
	ForkSession            bool                  `json:"fork_session,omitempty"`
	SessionFingerprint     string                `json:"session_fingerprint,omitempty"`
	ProviderHint           string                `json:"provider_hint,omitempty"`
	InteractionEnabled     bool                  `json:"interaction_enabled,omitempty"`
	ResumeConversation     json.RawMessage       `json:"resume_conversation,omitempty"`
	ResumePendingToolUseID string                `json:"resume_pending_tool_use_id,omitempty"`
	ResumeAnswer           string                `json:"resume_answer,omitempty"`
	Memory                 *MemorySpec           `json:"memory,omitempty"`
	CompressMode           string                `json:"compress_mode,omitempty"`
	Rewriters              []plugin.RewriterSpec `json:"rewriters,omitempty"`
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
	Output              map[string]any  `json:"output,omitempty"`
	Tokens              int             `json:"tokens,omitempty"`
	DurationMS          int64           `json:"duration_ms,omitempty"`
	ExitCode            int             `json:"exit_code,omitempty"`
	Stderr              string          `json:"stderr,omitempty"`
	BackendName         string          `json:"backend_name,omitempty"`
	RawOutputLen        int             `json:"raw_output_len,omitempty"`
	ParseFallback       bool            `json:"parse_fallback,omitempty"`
	FormattingPassUsed  bool            `json:"formatting_pass_used,omitempty"`
	SessionID           string          `json:"session_id,omitempty"`
	PendingConversation json.RawMessage `json:"pending_conversation,omitempty"`
	PendingToolUseID    string          `json:"pending_tool_use_id,omitempty"`
	Error               string          `json:"error,omitempty"`
}

// ToIOTask converts a [Task] to its wire form. The Sandbox handle and
// closure fields are dropped; the [ToolDef.Execute] closures are
// replaced by metadata-only [IOToolDef] entries (V2-2 — the runner
// builds proxy ToolDefs).
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
		Iteration:              t.Iteration,
		SystemPrompt:           t.SystemPrompt,
		SystemPromptMode:       t.SystemPromptMode,
		UserPrompt:             t.UserPrompt,
		UserContent:            t.UserContent,
		AllowedTools:           t.AllowedTools,
		Readonly:               t.Readonly,
		Capabilities:           t.Capabilities,
		StoreDir:               t.StoreDir,
		BoardHTTPEndpoint:      t.BoardHTTPEndpoint,
		BoardRunToken:          t.BoardRunToken,
		AskUserHTTPEndpoint:    t.AskUserHTTPEndpoint,
		AskUserRunToken:        t.AskUserRunToken,
		ToolDefs:               ioToolDefs,
		OutputSchema:           t.OutputSchema,
		Model:                  t.Model,
		HasTools:               t.HasTools,
		ToolMaxSteps:           t.ToolMaxSteps,
		MaxTokens:              t.MaxTokens,
		WorkDir:                t.WorkDir,
		BaseDir:                t.BaseDir,
		RepoRoot:               t.RepoRoot,
		ReasoningEffort:        t.ReasoningEffort,
		Ultracode:              t.Ultracode,
		SecretsHygiene:         t.SecretsHygiene,
		SecretFiles:            t.SecretFiles,
		CursorFragments:        t.CursorFragments,
		SkillHints:             t.SkillHints,
		PresetFragment:         t.PresetFragment,
		CompactThresholdRatio:  t.CompactThresholdRatio,
		CompactPreserveRecent:  t.CompactPreserveRecent,
		SessionID:              t.SessionID,
		ForkSession:            t.ForkSession,
		SessionFingerprint:     t.SessionFingerprint,
		ProviderHint:           t.ProviderHint,
		InteractionEnabled:     t.InteractionEnabled,
		ResumeConversation:     t.ResumeConversation,
		ResumePendingToolUseID: t.ResumePendingToolUseID,
		ResumeAnswer:           t.ResumeAnswer,
		Memory:                 t.Memory,
		CompressMode:           t.CompressMode,
		Rewriters:              t.Rewriters,
	}
}

// FromIOTask converts an [IOTask] back to a [Task]. Sandbox is left
// nil (the runner is inside the sandbox already); ToolDefs is left
// nil (the runner registers them on its own); Hooks, MaterializeSecrets,
// and InboxDrain are left zero (closures don't cross processes —
// intermediate events are surfaced through the envelope channel).
func FromIOTask(t IOTask) Task {
	return Task{
		NodeID:                 t.NodeID,
		Iteration:              t.Iteration,
		SystemPrompt:           t.SystemPrompt,
		SystemPromptMode:       t.SystemPromptMode,
		UserPrompt:             t.UserPrompt,
		UserContent:            t.UserContent,
		AllowedTools:           t.AllowedTools,
		Readonly:               t.Readonly,
		Capabilities:           t.Capabilities,
		StoreDir:               t.StoreDir,
		BoardHTTPEndpoint:      t.BoardHTTPEndpoint,
		BoardRunToken:          t.BoardRunToken,
		AskUserHTTPEndpoint:    t.AskUserHTTPEndpoint,
		AskUserRunToken:        t.AskUserRunToken,
		OutputSchema:           t.OutputSchema,
		Model:                  t.Model,
		HasTools:               t.HasTools,
		ToolMaxSteps:           t.ToolMaxSteps,
		MaxTokens:              t.MaxTokens,
		WorkDir:                t.WorkDir,
		BaseDir:                t.BaseDir,
		RepoRoot:               t.RepoRoot,
		ReasoningEffort:        t.ReasoningEffort,
		Ultracode:              t.Ultracode,
		SecretsHygiene:         t.SecretsHygiene,
		SecretFiles:            t.SecretFiles,
		CursorFragments:        t.CursorFragments,
		SkillHints:             t.SkillHints,
		PresetFragment:         t.PresetFragment,
		CompactThresholdRatio:  t.CompactThresholdRatio,
		CompactPreserveRecent:  t.CompactPreserveRecent,
		SessionID:              t.SessionID,
		ForkSession:            t.ForkSession,
		SessionFingerprint:     t.SessionFingerprint,
		ProviderHint:           t.ProviderHint,
		InteractionEnabled:     t.InteractionEnabled,
		ResumeConversation:     t.ResumeConversation,
		ResumePendingToolUseID: t.ResumePendingToolUseID,
		ResumeAnswer:           t.ResumeAnswer,
		Memory:                 t.Memory,
		CompressMode:           t.CompressMode,
		Rewriters:              t.Rewriters,
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

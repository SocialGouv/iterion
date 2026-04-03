package claude

import "context"

// HookEvent identifies when a hook fires in the agent lifecycle.
type HookEvent string

const (
	HookPreToolUse         HookEvent = "PreToolUse"
	HookPostToolUse        HookEvent = "PostToolUse"
	HookPostToolUseFailure HookEvent = "PostToolUseFailure"
	HookStop               HookEvent = "Stop"
	HookSubagentStart      HookEvent = "SubagentStart"
	HookSubagentStop       HookEvent = "SubagentStop"
	HookUserPromptSubmit   HookEvent = "UserPromptSubmit"
	HookPreCompact         HookEvent = "PreCompact"
	HookNotification       HookEvent = "Notification"
)

// HookMatcher defines a hook registration: a pattern to match and a handler to call.
type HookMatcher struct {
	// Matcher is a regex pattern to match tool names (e.g. "Bash|Edit").
	// nil matches all tools.
	Matcher *string

	// Timeout in seconds for the hook callback. Defaults to 60.
	Timeout *float64

	// Handler is called when the hook fires.
	Handler HookCallback
}

// HookCallback is a function invoked when a hook event fires.
// The [HookCallbackInput] fields populated vary by event type.
type HookCallback func(ctx context.Context, input HookCallbackInput) (HookOutput, error)

// HookCallbackInput contains the data sent to a hook callback.
type HookCallbackInput struct {
	SessionID      string         `json:"session_id"`
	TranscriptPath string         `json:"transcript_path"`
	Cwd            string         `json:"cwd"`
	PermissionMode *string        `json:"permission_mode,omitempty"`
	HookEventName  string         `json:"hook_event_name"`
	ToolName       string         `json:"tool_name,omitempty"`
	ToolInput      map[string]any `json:"tool_input,omitempty"`
	ToolResponse   any            `json:"tool_response,omitempty"`
	ToolUseID      *string        `json:"tool_use_id,omitempty"`
}

// HookOutput is returned by a hook callback to control agent behavior.
type HookOutput struct {
	// Decision controls permission for PreToolUse hooks: "allow", "deny", or "ask".
	Decision string `json:"permissionDecision,omitempty"`

	// DecisionReason explains the permission decision.
	DecisionReason string `json:"permissionDecisionReason,omitempty"`

	// UpdatedInput provides modified tool input (only with "allow" decision).
	UpdatedInput map[string]any `json:"updatedInput,omitempty"`

	// AdditionalContext is appended to the tool result (PostToolUse only).
	AdditionalContext string `json:"additionalContext,omitempty"`

	// SystemMessage is injected into the conversation for the model.
	SystemMessage string `json:"systemMessage,omitempty"`

	// Continue controls whether the agent should keep running.
	Continue *bool `json:"continue,omitempty"`

	// SuppressOutput prevents the hook output from appearing.
	SuppressOutput *bool `json:"suppressOutput,omitempty"`

	// BlockStop prevents the stop event from completing (Stop hook).
	BlockStop bool `json:"decision,omitempty"`

	// StopReason overrides the stop reason.
	StopReason string `json:"stopReason,omitempty"`

	// Reason explains a blocked stop event.
	Reason string `json:"reason,omitempty"`
}

// hookMatcherConfig is the JSON-serializable form sent to the CLI initialize request.
type hookMatcherConfig struct {
	Matcher         *string  `json:"matcher"`
	HookCallbackIDs []string `json:"hookCallbackIds"`
	Timeout         *float64 `json:"timeout,omitempty"`
}

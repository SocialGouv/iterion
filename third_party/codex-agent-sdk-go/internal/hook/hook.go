// Package hook provides hook types for intercepting CLI events.
package hook

import "context"

// Event represents the type of event that triggers a hook.
type Event string

const (
	// EventPreToolUse is triggered before a tool is used.
	EventPreToolUse Event = "PreToolUse"
	// EventPostToolUse is triggered after a tool is used.
	EventPostToolUse Event = "PostToolUse"
	// EventUserPromptSubmit is triggered when a user submits a prompt.
	EventUserPromptSubmit Event = "UserPromptSubmit"
	// EventStop is triggered when a session stops.
	EventStop Event = "Stop"
	// EventSubagentStop is triggered when a subagent stops.
	EventSubagentStop Event = "SubagentStop"
	// EventPreCompact is triggered before compaction.
	EventPreCompact Event = "PreCompact"
	// EventPostToolUseFailure is triggered after a tool use fails.
	EventPostToolUseFailure Event = "PostToolUseFailure"
	// EventNotification is triggered when a notification is sent.
	EventNotification Event = "Notification"
	// EventSubagentStart is triggered when a subagent starts.
	EventSubagentStart Event = "SubagentStart"
	// EventPermissionRequest is triggered when a permission is requested.
	EventPermissionRequest Event = "PermissionRequest"
)

// Input is the interface for all hook input types.
type Input interface {
	GetHookEventName() Event
	GetSessionID() string
	GetTranscriptPath() string
	GetCwd() string
	GetPermissionMode() *string
}

// Compile-time verification that all hook input types implement Input.
var (
	_ Input = (*PreToolUseInput)(nil)
	_ Input = (*PostToolUseInput)(nil)
	_ Input = (*UserPromptSubmitInput)(nil)
	_ Input = (*StopInput)(nil)
	_ Input = (*SubagentStopInput)(nil)
	_ Input = (*PreCompactInput)(nil)
	_ Input = (*PostToolUseFailureInput)(nil)
	_ Input = (*NotificationInput)(nil)
	_ Input = (*SubagentStartInput)(nil)
	_ Input = (*PermissionRequestInput)(nil)
)

// BaseInput contains common fields for all hook inputs.
//
//nolint:tagliatelle // CLI uses snake_case
type BaseInput struct {
	SessionID      string  `json:"session_id"`
	TranscriptPath string  `json:"transcript_path"`
	Cwd            string  `json:"cwd"`
	PermissionMode *string `json:"permission_mode,omitempty"`
}

// GetSessionID implements Input.
func (b *BaseInput) GetSessionID() string { return b.SessionID }

// GetTranscriptPath implements Input.
func (b *BaseInput) GetTranscriptPath() string { return b.TranscriptPath }

// GetCwd implements Input.
func (b *BaseInput) GetCwd() string { return b.Cwd }

// GetPermissionMode implements Input.
func (b *BaseInput) GetPermissionMode() *string { return b.PermissionMode }

// PreToolUseInput is the input for PreToolUse hooks.
//
//nolint:tagliatelle // CLI uses snake_case
type PreToolUseInput struct {
	BaseInput
	HookEventName string         `json:"hook_event_name"`
	ToolName      string         `json:"tool_name"`
	ToolInput     map[string]any `json:"tool_input"`
	ToolUseID     string         `json:"tool_use_id"`
}

// GetHookEventName implements Input.
func (p *PreToolUseInput) GetHookEventName() Event { return EventPreToolUse }

// PostToolUseInput is the input for PostToolUse hooks.
//
//nolint:tagliatelle // CLI uses snake_case
type PostToolUseInput struct {
	BaseInput
	HookEventName string         `json:"hook_event_name"`
	ToolName      string         `json:"tool_name"`
	ToolInput     map[string]any `json:"tool_input"`
	ToolUseID     string         `json:"tool_use_id"`
	ToolResponse  any            `json:"tool_response"`
}

// GetHookEventName implements Input.
func (p *PostToolUseInput) GetHookEventName() Event { return EventPostToolUse }

// UserPromptSubmitInput is the input for UserPromptSubmit hooks.
//
//nolint:tagliatelle // CLI uses snake_case
type UserPromptSubmitInput struct {
	BaseInput
	HookEventName string `json:"hook_event_name"`
	Prompt        string `json:"prompt"`
}

// GetHookEventName implements Input.
func (u *UserPromptSubmitInput) GetHookEventName() Event {
	return EventUserPromptSubmit
}

// StopInput is the input for Stop hooks.
//
//nolint:tagliatelle // CLI uses snake_case
type StopInput struct {
	BaseInput
	HookEventName  string `json:"hook_event_name"`
	StopHookActive bool   `json:"stop_hook_active"`
}

// GetHookEventName implements Input.
func (s *StopInput) GetHookEventName() Event { return EventStop }

// SubagentStopInput is the input for SubagentStop hooks.
//
//nolint:tagliatelle // CLI uses snake_case
type SubagentStopInput struct {
	BaseInput
	HookEventName       string `json:"hook_event_name"`
	StopHookActive      bool   `json:"stop_hook_active"`
	AgentID             string `json:"agent_id"`
	AgentTranscriptPath string `json:"agent_transcript_path"`
	AgentType           string `json:"agent_type"`
}

// GetHookEventName implements Input.
func (s *SubagentStopInput) GetHookEventName() Event { return EventSubagentStop }

// PostToolUseFailureInput is the input for PostToolUseFailure hooks.
//
//nolint:tagliatelle // CLI uses snake_case
type PostToolUseFailureInput struct {
	BaseInput
	HookEventName string         `json:"hook_event_name"`
	ToolName      string         `json:"tool_name"`
	ToolInput     map[string]any `json:"tool_input"`
	ToolUseID     string         `json:"tool_use_id"`
	Error         string         `json:"error"`
	IsInterrupt   *bool          `json:"is_interrupt,omitempty"`
}

// GetHookEventName implements Input.
func (p *PostToolUseFailureInput) GetHookEventName() Event { return EventPostToolUseFailure }

// NotificationInput is the input for Notification hooks.
//
//nolint:tagliatelle // CLI uses snake_case
type NotificationInput struct {
	BaseInput
	HookEventName    string  `json:"hook_event_name"`
	Message          string  `json:"message"`
	Title            *string `json:"title,omitempty"`
	NotificationType string  `json:"notification_type"`
}

// GetHookEventName implements Input.
func (n *NotificationInput) GetHookEventName() Event { return EventNotification }

// SubagentStartInput is the input for SubagentStart hooks.
//
//nolint:tagliatelle // CLI uses snake_case
type SubagentStartInput struct {
	BaseInput
	HookEventName string `json:"hook_event_name"`
	AgentID       string `json:"agent_id"`
	AgentType     string `json:"agent_type"`
}

// GetHookEventName implements Input.
func (s *SubagentStartInput) GetHookEventName() Event { return EventSubagentStart }

// PermissionRequestInput is the input for PermissionRequest hooks.
//
//nolint:tagliatelle // CLI uses snake_case
type PermissionRequestInput struct {
	BaseInput
	HookEventName         string         `json:"hook_event_name"`
	ToolName              string         `json:"tool_name"`
	ToolInput             map[string]any `json:"tool_input"`
	PermissionSuggestions []any          `json:"permission_suggestions"`
}

// GetHookEventName implements Input.
func (p *PermissionRequestInput) GetHookEventName() Event { return EventPermissionRequest }

// PreCompactInput is the input for PreCompact hooks.
//
//nolint:tagliatelle // CLI uses snake_case
type PreCompactInput struct {
	BaseInput
	HookEventName      string  `json:"hook_event_name"`
	Trigger            string  `json:"trigger"` // "manual" or "auto"
	CustomInstructions *string `json:"custom_instructions,omitempty"`
}

// GetHookEventName implements Input.
func (p *PreCompactInput) GetHookEventName() Event { return EventPreCompact }

// JSONOutput is the interface for hook output types.
// This is a marker interface for type safety; use type switches to distinguish
// between AsyncJSONOutput and SyncJSONOutput.
type JSONOutput any

// Compile-time verification that hook output types implement JSONOutput.
var (
	_ JSONOutput = (*AsyncJSONOutput)(nil)
	_ JSONOutput = (*SyncJSONOutput)(nil)
)

// AsyncJSONOutput represents an async hook output.
type AsyncJSONOutput struct {
	Async        bool `json:"async"`
	AsyncTimeout *int `json:"asyncTimeout,omitempty"` // milliseconds
}

// SyncJSONOutput represents a sync hook output.
type SyncJSONOutput struct {
	Continue           *bool          `json:"continue,omitempty"`
	SuppressOutput     *bool          `json:"suppressOutput,omitempty"`
	StopReason         *string        `json:"stopReason,omitempty"`
	Decision           *string        `json:"decision,omitempty"` // "block"
	SystemMessage      *string        `json:"systemMessage,omitempty"`
	Reason             *string        `json:"reason,omitempty"`
	HookSpecificOutput SpecificOutput `json:"hookSpecificOutput,omitempty"`
}

// SpecificOutput is the interface for hook-specific outputs.
type SpecificOutput interface {
	GetHookEventName() string
}

// Compile-time verification that hook-specific output types implement SpecificOutput.
var (
	_ SpecificOutput = (*PreToolUseSpecificOutput)(nil)
	_ SpecificOutput = (*PostToolUseSpecificOutput)(nil)
	_ SpecificOutput = (*UserPromptSubmitSpecificOutput)(nil)
	_ SpecificOutput = (*PostToolUseFailureSpecificOutput)(nil)
	_ SpecificOutput = (*NotificationSpecificOutput)(nil)
	_ SpecificOutput = (*SubagentStartSpecificOutput)(nil)
	_ SpecificOutput = (*PermissionRequestSpecificOutput)(nil)
)

// PreToolUseSpecificOutput is the hook-specific output for PreToolUse.
type PreToolUseSpecificOutput struct {
	HookEventName            string         `json:"hookEventName"` // "PreToolUse"
	PermissionDecision       *string        `json:"permissionDecision,omitempty"`
	PermissionDecisionReason *string        `json:"permissionDecisionReason,omitempty"`
	UpdatedInput             map[string]any `json:"updatedInput,omitempty"`
	AdditionalContext        *string        `json:"additionalContext,omitempty"`
}

// GetHookEventName implements SpecificOutput.
func (p *PreToolUseSpecificOutput) GetHookEventName() string { return "PreToolUse" }

// PostToolUseSpecificOutput is the hook-specific output for PostToolUse.
type PostToolUseSpecificOutput struct {
	HookEventName        string  `json:"hookEventName"` // "PostToolUse"
	AdditionalContext    *string `json:"additionalContext,omitempty"`
	UpdatedMCPToolOutput any     `json:"updatedMCPToolOutput,omitempty"` //nolint:tagliatelle // CLI protocol uses MCP acronym
}

// GetHookEventName implements SpecificOutput.
func (p *PostToolUseSpecificOutput) GetHookEventName() string { return "PostToolUse" }

// UserPromptSubmitSpecificOutput is the hook-specific output for UserPromptSubmit.
type UserPromptSubmitSpecificOutput struct {
	HookEventName     string  `json:"hookEventName"` // "UserPromptSubmit"
	AdditionalContext *string `json:"additionalContext,omitempty"`
}

// GetHookEventName implements SpecificOutput.
func (u *UserPromptSubmitSpecificOutput) GetHookEventName() string {
	return "UserPromptSubmit"
}

// PostToolUseFailureSpecificOutput is the hook-specific output for PostToolUseFailure.
type PostToolUseFailureSpecificOutput struct {
	HookEventName     string  `json:"hookEventName"` // "PostToolUseFailure"
	AdditionalContext *string `json:"additionalContext,omitempty"`
}

// GetHookEventName implements SpecificOutput.
func (p *PostToolUseFailureSpecificOutput) GetHookEventName() string {
	return "PostToolUseFailure"
}

// NotificationSpecificOutput is the hook-specific output for Notification.
type NotificationSpecificOutput struct {
	HookEventName     string  `json:"hookEventName"` // "Notification"
	AdditionalContext *string `json:"additionalContext,omitempty"`
}

// GetHookEventName implements SpecificOutput.
func (n *NotificationSpecificOutput) GetHookEventName() string { return "Notification" }

// SubagentStartSpecificOutput is the hook-specific output for SubagentStart.
type SubagentStartSpecificOutput struct {
	HookEventName     string  `json:"hookEventName"` // "SubagentStart"
	AdditionalContext *string `json:"additionalContext,omitempty"`
}

// GetHookEventName implements SpecificOutput.
func (s *SubagentStartSpecificOutput) GetHookEventName() string { return "SubagentStart" }

// PermissionRequestSpecificOutput is the hook-specific output for PermissionRequest.
type PermissionRequestSpecificOutput struct {
	HookEventName string         `json:"hookEventName"` // "PermissionRequest"
	Decision      map[string]any `json:"decision,omitempty"`
}

// GetHookEventName implements SpecificOutput.
func (p *PermissionRequestSpecificOutput) GetHookEventName() string { return "PermissionRequest" }

// Context provides context for hook execution.
type Context struct{}

// Callback is the function signature for hook callbacks.
type Callback func(
	ctx context.Context,
	input Input,
	toolUseID *string,
	hookCtx *Context,
) (JSONOutput, error)

// Matcher configures which tools/events a hook applies to.
type Matcher struct {
	// Matcher is a tool name like "Bash" or a pipe-separated combination like "Write|Edit".
	// When nil, the hook matches all tools/events.
	// This is NOT regex - pipe (|) separates multiple tool names to match.
	Matcher *string
	Hooks   []Callback
	Timeout *float64 // seconds (default 60)
}

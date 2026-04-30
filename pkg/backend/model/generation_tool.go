package model

import (
	"context"
	"encoding/json"

	"github.com/SocialGouv/claw-code-go/pkg/api"
	"github.com/SocialGouv/claw-code-go/pkg/api/hooks"
)

// GenerationTool describes a tool available during direct generation.
// It replaces sdk.Tool for the native generation engine.
type GenerationTool struct {
	// Name is the tool identifier.
	Name string

	// Description describes what the tool does.
	Description string

	// InputSchema is the JSON schema for the tool's input parameters.
	InputSchema json.RawMessage

	// Execute runs the tool with the given JSON input and returns the result text.
	Execute func(ctx context.Context, input json.RawMessage) (string, error)
}

// GenerationOptions configures a direct generation call.
type GenerationOptions struct {
	// Model is the model ID (e.g., "claude-sonnet-4-6").
	Model string

	// System is the system prompt (plain string form).
	System string

	// SystemBlocks, when non-empty, takes precedence over System and is sent
	// as the Anthropic array-form `system` field. This is the only way to
	// attach `cache_control` markers to system content for prompt caching.
	SystemBlocks []api.ContentBlock

	// Messages is the initial conversation history.
	Messages []api.Message

	// Tools available for the model to call.
	Tools []GenerationTool

	// MaxSteps is the maximum number of tool-loop iterations (default 10).
	MaxSteps int

	// MaxTokens is the maximum tokens per response (default 8192).
	MaxTokens int

	// Temperature controls randomness (nil = provider default).
	Temperature *float64

	// ExplicitSchema is the JSON schema for structured output (GenerateObjectDirect).
	ExplicitSchema json.RawMessage

	// SchemaName is the name for the synthetic structured-output tool (default "structured_output").
	SchemaName string

	// ProviderOptions are provider-specific options (e.g., reasoning_effort).
	ProviderOptions map[string]any

	// CompactThresholdRatio overrides the default compaction trigger as a
	// fraction of the model's context window. 0 falls back to the built-in
	// default (0.85). Values outside (0, 1] fall back to the default.
	CompactThresholdRatio float64

	// CompactPreserveRecent overrides the default count of recent messages
	// kept verbatim during compaction. 0 falls back to the built-in default
	// (4). Values < 0 fall back to the default.
	CompactPreserveRecent int

	// --- Hook callbacks ---

	// OnRequest is called before each StreamResponse call.
	OnRequest func(RequestInfo)

	// OnResponse is called after each StreamResponse aggregation completes.
	OnResponse func(ResponseInfo)

	// OnStepFinish is called after each tool-loop step completes.
	OnStepFinish func(StepResult)

	// OnToolCall is called after each tool execution.
	OnToolCall func(ToolCallInfo)

	// OnCompact is called once after each in-loop compaction round
	// that actually shrunk the message history. No-op compactions
	// (transcript too short) do not fire the callback.
	OnCompact func(CompactInfo)

	// Hooks, when non-nil, is consulted around tool execution and at
	// session end. PreToolUse fires before each Execute and may Block
	// (the tool returns a synthetic refusal). PostToolUse fires after
	// successful Execute, PostToolUseFailure fires on error. Stop
	// fires once when the generation loop exits (success or failure).
	Hooks *hooks.Runner
}

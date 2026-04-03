package config

import (
	"context"
	"log/slog"
	"time"

	"github.com/ethpandaops/codex-agent-sdk-go/internal/hook"
	"github.com/ethpandaops/codex-agent-sdk-go/internal/mcp"
	"github.com/ethpandaops/codex-agent-sdk-go/internal/permission"
	"github.com/ethpandaops/codex-agent-sdk-go/internal/userinput"
)

// DynamicTool defines a tool registered via the dynamicTools API.
// The Codex CLI discovers these tools at thread/start and calls them back
// via item/tool/call RPC using the plain tool name.
type DynamicTool struct {
	Name        string
	Description string
	InputSchema map[string]any
	Handler     func(ctx context.Context, input map[string]any) (map[string]any, error)
}

// Effort controls thinking depth.
type Effort string

const (
	// EffortLow uses minimal thinking.
	EffortLow Effort = "low"
	// EffortMedium uses moderate thinking.
	EffortMedium Effort = "medium"
	// EffortHigh uses deep thinking.
	EffortHigh Effort = "high"
	// EffortMax uses maximum thinking depth.
	EffortMax Effort = "xhigh"
	// EffortNone disables reasoning entirely.
	EffortNone Effort = "none"
	// EffortMinimal uses minimal reasoning.
	EffortMinimal Effort = "minimal"
)

// ThinkingConfig controls extended thinking behavior.
// Implementations: ThinkingConfigAdaptive, ThinkingConfigEnabled, ThinkingConfigDisabled.
type ThinkingConfig interface {
	thinkingConfig() // marker method
}

// ThinkingConfigAdaptive enables adaptive thinking mode.
type ThinkingConfigAdaptive struct{}

func (ThinkingConfigAdaptive) thinkingConfig() {}

// ThinkingConfigEnabled enables thinking with a specific token budget.
type ThinkingConfigEnabled struct {
	BudgetTokens int
}

func (ThinkingConfigEnabled) thinkingConfig() {}

// ThinkingConfigDisabled disables extended thinking.
type ThinkingConfigDisabled struct{}

func (ThinkingConfigDisabled) thinkingConfig() {}

// Options configures the behavior of the Codex agent.
type Options struct {
	// Logger is the slog logger for debug output.
	// If nil, logging is disabled (silent operation).
	Logger *slog.Logger

	// SystemPrompt is the system message to send to the agent.
	SystemPrompt string

	// SystemPromptPreset specifies a preset system prompt configuration.
	// If set, this takes precedence over SystemPrompt.
	SystemPromptPreset *SystemPromptPreset

	// Model specifies which model to use.
	Model string

	// PermissionMode controls how permissions are handled.
	// Supported values are "default", "acceptEdits", "plan", and
	// "bypassPermissions".
	PermissionMode string

	// Cwd sets the working directory for the CLI process.
	Cwd string

	// CliPath is the explicit path to the codex CLI binary.
	// If empty, the CLI will be searched in PATH.
	CliPath string

	// Env provides additional environment variables for the CLI process.
	Env map[string]string

	// Hooks configures event hooks for tool interception.
	// Hooks are registered via protocol session and dispatched when Codex CLI sends
	// hooks/callback requests.
	Hooks map[hook.Event][]*hook.Matcher

	// Effort controls thinking depth.
	// Passed to CLI via initialization; support depends on Codex CLI version.
	Effort *Effort

	// Personality controls the agent's response personality.
	// Valid values: "none", "friendly", "pragmatic".
	Personality string

	// ServiceTier controls the service tier for API requests.
	// Valid values: "fast", "flex".
	ServiceTier string

	// DeveloperInstructions provides additional instructions for the agent.
	// This is separate from SystemPrompt and is passed as developerInstructions
	// in the thread/start payload.
	DeveloperInstructions string

	// MCPServers configures external MCP servers to connect to.
	// SDK MCP servers are registered and respond to mcp_message requests from CLI.
	MCPServers map[string]mcp.ServerConfig

	// SDKTools holds dynamic tools registered via WithSDKTools.
	// These are serialized as dynamicTools in the thread/start payload and
	// dispatched via item/tool/call RPC using plain tool names.
	SDKTools []*DynamicTool

	// CanUseTool is called before each tool use for permission checking.
	// Permission callback invoked when CLI sends can_use_tool requests via protocol.
	CanUseTool permission.Callback

	// OnUserInput is called when the CLI sends item/tool/requestUserInput requests.
	// This callback allows the SDK consumer to answer questions posed by the agent
	// (e.g., multiple-choice or free-text prompts in plan mode).
	OnUserInput userinput.Callback

	// ===== CLAUDE SDK PARITY FIELDS =====

	// Tools specifies which tools are available.
	Tools ToolsConfig

	// AllowedTools is a list of pre-approved tools.
	AllowedTools []string

	// DisallowedTools is a list of explicitly blocked tools.
	DisallowedTools []string

	// PermissionPromptToolName specifies the tool name for permission prompts.
	PermissionPromptToolName string

	// AddDirs is a list of additional directories to make accessible.
	AddDirs []string

	// ExtraArgs provides arbitrary CLI flags.
	ExtraArgs map[string]*string

	// Stderr is a callback function for handling stderr output.
	Stderr func(string)

	// ContinueConversation indicates whether to continue an existing conversation.
	ContinueConversation bool

	// Resume is a session ID to resume from.
	Resume string

	// ForkSession indicates whether to fork the resumed session.
	ForkSession bool

	// OutputFormat specifies a JSON schema for structured output.
	OutputFormat map[string]any

	// InitializeTimeout is the timeout for the initialize control request.
	InitializeTimeout *time.Duration

	// Transport allows injecting a custom transport implementation.
	// If nil, the default CLITransport is created automatically.
	Transport Transport `json:"-"`

	// ===== CODEX-NATIVE FIELDS =====

	// Sandbox sets the Codex sandbox mode (read-only, workspace-write, danger-full-access).
	Sandbox string

	// Images lists file paths for image inputs (passed via -i flags).
	Images []string

	// Config holds key=value pairs for Codex CLI configuration (passed via -c flags).
	Config map[string]string

	// OutputSchema sets the --output-schema flag for structured Codex output.
	OutputSchema string

	// SkipVersionCheck disables CLI version validation during discovery.
	SkipVersionCheck bool

	// IncludePartialMessages controls whether streaming deltas are emitted
	// as StreamEvent messages. When false (default), only completed
	// AssistantMessage and ResultMessage are emitted. When true, token-by-token
	// deltas are emitted as StreamEvent with content_block_delta/text_delta shape.
	IncludePartialMessages bool

	// CodexHome overrides the Codex home directory (default ~/.codex).
	// Used by StatSession to locate the session database.
	CodexHome string
}

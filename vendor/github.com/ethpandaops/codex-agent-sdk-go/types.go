package codexsdk

import (
	"iter"

	"github.com/ethpandaops/codex-agent-sdk-go/internal/config"
	"github.com/ethpandaops/codex-agent-sdk-go/internal/hook"
	"github.com/ethpandaops/codex-agent-sdk-go/internal/mcp"
	"github.com/ethpandaops/codex-agent-sdk-go/internal/message"
	"github.com/ethpandaops/codex-agent-sdk-go/internal/model"
	"github.com/ethpandaops/codex-agent-sdk-go/internal/permission"
	"github.com/ethpandaops/codex-agent-sdk-go/internal/sandbox"
	"github.com/ethpandaops/codex-agent-sdk-go/internal/userinput"
)

// Re-export types from internal packages

// ===== Transport =====

// Transport defines the interface for Codex CLI communication.
// Re-exported from internal/config for public API access.
// See transport.go for full documentation.
// type Transport = config.Transport (defined in transport.go)

// ===== Options and Configuration =====

// CodexAgentOptions configures the behavior of the Codex agent.
type CodexAgentOptions = config.Options

// SdkBeta represents a beta feature flag for the SDK.
type SdkBeta = config.Beta

const (
	// SdkBetaContext1M enables 1 million token context window.
	SdkBetaContext1M = config.BetaContext1M
)

// SettingSource represents where settings should be loaded from.
type SettingSource = config.SettingSource

const (
	// SettingSourceUser loads from user-level settings.
	SettingSourceUser = config.SettingSourceUser
	// SettingSourceProject loads from project-level settings.
	SettingSourceProject = config.SettingSourceProject
	// SettingSourceLocal loads from local-level settings.
	SettingSourceLocal = config.SettingSourceLocal
)

// ===== Thinking Configuration =====

// ThinkingConfig controls extended thinking behavior.
type ThinkingConfig = config.ThinkingConfig

// ThinkingConfigAdaptive enables adaptive thinking mode.
type ThinkingConfigAdaptive = config.ThinkingConfigAdaptive

// ThinkingConfigEnabled enables thinking with a specific token budget.
type ThinkingConfigEnabled = config.ThinkingConfigEnabled

// ThinkingConfigDisabled disables extended thinking.
type ThinkingConfigDisabled = config.ThinkingConfigDisabled

// Effort controls thinking depth.
type Effort = config.Effort

const (
	// EffortLow uses minimal thinking.
	EffortLow = config.EffortLow
	// EffortMedium uses moderate thinking.
	EffortMedium = config.EffortMedium
	// EffortHigh uses deep thinking.
	EffortHigh = config.EffortHigh
	// EffortMax uses maximum thinking depth.
	EffortMax = config.EffortMax
	// EffortNone disables reasoning entirely.
	EffortNone = config.EffortNone
	// EffortMinimal uses minimal reasoning.
	EffortMinimal = config.EffortMinimal
)

// AgentDefinition defines a custom agent configuration.
type AgentDefinition = config.AgentDefinition

// SystemPromptPreset defines a system prompt preset configuration.
type SystemPromptPreset = config.SystemPromptPreset

// SdkPluginConfig configures a plugin to load.
type SdkPluginConfig = config.PluginConfig

// ToolsPreset represents a preset configuration for available tools.
type ToolsPreset = config.ToolsPreset

// ToolsConfig is an interface for configuring available tools.
// It represents either a list of tool names or a preset configuration.
type ToolsConfig = config.ToolsConfig

// ToolsList is a list of tool names to make available.
type ToolsList = config.ToolsList

// ===== Messages =====

// Message represents any message in the conversation.
type Message = message.Message

// UserMessage represents a message from the user.
type UserMessage = message.UserMessage

// UserMessageContent represents content that can be either a string or []ContentBlock.
type UserMessageContent = message.UserMessageContent

// NewUserMessageContent creates UserMessageContent from a string.
var NewUserMessageContent = message.NewUserMessageContent

// NewUserMessageContentBlocks creates UserMessageContent from blocks.
var NewUserMessageContentBlocks = message.NewUserMessageContentBlocks

// AssistantMessage represents a message from the agent.
type AssistantMessage = message.AssistantMessage

// AssistantMessageError represents error types from the assistant.
type AssistantMessageError = message.AssistantMessageError

const (
	// AssistantMessageErrorAuthFailed indicates authentication failure.
	AssistantMessageErrorAuthFailed = message.AssistantMessageErrorAuthFailed
	// AssistantMessageErrorBilling indicates a billing error.
	AssistantMessageErrorBilling = message.AssistantMessageErrorBilling
	// AssistantMessageErrorRateLimit indicates rate limiting.
	AssistantMessageErrorRateLimit = message.AssistantMessageErrorRateLimit
	// AssistantMessageErrorInvalidReq indicates an invalid request.
	AssistantMessageErrorInvalidReq = message.AssistantMessageErrorInvalidReq
	// AssistantMessageErrorServer indicates a server error.
	AssistantMessageErrorServer = message.AssistantMessageErrorServer
	// AssistantMessageErrorUnknown indicates an unknown error.
	AssistantMessageErrorUnknown = message.AssistantMessageErrorUnknown
)

// SystemMessage represents a system message.
type SystemMessage = message.SystemMessage

// TaskStartedMessage is emitted when a Codex turn starts.
type TaskStartedMessage = message.TaskStartedMessage

// TaskCompleteMessage is emitted when a Codex turn completes.
type TaskCompleteMessage = message.TaskCompleteMessage

// ThreadRolledBackMessage is emitted when a thread rollback event is observed.
type ThreadRolledBackMessage = message.ThreadRolledBackMessage

// ResultMessage represents the final result of a query.
type ResultMessage = message.ResultMessage

// StreamEvent represents a streaming event from the API.
type StreamEvent = message.StreamEvent

// Usage contains token usage information.
type Usage = message.Usage

// ===== Content Blocks =====

const (
	// BlockTypeText is a plain text user content block.
	BlockTypeText = message.BlockTypeText
	// BlockTypeImage is an app-server image URL/data-URL content block.
	BlockTypeImage = message.BlockTypeImage
	// BlockTypeLocalImage is a local image-path content block.
	BlockTypeLocalImage = message.BlockTypeLocalImage
	// BlockTypeMention is a local path mention content block.
	BlockTypeMention = message.BlockTypeMention
	// BlockTypeThinking is an assistant reasoning content block.
	BlockTypeThinking = message.BlockTypeThinking
	// BlockTypeToolUse is a tool-use content block.
	BlockTypeToolUse = message.BlockTypeToolUse
	// BlockTypeToolResult is a tool-result content block.
	BlockTypeToolResult = message.BlockTypeToolResult
)

// ContentBlock represents a block of content within a message.
type ContentBlock = message.ContentBlock

// TextBlock contains plain text content.
type TextBlock = message.TextBlock

// InputImageBlock contains an app-server image input reference.
type InputImageBlock = message.InputImageBlock

// InputLocalImageBlock contains a local image-path input reference.
type InputLocalImageBlock = message.InputLocalImageBlock

// InputMentionBlock contains a generic local path mention.
type InputMentionBlock = message.InputMentionBlock

// ThinkingBlock contains the agent's thinking process.
type ThinkingBlock = message.ThinkingBlock

// ToolUseBlock represents the agent using a tool.
type ToolUseBlock = message.ToolUseBlock

// ToolResultBlock contains the result of a tool execution.
type ToolResultBlock = message.ToolResultBlock

// UnknownBlock preserves unrecognized content blocks without losing data.
type UnknownBlock = message.UnknownBlock

// ===== Hooks =====

// HookEvent represents the type of event that triggers a hook.
type HookEvent = hook.Event

const (
	// HookEventPreToolUse is triggered before a tool is used.
	HookEventPreToolUse = hook.EventPreToolUse
	// HookEventPostToolUse is triggered after a tool is used.
	HookEventPostToolUse = hook.EventPostToolUse
	// HookEventUserPromptSubmit is triggered when a user submits a prompt.
	HookEventUserPromptSubmit = hook.EventUserPromptSubmit
	// HookEventStop is triggered when a session stops.
	HookEventStop = hook.EventStop
	// HookEventSubagentStop is triggered when a subagent stops.
	HookEventSubagentStop = hook.EventSubagentStop
	// HookEventPreCompact is triggered before compaction.
	HookEventPreCompact = hook.EventPreCompact
	// HookEventPostToolUseFailure is triggered after a tool use fails.
	HookEventPostToolUseFailure = hook.EventPostToolUseFailure
	// HookEventNotification is triggered when a notification is sent.
	HookEventNotification = hook.EventNotification
	// HookEventSubagentStart is triggered when a subagent starts.
	HookEventSubagentStart = hook.EventSubagentStart
	// HookEventPermissionRequest is triggered when a permission is requested.
	HookEventPermissionRequest = hook.EventPermissionRequest
)

// HookInput is the interface for all hook input types.
type HookInput = hook.Input

// BaseHookInput contains common fields for all hook inputs.
type BaseHookInput = hook.BaseInput

// PreToolUseHookInput is the input for PreToolUse hooks.
type PreToolUseHookInput = hook.PreToolUseInput

// PostToolUseHookInput is the input for PostToolUse hooks.
type PostToolUseHookInput = hook.PostToolUseInput

// UserPromptSubmitHookInput is the input for UserPromptSubmit hooks.
type UserPromptSubmitHookInput = hook.UserPromptSubmitInput

// StopHookInput is the input for Stop hooks.
type StopHookInput = hook.StopInput

// SubagentStopHookInput is the input for SubagentStop hooks.
type SubagentStopHookInput = hook.SubagentStopInput

// PreCompactHookInput is the input for PreCompact hooks.
type PreCompactHookInput = hook.PreCompactInput

// PostToolUseFailureHookInput is the input for PostToolUseFailure hooks.
type PostToolUseFailureHookInput = hook.PostToolUseFailureInput

// NotificationHookInput is the input for Notification hooks.
type NotificationHookInput = hook.NotificationInput

// SubagentStartHookInput is the input for SubagentStart hooks.
type SubagentStartHookInput = hook.SubagentStartInput

// PermissionRequestHookInput is the input for PermissionRequest hooks.
type PermissionRequestHookInput = hook.PermissionRequestInput

// HookJSONOutput is the interface for hook output types.
type HookJSONOutput = hook.JSONOutput

// AsyncHookJSONOutput represents an async hook output.
type AsyncHookJSONOutput = hook.AsyncJSONOutput

// SyncHookJSONOutput represents a sync hook output.
type SyncHookJSONOutput = hook.SyncJSONOutput

// HookSpecificOutput is the interface for hook-specific outputs.
type HookSpecificOutput = hook.SpecificOutput

// PreToolUseHookSpecificOutput is the hook-specific output for PreToolUse.
type PreToolUseHookSpecificOutput = hook.PreToolUseSpecificOutput

// PostToolUseHookSpecificOutput is the hook-specific output for PostToolUse.
type PostToolUseHookSpecificOutput = hook.PostToolUseSpecificOutput

// UserPromptSubmitHookSpecificOutput is the hook-specific output for UserPromptSubmit.
type UserPromptSubmitHookSpecificOutput = hook.UserPromptSubmitSpecificOutput

// PostToolUseFailureHookSpecificOutput is the hook-specific output for PostToolUseFailure.
type PostToolUseFailureHookSpecificOutput = hook.PostToolUseFailureSpecificOutput

// NotificationHookSpecificOutput is the hook-specific output for Notification.
type NotificationHookSpecificOutput = hook.NotificationSpecificOutput

// SubagentStartHookSpecificOutput is the hook-specific output for SubagentStart.
type SubagentStartHookSpecificOutput = hook.SubagentStartSpecificOutput

// PermissionRequestHookSpecificOutput is the hook-specific output for PermissionRequest.
type PermissionRequestHookSpecificOutput = hook.PermissionRequestSpecificOutput

// HookContext provides context for hook execution.
type HookContext = hook.Context

// HookCallback is the function signature for hook callbacks.
type HookCallback = hook.Callback

// HookMatcher configures which tools/events a hook applies to.
type HookMatcher = hook.Matcher

// ===== Permissions =====

// PermissionMode represents different permission handling modes.
type PermissionMode = permission.Mode

const (
	// PermissionModeDefault uses standard permission prompts.
	PermissionModeDefault = permission.ModeDefault
	// PermissionModeAcceptEdits automatically accepts file edits.
	PermissionModeAcceptEdits = permission.ModeAcceptEdits
	// PermissionModePlan enables plan mode for implementation planning.
	PermissionModePlan = permission.ModePlan
	// PermissionModeBypassPermissions bypasses all permission checks.
	PermissionModeBypassPermissions = permission.ModeBypassPermissions
)

// PermissionUpdateType represents the type of permission update.
type PermissionUpdateType = permission.UpdateType

const (
	// PermissionUpdateTypeAddRules adds new permission rules.
	PermissionUpdateTypeAddRules = permission.UpdateTypeAddRules
	// PermissionUpdateTypeReplaceRules replaces existing permission rules.
	PermissionUpdateTypeReplaceRules = permission.UpdateTypeReplaceRules
	// PermissionUpdateTypeRemoveRules removes permission rules.
	PermissionUpdateTypeRemoveRules = permission.UpdateTypeRemoveRules
	// PermissionUpdateTypeSetMode sets the permission mode.
	PermissionUpdateTypeSetMode = permission.UpdateTypeSetMode
	// PermissionUpdateTypeAddDirectories adds accessible directories.
	PermissionUpdateTypeAddDirectories = permission.UpdateTypeAddDirectories
	// PermissionUpdateTypeRemoveDirectories removes accessible directories.
	PermissionUpdateTypeRemoveDirectories = permission.UpdateTypeRemoveDirectories
)

// PermissionUpdateDestination represents where permission updates are stored.
type PermissionUpdateDestination = permission.UpdateDestination

const (
	// PermissionUpdateDestUserSettings stores in user-level settings.
	PermissionUpdateDestUserSettings = permission.UpdateDestUserSettings
	// PermissionUpdateDestProjectSettings stores in project-level settings.
	PermissionUpdateDestProjectSettings = permission.UpdateDestProjectSettings
	// PermissionUpdateDestLocalSettings stores in local-level settings.
	PermissionUpdateDestLocalSettings = permission.UpdateDestLocalSettings
	// PermissionUpdateDestSession stores in the current session only.
	PermissionUpdateDestSession = permission.UpdateDestSession
)

// PermissionBehavior represents the permission behavior for a rule.
type PermissionBehavior = permission.Behavior

const (
	// PermissionBehaviorAllow automatically allows the operation.
	PermissionBehaviorAllow = permission.BehaviorAllow
	// PermissionBehaviorDeny automatically denies the operation.
	PermissionBehaviorDeny = permission.BehaviorDeny
	// PermissionBehaviorAsk prompts the user for permission.
	PermissionBehaviorAsk = permission.BehaviorAsk
)

// PermissionRuleValue represents a permission rule.
type PermissionRuleValue = permission.RuleValue

// PermissionUpdate represents a permission update request.
type PermissionUpdate = permission.Update

// ToolPermissionContext provides context for tool permission callbacks.
type ToolPermissionContext = permission.Context

// PermissionResult is the interface for permission decision results.
type PermissionResult = permission.Result

// PermissionResultAllow represents an allow decision.
type PermissionResultAllow = permission.ResultAllow

// PermissionResultDeny represents a deny decision.
type PermissionResultDeny = permission.ResultDeny

// ToolPermissionCallback is called before each tool use for permission checking.
type ToolPermissionCallback = permission.Callback

// ===== User Input =====

// UserInputQuestionOption represents a selectable choice within a question.
type UserInputQuestionOption = userinput.QuestionOption

// UserInputQuestion represents a single question posed to the user.
type UserInputQuestion = userinput.Question

// UserInputAnswer contains the user's response(s) to a question.
type UserInputAnswer = userinput.Answer

// UserInputRequest represents the full user input request from the CLI.
type UserInputRequest = userinput.Request

// UserInputResponse contains the answers to all questions keyed by question ID.
type UserInputResponse = userinput.Response

// UserInputCallback is invoked when the CLI sends an item/tool/requestUserInput request.
type UserInputCallback = userinput.Callback

// ===== MCP Server Configuration =====

// MCPServerType represents the type of MCP server.
type MCPServerType = mcp.ServerType

const (
	// MCPServerTypeStdio uses stdio for communication.
	MCPServerTypeStdio = mcp.ServerTypeStdio
	// MCPServerTypeSSE uses Server-Sent Events.
	MCPServerTypeSSE = mcp.ServerTypeSSE
	// MCPServerTypeHTTP uses HTTP for communication.
	MCPServerTypeHTTP = mcp.ServerTypeHTTP
	// MCPServerTypeSDK uses the SDK interface.
	MCPServerTypeSDK = mcp.ServerTypeSDK
)

// MCPServerConfig is the interface for MCP server configurations.
type MCPServerConfig = mcp.ServerConfig

// MCPStdioServerConfig configures a stdio-based MCP server.
type MCPStdioServerConfig = mcp.StdioServerConfig

// MCPSSEServerConfig configures a Server-Sent Events MCP server.
type MCPSSEServerConfig = mcp.SSEServerConfig

// MCPHTTPServerConfig configures an HTTP-based MCP server.
type MCPHTTPServerConfig = mcp.HTTPServerConfig

// MCPSdkServerConfig configures an SDK-provided MCP server.
type MCPSdkServerConfig = mcp.SdkServerConfig

// SdkMcpServerInstance is the interface that SDK MCP servers must implement.
type SdkMcpServerInstance = mcp.ServerInstance

// ===== Model Discovery =====

// ModelInfo describes a model available from the Codex CLI.
type ModelInfo = model.Info

// ReasoningEffortOption describes a selectable reasoning effort level.
type ReasoningEffortOption = model.ReasoningEffortOption

// ModelListResponse is the response payload from the model/list RPC method.
type ModelListResponse = model.ListResponse

// ===== MCP Status =====

// MCPServerStatus represents the connection status of a single MCP server.
type MCPServerStatus = mcp.ServerStatus

// MCPAuthStatus represents the authentication state of an MCP server.
type MCPAuthStatus = mcp.AuthStatus

const (
	// MCPAuthStatusUnsupported means the server does not use authentication.
	MCPAuthStatusUnsupported = mcp.AuthStatusUnsupported
	// MCPAuthStatusNotLoggedIn means the server requires login before use.
	MCPAuthStatusNotLoggedIn = mcp.AuthStatusNotLoggedIn
	// MCPAuthStatusBearerToken means the server is authenticated with a bearer token.
	MCPAuthStatusBearerToken = mcp.AuthStatusBearerToken
	// MCPAuthStatusOAuth means the server is authenticated with OAuth.
	MCPAuthStatusOAuth = mcp.AuthStatusOAuth
)

// MCPTool describes an MCP tool exposed by a server.
type MCPTool = mcp.Tool

// MCPResource describes a concrete MCP resource.
type MCPResource = mcp.Resource

// MCPResourceTemplate describes a parameterized MCP resource.
type MCPResourceTemplate = mcp.ResourceTemplate

// MCPStatus represents the connection status of all configured MCP servers.
type MCPStatus = mcp.Status

// ===== Sandbox Configuration =====

// SandboxNetworkConfig configures network access for the sandbox.
type SandboxNetworkConfig = sandbox.NetworkConfig

// SandboxIgnoreViolations configures which violations to ignore.
type SandboxIgnoreViolations = sandbox.IgnoreViolations

// SandboxSettings configures CLI sandbox behavior.
type SandboxSettings = sandbox.Settings

// ===== Streaming Input =====

// MessageStream is an iterator that yields streaming messages.
type MessageStream = iter.Seq[StreamingMessage]

// StreamingMessage represents a message sent in streaming mode.
type StreamingMessage = message.StreamingMessage

// StreamingMessageContent represents the content of a streaming message.
type StreamingMessageContent = message.StreamingMessageContent

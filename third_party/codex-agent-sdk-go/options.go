package codexsdk

import (
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/ethpandaops/codex-agent-sdk-go/internal/config"
)

// Option configures CodexAgentOptions using the functional options pattern.
type Option func(*CodexAgentOptions)

// applyAgentOptions applies functional options to a CodexAgentOptions struct.
func applyAgentOptions(opts []Option) *CodexAgentOptions {
	options := &CodexAgentOptions{}
	for _, opt := range opts {
		opt(options)
	}

	return options
}

// ===== Basic Configuration =====

// WithLogger sets the logger for debug output.
// If not set, logging is disabled (silent operation).
func WithLogger(logger *slog.Logger) Option {
	return func(o *CodexAgentOptions) {
		o.Logger = logger
	}
}

// WithAgentLogger is an alias for WithLogger.
//
// Deprecated: Use WithLogger instead.
var WithAgentLogger = WithLogger

// WithSystemPrompt sets the system message to send to the agent.
func WithSystemPrompt(prompt string) Option {
	return func(o *CodexAgentOptions) {
		o.SystemPrompt = prompt
	}
}

// WithSystemPromptPreset sets a preset system prompt configuration.
// If set, this takes precedence over WithSystemPrompt.
func WithSystemPromptPreset(preset *SystemPromptPreset) Option {
	return func(o *CodexAgentOptions) {
		o.SystemPromptPreset = preset
	}
}

// WithModel specifies which model to use.
func WithModel(model string) Option {
	return func(o *CodexAgentOptions) {
		o.Model = model
	}
}

// WithPermissionMode controls how permissions are handled.
// Supported values are "default", "acceptEdits", "plan", and
// "bypassPermissions".
func WithPermissionMode(mode string) Option {
	return func(o *CodexAgentOptions) {
		o.PermissionMode = mode
	}
}

// WithMaxTurns sets the maximum number of conversation turns.
func WithMaxTurns(maxTurns int) Option {
	return func(o *CodexAgentOptions) {
		o.MaxTurns = maxTurns
	}
}

// WithCwd sets the working directory for the CLI process.
func WithCwd(cwd string) Option {
	return func(o *CodexAgentOptions) {
		o.Cwd = cwd
	}
}

// WithCliPath sets the explicit path to the codex CLI binary.
// If not set, the CLI will be searched in PATH.
func WithCliPath(path string) Option {
	return func(o *CodexAgentOptions) {
		o.CliPath = path
	}
}

// WithEnv provides additional environment variables for the CLI process.
func WithEnv(env map[string]string) Option {
	return func(o *CodexAgentOptions) {
		o.Env = env
	}
}

// ===== Hooks =====

// WithHooks configures event hooks for tool interception.
// Hooks are registered via protocol session and dispatched when Codex CLI sends
// hooks/callback requests.
func WithHooks(hooks map[HookEvent][]*HookMatcher) Option {
	return func(o *CodexAgentOptions) {
		o.Hooks = hooks
	}
}

// ===== Token/Budget =====

// WithEffort sets the thinking effort level.
// Passed to CLI via initialization; support depends on Codex CLI version.
func WithEffort(effort config.Effort) Option {
	return func(o *CodexAgentOptions) {
		o.Effort = &effort
	}
}

// ===== Personality / Service Tier =====

// WithPersonality sets the agent's response personality.
// Valid values: "none", "friendly", "pragmatic".
func WithPersonality(personality string) Option {
	return func(o *CodexAgentOptions) {
		o.Personality = personality
	}
}

// WithServiceTier sets the service tier for API requests.
// Valid values: "fast", "flex".
func WithServiceTier(tier string) Option {
	return func(o *CodexAgentOptions) {
		o.ServiceTier = tier
	}
}

// WithDeveloperInstructions provides additional instructions for the agent.
// This is separate from WithSystemPrompt and maps to the developerInstructions
// field in the Codex CLI protocol.
func WithDeveloperInstructions(instructions string) Option {
	return func(o *CodexAgentOptions) {
		o.DeveloperInstructions = instructions
	}
}

// ===== MCP =====

// WithMCPServers configures external MCP servers to connect to.
// Map key is the server name, value is the server configuration.
func WithMCPServers(servers map[string]MCPServerConfig) Option {
	return func(o *CodexAgentOptions) {
		o.MCPServers = servers
	}
}

// ===== Tools =====

// WithTools specifies which tools are available.
// Accepts ToolsList (tool names) or *ToolsPreset.
func WithTools(tools config.ToolsConfig) Option {
	return func(o *CodexAgentOptions) {
		o.Tools = tools
	}
}

// WithAllowedTools sets pre-approved tools that can be used without prompting.
func WithAllowedTools(tools ...string) Option {
	return func(o *CodexAgentOptions) {
		o.AllowedTools = tools
	}
}

// WithSDKTools registers high-level Tool instances as dynamic tools.
// Tools are serialized in the thread/start payload as dynamicTools and
// called back via item/tool/call RPC using plain tool names.
// Each tool is automatically added to AllowedTools.
func WithSDKTools(tools ...Tool) Option {
	return func(o *CodexAgentOptions) {
		if len(tools) == 0 {
			return
		}

		for _, t := range tools {
			o.SDKTools = append(o.SDKTools, &config.DynamicTool{
				Name:        t.Name(),
				Description: t.Description(),
				InputSchema: t.InputSchema(),
				Handler:     t.Execute,
			})
			o.AllowedTools = append(o.AllowedTools, t.Name())
		}
	}
}

// WithDisallowedTools sets tools that are explicitly blocked.
func WithDisallowedTools(tools ...string) Option {
	return func(o *CodexAgentOptions) {
		o.DisallowedTools = tools
	}
}

// WithCanUseTool sets a callback for permission checking before each tool use.
// Permission callback invoked when CLI sends can_use_tool requests via protocol.
func WithCanUseTool(callback ToolPermissionCallback) Option {
	return func(o *CodexAgentOptions) {
		o.CanUseTool = callback
	}
}

// WithOnUserInput sets a callback for handling user input requests from the CLI.
// The callback is invoked when the agent sends item/tool/requestUserInput requests,
// allowing the SDK consumer to answer multiple-choice or free-text questions
// (e.g., in plan mode).
func WithOnUserInput(callback UserInputCallback) Option {
	return func(o *CodexAgentOptions) {
		o.OnUserInput = callback
	}
}

// ===== Elicitation =====

// WithOnElicitation sets a callback for handling MCP elicitation requests.
// The callback is invoked when an MCP server sends an elicitation/create request
// through the CLI, allowing the SDK consumer to present forms or collect input.
// If not set, elicitation requests are auto-declined.
func WithOnElicitation(callback ElicitationCallback) Option {
	return func(o *CodexAgentOptions) {
		o.OnElicitation = callback
	}
}

// ===== Session =====

// WithContinueConversation indicates whether to continue an existing conversation.
func WithContinueConversation(cont bool) Option {
	return func(o *CodexAgentOptions) {
		o.ContinueConversation = cont
	}
}

// WithResume sets a session ID to resume from.
func WithResume(sessionID string) Option {
	return func(o *CodexAgentOptions) {
		o.Resume = sessionID
	}
}

// WithForkSession indicates whether to fork the resumed session to a new ID.
func WithForkSession(fork bool) Option {
	return func(o *CodexAgentOptions) {
		o.ForkSession = fork
	}
}

// ===== Advanced =====

// WithPermissionPromptToolName specifies the tool name to use for permission prompts.
func WithPermissionPromptToolName(name string) Option {
	return func(o *CodexAgentOptions) {
		o.PermissionPromptToolName = name
	}
}

// WithAddDirs adds additional directories to make accessible.
func WithAddDirs(dirs ...string) Option {
	return func(o *CodexAgentOptions) {
		o.AddDirs = dirs
	}
}

// WithExtraArgs provides arbitrary CLI flags to pass to the CLI.
// If the value is nil, the flag is passed without a value (boolean flag).
func WithExtraArgs(args map[string]*string) Option {
	return func(o *CodexAgentOptions) {
		o.ExtraArgs = args
	}
}

// WithStderr sets a callback function for handling stderr output.
func WithStderr(handler func(string)) Option {
	return func(o *CodexAgentOptions) {
		o.Stderr = handler
	}
}

// WithOutputFormat specifies a JSON schema for structured output.
//
// The canonical format uses a wrapper object:
//
//	codexsdk.WithOutputFormat(map[string]any{
//	    "type": "json_schema",
//	    "schema": map[string]any{
//	        "type":       "object",
//	        "properties": map[string]any{...},
//	        "required":   []string{...},
//	    },
//	})
//
// Raw JSON schemas (without the wrapper) are also accepted and auto-wrapped:
//
//	codexsdk.WithOutputFormat(map[string]any{
//	    "type":       "object",
//	    "properties": map[string]any{...},
//	    "required":   []string{...},
//	})
func WithOutputFormat(format map[string]any) Option {
	return func(o *CodexAgentOptions) {
		o.OutputFormat = format
	}
}

// WithInitializeTimeout sets the timeout for the initialize control request.
func WithInitializeTimeout(timeout time.Duration) Option {
	return func(o *CodexAgentOptions) {
		o.InitializeTimeout = &timeout
	}
}

// WithTransport injects a custom transport implementation.
// The transport must implement the Transport interface.
func WithTransport(transport config.Transport) Option {
	return func(o *CodexAgentOptions) {
		o.Transport = transport
	}
}

// ===== Codex-Native Options =====

// WithSandbox sets the Codex sandbox mode directly.
// Valid values: "read-only", "workspace-write", "danger-full-access".
func WithSandbox(sandbox string) Option {
	return func(o *CodexAgentOptions) {
		o.Sandbox = sandbox
	}
}

// WithImages provides file paths for image inputs (passed via -i flags).
func WithImages(images ...string) Option {
	return func(o *CodexAgentOptions) {
		o.Images = images
	}
}

// WithConfig provides key=value pairs for Codex CLI configuration (passed via -c flags).
func WithConfig(cfg map[string]string) Option {
	return func(o *CodexAgentOptions) {
		o.Config = cfg
	}
}

// WithOutputSchema sets the --output-schema flag for structured Codex output.
func WithOutputSchema(schema string) Option {
	return func(o *CodexAgentOptions) {
		o.OutputSchema = schema
	}
}

// WithSkipVersionCheck disables CLI version validation during discovery.
func WithSkipVersionCheck(skip bool) Option {
	return func(o *CodexAgentOptions) {
		o.SkipVersionCheck = skip
	}
}

// ===== Streaming =====

// WithIncludePartialMessages controls whether streaming deltas are emitted as
// StreamEvent messages. When false (default), only completed AssistantMessage
// and ResultMessage are emitted. When true, token-by-token deltas are emitted
// as StreamEvent with content_block_delta shape, followed by the completed
// AssistantMessage.
//
// Each StreamEvent's event.delta carries a "type" field that identifies the
// source of the chunk so consumers can route it appropriately:
//
//   - text_delta            — assistant prose (delta.text)
//   - thinking_delta        — model reasoning content (delta.thinking)
//   - command_output_delta  — shell stdout/stderr from a command_execution item
//     (delta.text; delta.item_id correlates back to the ToolUseBlock)
//   - file_change_delta     — diff output from a file_change item
//     (delta.text; delta.item_id correlates back to the ToolUseBlock)
//
// Consumers that render assistant prose should match on text_delta only and
// route command_output_delta / file_change_delta into the ToolUseBlock view
// rather than the assistant text stream.
func WithIncludePartialMessages(include bool) Option {
	return func(o *CodexAgentOptions) {
		o.IncludePartialMessages = include
	}
}

// ===== Session Metadata =====

// WithCodexHome overrides the Codex home directory (default ~/.codex).
// Used by StatSession to locate the session database.
func WithCodexHome(path string) Option {
	return func(o *CodexAgentOptions) {
		o.CodexHome = path
	}
}

// ===== Observability =====

// WithMeterProvider sets the OTel meter provider for recording SDK metrics.
// When not set, all metric recording is noop (zero-cost).
// The SDK depends on the OTel API only — callers supply their own MeterProvider.
func WithMeterProvider(mp metric.MeterProvider) Option {
	return func(o *CodexAgentOptions) {
		o.MeterProvider = mp
	}
}

// WithTracerProvider sets the OTel tracer provider for recording SDK spans.
// When not set, all trace recording is noop (zero-cost).
func WithTracerProvider(tp trace.TracerProvider) Option {
	return func(o *CodexAgentOptions) {
		o.TracerProvider = tp
	}
}

// WithPrometheusRegisterer configures a Prometheus registerer for SDK metrics.
// This is sugar: when set and WithMeterProvider is not, an OTel MeterProvider
// is created automatically from the registerer via the OTel→Prometheus bridge.
// If WithMeterProvider is also set, WithMeterProvider takes precedence.
func WithPrometheusRegisterer(reg prometheus.Registerer) Option {
	return func(o *CodexAgentOptions) {
		o.PrometheusRegisterer = reg
	}
}

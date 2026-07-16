package config

import (
	"fmt"

	sdkerrors "github.com/ethpandaops/codex-agent-sdk-go/internal/errors"
	"github.com/ethpandaops/codex-agent-sdk-go/internal/mcp"
)

// QueryBackend describes the built-in backend selected for one-shot Query.
type QueryBackend string

const (
	// QueryBackendExec uses `codex exec`.
	QueryBackendExec QueryBackend = "exec"
	// QueryBackendAppServer uses `codex app-server` through the adapter.
	QueryBackendAppServer QueryBackend = "app-server"
)

// SupportLevel describes how a backend handles an option.
type SupportLevel string

const (
	// SupportSupported means native/direct support.
	SupportSupported SupportLevel = "supported"
	// SupportEmulated means behavior is supported with constraints or emulation.
	SupportEmulated SupportLevel = "emulated"
	// SupportUnsupported means the option is not supported on that backend.
	SupportUnsupported SupportLevel = "unsupported"
)

// OptionCapability defines support by backend for one option field.
type OptionCapability struct {
	Field      string
	Exec       SupportLevel
	AppServer  SupportLevel
	Notes      string
	OptionName string
}

var optionCapabilities = []OptionCapability{
	{Field: "Logger", OptionName: "WithLogger", Exec: SupportSupported, AppServer: SupportSupported},
	{Field: "SystemPrompt", OptionName: "WithSystemPrompt", Exec: SupportUnsupported, AppServer: SupportSupported},
	{
		Field: "SystemPromptPreset", OptionName: "WithSystemPromptPreset", Exec: SupportUnsupported,
		AppServer: SupportEmulated, Notes: "emulated by mapping preset append text to developerInstructions",
	},
	{Field: "Model", OptionName: "WithModel", Exec: SupportSupported, AppServer: SupportSupported},
	{Field: "PermissionMode", OptionName: "WithPermissionMode", Exec: SupportSupported, AppServer: SupportSupported},
	{Field: "MaxTurns", OptionName: "WithMaxTurns", Exec: SupportSupported, AppServer: SupportSupported},
	{Field: "Cwd", OptionName: "WithCwd", Exec: SupportSupported, AppServer: SupportSupported},
	{Field: "CliPath", OptionName: "WithCliPath", Exec: SupportSupported, AppServer: SupportSupported},
	{Field: "Env", OptionName: "WithEnv", Exec: SupportSupported, AppServer: SupportSupported},
	{
		Field: "Hooks", OptionName: "WithHooks", Exec: SupportUnsupported,
		AppServer: SupportUnsupported, Notes: "hooks are a CLI-internal concept, not exposed over app-server protocol",
	},
	{Field: "Effort", OptionName: "WithEffort", Exec: SupportUnsupported, AppServer: SupportSupported},
	{Field: "Personality", OptionName: "WithPersonality", Exec: SupportUnsupported, AppServer: SupportSupported},
	{Field: "ServiceTier", OptionName: "WithServiceTier", Exec: SupportUnsupported, AppServer: SupportSupported},
	{Field: "DeveloperInstructions", OptionName: "WithDeveloperInstructions", Exec: SupportUnsupported, AppServer: SupportSupported},
	{Field: "MCPServers", OptionName: "WithMCPServers", Exec: SupportSupported, AppServer: SupportSupported},
	{Field: "SDKTools", OptionName: "WithSDKTools", Exec: SupportUnsupported, AppServer: SupportSupported},
	{Field: "CanUseTool", OptionName: "WithCanUseTool", Exec: SupportUnsupported, AppServer: SupportSupported},
	{Field: "OnUserInput", OptionName: "WithOnUserInput", Exec: SupportUnsupported, AppServer: SupportSupported},
	{Field: "OnElicitation", OptionName: "WithOnElicitation", Exec: SupportUnsupported, AppServer: SupportSupported},
	{
		Field: "Tools", OptionName: "WithTools", Exec: SupportUnsupported, AppServer: SupportEmulated,
		Notes: "emulated via SDK can_use_tool policy (not a native codex option)",
	},
	{
		Field: "AllowedTools", OptionName: "WithAllowedTools", Exec: SupportUnsupported,
		AppServer: SupportEmulated, Notes: "emulated via SDK can_use_tool policy (not a native codex option)",
	},
	{
		Field: "DisallowedTools", OptionName: "WithDisallowedTools", Exec: SupportUnsupported,
		AppServer: SupportEmulated, Notes: "emulated via SDK can_use_tool policy (not a native codex option)",
	},
	{
		Field: "PermissionPromptToolName", OptionName: "WithPermissionPromptToolName", Exec: SupportUnsupported,
		AppServer: SupportEmulated, Notes: "only \"stdio\" is supported in app-server mode",
	},
	{
		Field: "AddDirs", OptionName: "WithAddDirs", Exec: SupportSupported, AppServer: SupportUnsupported,
		Notes: "additional writable roots are not mapped in app-server mode",
	},
	{
		Field: "ExtraArgs", OptionName: "WithExtraArgs", Exec: SupportSupported, AppServer: SupportUnsupported,
		Notes: "app-server mode currently ignores extra CLI args",
	},
	{Field: "Stderr", OptionName: "WithStderr", Exec: SupportSupported, AppServer: SupportSupported},
	{
		Field: "ContinueConversation", OptionName: "WithContinueConversation", Exec: SupportUnsupported,
		AppServer: SupportEmulated, Notes: "requires WithResume in app-server mode",
	},
	{Field: "Resume", OptionName: "WithResume", Exec: SupportUnsupported, AppServer: SupportSupported},
	{Field: "ForkSession", OptionName: "WithForkSession", Exec: SupportUnsupported, AppServer: SupportSupported},
	{Field: "OutputFormat", OptionName: "WithOutputFormat", Exec: SupportUnsupported, AppServer: SupportSupported},
	{
		Field: "InitializeTimeout", OptionName: "WithInitializeTimeout", Exec: SupportUnsupported,
		AppServer: SupportSupported,
	},
	{Field: "Transport", OptionName: "WithTransport", Exec: SupportSupported, AppServer: SupportSupported},
	{Field: "Sandbox", OptionName: "WithSandbox", Exec: SupportSupported, AppServer: SupportSupported},
	{Field: "Images", OptionName: "WithImages", Exec: SupportSupported, AppServer: SupportSupported},
	{Field: "Config", OptionName: "WithConfig", Exec: SupportSupported, AppServer: SupportSupported},
	{Field: "OutputSchema", OptionName: "WithOutputSchema", Exec: SupportSupported, AppServer: SupportSupported},
	{
		Field: "SkipVersionCheck", OptionName: "WithSkipVersionCheck", Exec: SupportSupported,
		AppServer: SupportSupported,
	},
	{
		Field: "IncludePartialMessages", OptionName: "WithIncludePartialMessages", Exec: SupportUnsupported,
		AppServer: SupportSupported,
	},
	{
		Field: "CodexHome", OptionName: "WithCodexHome", Exec: SupportSupported,
		AppServer: SupportSupported, Notes: "only used by StatSession, no-op for Query/Client",
	},
	{
		Field: "MeterProvider", OptionName: "WithMeterProvider", Exec: SupportSupported,
		AppServer: SupportSupported, Notes: "SDK-local observability, no CLI interaction",
	},
	{
		Field: "TracerProvider", OptionName: "WithTracerProvider", Exec: SupportSupported,
		AppServer: SupportSupported, Notes: "SDK-local observability, no CLI interaction",
	},
	{
		Field: "PrometheusRegisterer", OptionName: "WithPrometheusRegisterer", Exec: SupportSupported,
		AppServer: SupportSupported, Notes: "SDK-local observability, creates MeterProvider from Prometheus registry",
	},
	{
		Field: "MetricsRecorder", OptionName: "initMetricsRecorder", Exec: SupportSupported,
		AppServer: SupportSupported, Notes: "SDK-internal, set at runtime by initMetricsRecorder",
	},
	{
		Field: "Observer", OptionName: "initMetricsRecorder", Exec: SupportSupported,
		AppServer: SupportSupported, Notes: "SDK-internal, set at runtime alongside MetricsRecorder",
	},
}

var optionCapabilityByField = func() map[string]OptionCapability {
	index := make(map[string]OptionCapability, len(optionCapabilities))
	for _, c := range optionCapabilities {
		index[c.Field] = c
	}

	return index
}()

// OptionCapabilities returns all option capabilities.
func OptionCapabilities() []OptionCapability {
	out := make([]OptionCapability, len(optionCapabilities))
	copy(out, optionCapabilities)

	return out
}

// EnabledOptionFields returns option fields explicitly set by the caller.
func EnabledOptionFields(opts *Options) map[string]bool {
	enabled := make(map[string]bool, len(optionCapabilities))
	if opts == nil {
		return enabled
	}

	set := func(field string, ok bool) {
		if ok {
			enabled[field] = true
		}
	}

	set("Logger", opts.Logger != nil)
	set("SystemPrompt", opts.SystemPrompt != "")
	set("SystemPromptPreset", opts.SystemPromptPreset != nil)
	set("Model", opts.Model != "")
	set("PermissionMode", opts.PermissionMode != "")
	set("MaxTurns", opts.MaxTurns > 0)
	set("Cwd", opts.Cwd != "")
	set("CliPath", opts.CliPath != "")
	set("Env", len(opts.Env) > 0)
	set("Hooks", len(opts.Hooks) > 0)
	set("Effort", opts.Effort != nil)
	set("Personality", opts.Personality != "")
	set("ServiceTier", opts.ServiceTier != "")
	set("DeveloperInstructions", opts.DeveloperInstructions != "")
	set("MCPServers", len(opts.MCPServers) > 0)
	set("SDKTools", len(opts.SDKTools) > 0)
	set("CanUseTool", opts.CanUseTool != nil)
	set("OnUserInput", opts.OnUserInput != nil)
	set("OnElicitation", opts.OnElicitation != nil)
	set("Tools", opts.Tools != nil)
	set("AllowedTools", len(opts.AllowedTools) > 0)
	set("DisallowedTools", len(opts.DisallowedTools) > 0)
	set("PermissionPromptToolName", opts.PermissionPromptToolName != "")
	set("AddDirs", len(opts.AddDirs) > 0)
	set("ExtraArgs", len(opts.ExtraArgs) > 0)
	set("Stderr", opts.Stderr != nil)
	set("ContinueConversation", opts.ContinueConversation)
	set("Resume", opts.Resume != "")
	set("ForkSession", opts.ForkSession)
	set("OutputFormat", opts.OutputFormat != nil)
	set("InitializeTimeout", opts.InitializeTimeout != nil)
	set("Transport", opts.Transport != nil)
	set("Sandbox", opts.Sandbox != "")
	set("Images", len(opts.Images) > 0)
	set("Config", len(opts.Config) > 0)
	set("OutputSchema", opts.OutputSchema != "")
	set("SkipVersionCheck", opts.SkipVersionCheck)
	set("IncludePartialMessages", opts.IncludePartialMessages)
	set("CodexHome", opts.CodexHome != "")
	set("MeterProvider", opts.MeterProvider != nil)
	set("TracerProvider", opts.TracerProvider != nil)
	set("PrometheusRegisterer", opts.PrometheusRegisterer != nil)

	return enabled
}

// SelectQueryBackend chooses which built-in backend Query should use.
func SelectQueryBackend(opts *Options) QueryBackend {
	if opts == nil {
		return QueryBackendExec
	}

	// SDK MCP servers require bidirectional callbacks and cannot be serialized
	// to exec CLI flags, so Query must run over app-server mode.
	if hasSDKMCPServers(opts.MCPServers) {
		return QueryBackendAppServer
	}

	enabled := EnabledOptionFields(opts)
	for field := range enabled {
		capability, ok := optionCapabilityByField[field]
		if !ok {
			return QueryBackendAppServer
		}

		if capability.Exec != SupportSupported {
			return QueryBackendAppServer
		}
	}

	return QueryBackendExec
}

// ValidateOptionsForBackend returns ErrUnsupportedOption if any set option is not
// supported for the selected backend.
func ValidateOptionsForBackend(opts *Options, backend QueryBackend) error {
	if opts == nil {
		return nil
	}

	if backend == QueryBackendExec && hasSDKMCPServers(opts.MCPServers) {
		return fmt.Errorf(
			"%w: option MCPServers (WithMCPServers) with SDK server type is unsupported on %s backend",
			sdkerrors.ErrUnsupportedOption,
			backend,
		)
	}

	enabled := EnabledOptionFields(opts)
	for field := range enabled {
		capability, ok := optionCapabilityByField[field]
		if !ok {
			return fmt.Errorf("%w: option %s has no capability classification", sdkerrors.ErrUnsupportedOption, field)
		}

		var level SupportLevel

		switch backend {
		case QueryBackendExec:
			level = capability.Exec
		case QueryBackendAppServer:
			level = capability.AppServer
		default:
			return fmt.Errorf("%w: unknown backend %q", sdkerrors.ErrUnsupportedOption, backend)
		}

		if level == SupportUnsupported {
			msg := fmt.Sprintf("option %s (%s) is unsupported on %s backend", field, capability.OptionName, backend)
			if capability.Notes != "" {
				msg = fmt.Sprintf("%s: %s", msg, capability.Notes)
			}

			return fmt.Errorf("%w: %s", sdkerrors.ErrUnsupportedOption, msg)
		}

		// Emulated options with additional constraints.
		if field == "ContinueConversation" && opts.ContinueConversation && opts.Resume == "" {
			return fmt.Errorf(
				"%w: option ContinueConversation (WithContinueConversation) requires WithResume on %s backend",
				sdkerrors.ErrUnsupportedOption,
				backend,
			)
		}

		if field == "PermissionPromptToolName" && opts.PermissionPromptToolName != "" &&
			opts.PermissionPromptToolName != "stdio" {
			return fmt.Errorf(
				"%w: option PermissionPromptToolName (WithPermissionPromptToolName) only supports value \"stdio\" on %s backend",
				sdkerrors.ErrUnsupportedOption,
				backend,
			)
		}
	}

	return nil
}

func hasSDKMCPServers(servers map[string]mcp.ServerConfig) bool {
	if len(servers) == 0 {
		return false
	}

	for _, server := range servers {
		if server == nil {
			continue
		}

		if server.GetType() == mcp.ServerTypeSDK {
			return true
		}
	}

	return false
}

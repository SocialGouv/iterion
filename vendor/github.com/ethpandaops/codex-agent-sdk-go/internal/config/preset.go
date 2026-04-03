package config

// Beta represents a beta feature flag for the SDK.
type Beta string

const (
	// BetaContext1M enables 1 million token context window.
	BetaContext1M Beta = "context-1m"
)

// SettingSource represents where settings should be loaded from.
type SettingSource string

const (
	// SettingSourceUser loads from user-level settings.
	SettingSourceUser SettingSource = "user"
	// SettingSourceProject loads from project-level settings.
	SettingSourceProject SettingSource = "project"
	// SettingSourceLocal loads from local-level settings.
	SettingSourceLocal SettingSource = "local"
)

// ToolsPreset represents a preset configuration for available tools.
type ToolsPreset struct {
	Type   string `json:"type"`   // "preset"
	Preset string `json:"preset"` // e.g. "codex_default"
}

// AgentDefinition defines a custom agent configuration.
type AgentDefinition struct {
	Description string   `json:"description"`
	Prompt      string   `json:"prompt"`
	Tools       []string `json:"tools,omitempty"`
	Model       *string  `json:"model,omitempty"`
}

// SystemPromptPreset defines a system prompt preset configuration.
type SystemPromptPreset struct {
	Type   string  `json:"type"`
	Preset string  `json:"preset"`
	Append *string `json:"append,omitempty"`
}

// PluginConfig configures a plugin to load.
type PluginConfig struct {
	Type string `json:"type"` // "local"
	Path string `json:"path"`
}

// ToolsConfig is an interface for configuring available tools.
// It represents either a list of tool names or a preset configuration.
type ToolsConfig interface {
	toolsConfig() // marker method
}

// ToolsList is a list of tool names to make available.
type ToolsList []string

func (ToolsList) toolsConfig() {}

func (*ToolsPreset) toolsConfig() {}

package runtime

import (
	"encoding/json"
	"github.com/SocialGouv/claw-code-go/internal/config"
	"os"
	"path/filepath"
)

const (
	DefaultModel     = "claude-sonnet-4-20250514"
	DefaultMaxTokens = 8096
)

// MCPServerConfig describes a single MCP server connection.
type MCPServerConfig struct {
	Name      string            `json:"name"`
	Transport string            `json:"transport"` // "stdio" or "sse"
	Command   string            `json:"command,omitempty"`
	Args      []string          `json:"args,omitempty"`
	URL       string            `json:"url,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
}

// Config holds runtime configuration for the CLI.
type Config struct {
	Model        string
	MaxTokens    int
	SystemPrompt string
	SessionDir   string
	APIKey       string
	BaseURL      string

	// Provider and auth fields (Phase 3).
	// ProviderName is one of: "anthropic", "bedrock", "vertex", "foundry".
	ProviderName string
	// AuthMethod is one of: "api_key", "oauth", "iam", "adc", "azure_identity".
	AuthMethod string
	// OAuthToken is the resolved OAuth access token (set at startup when using OAuth).
	OAuthToken string

	// MCPServers lists MCP server connections (Phase 4).
	MCPServers []MCPServerConfig

	// Compaction settings (Phase 6).
	// CompactionEnabled enables automatic session compaction (default: true).
	CompactionEnabled bool
	// CompactionThreshold is the fraction of MaxTokens at which compaction
	// triggers (e.g., 0.75 triggers at 75% of the token budget).
	CompactionThreshold float64
	// CompactionKeepRecent is the number of most-recent messages retained
	// verbatim after compaction.
	CompactionKeepRecent int

	// Permission settings (Phase 11).
	// PermissionMode is the active permission enforcement mode string.
	PermissionMode string
	// AllowedTools are tool names that are always allowed without prompting.
	AllowedTools []string
	// BlockedTools are tool names that are always denied without prompting.
	BlockedTools []string

	// Theme is the active TUI color theme ("dark" or "light").
	Theme string

	// Plugin settings.
	// PluginBundledRoot is the directory containing bundled plugins.
	PluginBundledRoot string
	// PluginInstallRoot is the directory where installed plugins are stored.
	PluginInstallRoot string
	// PluginExternalDirs are additional directories to scan for plugins.
	PluginExternalDirs []string
	// EnabledPlugins maps plugin IDs to their enabled state.
	EnabledPlugins map[string]bool

	// CLI behavior flags.
	// Compact enables compact output format.
	Compact bool
	// Verbose enables verbose output.
	Verbose bool
	// Quiet suppresses non-essential output.
	Quiet bool
	// NoSave disables session persistence.
	NoSave bool
	// Task is the task name for pre-configured task mode.
	Task string
	// BaseCommit is the base commit for diff context.
	BaseCommit string
	// ReasoningEffort is the reasoning effort level (low, medium, high).
	ReasoningEffort string
	// OutputFormat controls output mode: "text" (default), "json", "stream-json".
	// Maps to Rust's --output-format flag.
	OutputFormat string
}

// LoadConfig reads configuration from layered settings files and environment
// variables and applies defaults. Load order (later overrides earlier):
//  1. Defaults
//  2. Layered settings files (user global → project → local)
//  3. Environment variables
//  4. CLI flags (applied by the caller after this function returns)
func LoadConfig() *Config {
	cfg := &Config{
		Model:                DefaultModel,
		MaxTokens:            DefaultMaxTokens,
		PermissionMode:       "default",
		CompactionEnabled:    true,
		CompactionThreshold:  DefaultCompactionThreshold,
		CompactionKeepRecent: DefaultCompactionKeepRecent,
	}

	// Apply layered settings files (user global → project → local).
	s := config.Load()
	if s.Model != "" {
		cfg.Model = s.Model
	}
	if s.MaxTokens != 0 {
		cfg.MaxTokens = s.MaxTokens
	}
	if s.PermissionMode != "" {
		cfg.PermissionMode = s.PermissionMode
	}
	if len(s.AllowedTools) > 0 {
		cfg.AllowedTools = s.AllowedTools
	}
	if len(s.BlockedTools) > 0 {
		cfg.BlockedTools = s.BlockedTools
	}
	if s.Theme != "" {
		cfg.Theme = s.Theme
	}

	// Plugin configuration
	if s.EnabledPlugins != nil {
		cfg.EnabledPlugins = s.EnabledPlugins
	}

	// Environment variables override settings files.
	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		cfg.APIKey = key
	}
	if model := os.Getenv("ANTHROPIC_MODEL"); model != "" {
		cfg.Model = model
	}
	if baseURL := os.Getenv("ANTHROPIC_BASE_URL"); baseURL != "" {
		cfg.BaseURL = baseURL
	}

	// Default session dir: ~/.claw-code/sessions
	homeDir, err := os.UserHomeDir()
	if err == nil {
		cfg.SessionDir = filepath.Join(homeDir, ".claw-code", "sessions")
	} else {
		cfg.SessionDir = ".claw-code-sessions"
	}

	// Detect the active provider from environment variables.
	cfg.ProviderName = detectProvider()

	// Resolve provider-specific env vars for non-Anthropic providers.
	// These override the Anthropic defaults set above.
	switch cfg.ProviderName {
	case "xai":
		if key := os.Getenv("XAI_API_KEY"); key != "" {
			cfg.APIKey = key
		}
		if baseURL := os.Getenv("XAI_BASE_URL"); baseURL != "" {
			cfg.BaseURL = baseURL
		} else if cfg.BaseURL == "" {
			cfg.BaseURL = "https://api.x.ai/v1"
		}
	case "dashscope":
		if key := os.Getenv("DASHSCOPE_API_KEY"); key != "" {
			cfg.APIKey = key
		}
		if baseURL := os.Getenv("DASHSCOPE_BASE_URL"); baseURL != "" {
			cfg.BaseURL = baseURL
		} else if cfg.BaseURL == "" {
			cfg.BaseURL = "https://dashscope.aliyuncs.com/compatible-mode/v1"
		}
	case "openai":
		if key := os.Getenv("OPENAI_API_KEY"); key != "" {
			cfg.APIKey = key
		}
		if baseURL := os.Getenv("OPENAI_BASE_URL"); baseURL != "" {
			cfg.BaseURL = baseURL
		}
	}

	// Load MCP server configs.
	cfg.MCPServers = loadMCPServers(homeDir)

	return cfg
}

// loadMCPServers reads MCP server configurations from the settings file and
// the CLAUDE_MCP_SERVERS environment variable (JSON override, takes precedence).
func loadMCPServers(homeDir string) []MCPServerConfig {
	// Try env var override first.
	if raw := os.Getenv("CLAUDE_MCP_SERVERS"); raw != "" {
		var servers []MCPServerConfig
		if err := json.Unmarshal([]byte(raw), &servers); err == nil {
			return servers
		}
	}

	// Otherwise read from ~/.claude/settings.json.
	if homeDir == "" {
		return nil
	}
	settingsPath := filepath.Join(homeDir, ".claude", "settings.json")
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		return nil
	}

	var settings struct {
		MCPServers []MCPServerConfig `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		return nil
	}

	return settings.MCPServers
}

// detectProvider reads env vars to determine which provider to use.
// Precedence: bedrock > vertex > foundry > anthropic > xai > dashscope > openai.
// Anthropic is detected before xAI/DashScope to match Rust behavior and avoid
// surprising users who have multiple API keys set.
func detectProvider() string {
	switch {
	case os.Getenv("CLAUDE_CODE_USE_BEDROCK") == "1":
		return "bedrock"
	case os.Getenv("CLAUDE_CODE_USE_VERTEX") == "1":
		return "vertex"
	case os.Getenv("CLAUDE_CODE_USE_FOUNDRY") == "1":
		return "foundry"
	case os.Getenv("ANTHROPIC_API_KEY") != "" || os.Getenv("ANTHROPIC_AUTH_TOKEN") != "":
		return "anthropic"
	case os.Getenv("XAI_API_KEY") != "":
		return "xai"
	case os.Getenv("DASHSCOPE_API_KEY") != "":
		return "dashscope"
	case os.Getenv("OPENAI_API_KEY") != "":
		return "openai"
	default:
		return "anthropic"
	}
}

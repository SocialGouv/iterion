package runtime

import (
	"encoding/json"
	"fmt"
	"github.com/SocialGouv/claw-code-go/internal/config"
	clawctx "github.com/SocialGouv/claw-code-go/internal/context"
	"os"
	"path/filepath"
	"strings"
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
	Model string
	// OracleModel is the model the oracle tool consults (must live on the
	// session client's provider). Empty = the session model.
	OracleModel string
	MaxTokens   int
	// SystemPrompt, when set, REPLACES the authored base (identity sentence,
	// posture and behavioral sections). Context sections (environment, git,
	// CLAUDE.md, memory, compaction, MCP) still follow their toggles —
	// combine with the minimal-prompt mode to send the custom prompt alone.
	SystemPrompt string
	// AppendSystemPrompt is appended verbatim at the end of the assembled
	// system prompt (the claude CLI's --append-system-prompt semantics).
	AppendSystemPrompt string
	SessionDir         string
	APIKey             string
	BaseURL            string

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
	// AllowImmediateStructuredOutput disables the default requirement that a
	// work-capable session complete work before returning structured output.
	AllowImmediateStructuredOutput bool

	// Prompt holds the resolved system-prompt section toggles.
	// nil means "all defaults" (every section on) — callers constructing a
	// Config by hand keep today's behavior without setting anything.
	Prompt *PromptConfig
}

// PromptConfig holds the resolved (non-tri-state) system-prompt section
// toggles. The zero value disables everything; use DefaultPromptConfig for
// the all-on default.
type PromptConfig struct {
	Environment         bool
	GitStatus           bool
	ProjectInstructions bool
	McpTools            bool
	CompactionSummary   bool
	MemoryWalkUp        bool
	MemoryImports       bool
	AutoMemory          bool
	Posture             bool
	Communication       bool
	TaskManagement      bool
	DoingTasks          bool
	ToolPolicy          bool
	GitSafety           bool
	ContextManagement   bool
	// MemoryMaxBytes caps the combined injected memory content (0 = default).
	MemoryMaxBytes int
	// AutoMemoryDir overrides where the auto-memory section reads MEMORY.md
	// from (empty = derived from the working directory).
	AutoMemoryDir string
}

// DefaultPromptConfig returns the all-on default (Claude Code parity).
func DefaultPromptConfig() PromptConfig {
	return PromptConfig{
		Environment:         true,
		GitStatus:           true,
		ProjectInstructions: true,
		McpTools:            true,
		CompactionSummary:   true,
		MemoryWalkUp:        true,
		MemoryImports:       true,
		AutoMemory:          true,
		Posture:             true,
		Communication:       true,
		TaskManagement:      true,
		DoingTasks:          true,
		ToolPolicy:          true,
		GitSafety:           true,
		ContextManagement:   true,
	}
}

// MinimalPromptConfig returns the all-off preset (small-model mode): only the
// base identity sentence is sent, no automatic context sections.
func MinimalPromptConfig() PromptConfig {
	return PromptConfig{}
}

// AssembleOptions maps the resolved prompt config onto the context
// assembler's section toggles — the single place the mapping lives.
func (p PromptConfig) AssembleOptions() clawctx.AssembleOptions {
	return clawctx.AssembleOptions{
		Environment:         p.Environment,
		GitStatus:           p.GitStatus,
		ProjectInstructions: p.ProjectInstructions,
		AutoMemory:          p.AutoMemory,
		AutoMemoryDir:       p.AutoMemoryDir,
		Memory: clawctx.MemoryOptions{
			WalkUp:   p.MemoryWalkUp,
			Imports:  p.MemoryImports,
			MaxBytes: p.MemoryMaxBytes,
		},
	}
}

// ResolvePromptConfig resolves a tri-state settings block into a concrete
// PromptConfig: each section is its explicit value when set, else on unless
// "minimal" flips the default off.
func ResolvePromptConfig(p *config.RuntimePromptConfig) PromptConfig {
	if p == nil {
		return DefaultPromptConfig()
	}
	def := p.Minimal == nil || !*p.Minimal
	pick := func(v *bool) bool {
		if v != nil {
			return *v
		}
		return def
	}
	return PromptConfig{
		Environment:         pick(p.Environment),
		GitStatus:           pick(p.GitStatus),
		ProjectInstructions: pick(p.ProjectInstructions),
		McpTools:            pick(p.McpTools),
		CompactionSummary:   pick(p.CompactionSummary),
		MemoryWalkUp:        pick(p.MemoryWalkUp),
		MemoryImports:       pick(p.MemoryImports),
		AutoMemory:          pick(p.AutoMemory),
		Posture:             pick(p.Posture),
		Communication:       pick(p.Communication),
		TaskManagement:      pick(p.TaskManagement),
		DoingTasks:          pick(p.DoingTasks),
		ToolPolicy:          pick(p.ToolPolicy),
		GitSafety:           pick(p.GitSafety),
		ContextManagement:   pick(p.ContextManagement),
		MemoryMaxBytes:      p.MemoryMaxBytes,
	}
}

// promptSections is the single registry of toggleable sections: canonical
// (kebab-case) name + PromptConfig field accessor. Lookups normalize the
// input (lowercase, "-"/"_" stripped), so "git-status" == "gitStatus".
var promptSections = []struct {
	canonical string
	field     func(*PromptConfig) *bool
}{
	{"environment", func(p *PromptConfig) *bool { return &p.Environment }},
	{"git-status", func(p *PromptConfig) *bool { return &p.GitStatus }},
	{"project-instructions", func(p *PromptConfig) *bool { return &p.ProjectInstructions }},
	{"mcp-tools", func(p *PromptConfig) *bool { return &p.McpTools }},
	{"compaction-summary", func(p *PromptConfig) *bool { return &p.CompactionSummary }},
	{"memory-walk-up", func(p *PromptConfig) *bool { return &p.MemoryWalkUp }},
	{"memory-imports", func(p *PromptConfig) *bool { return &p.MemoryImports }},
	{"auto-memory", func(p *PromptConfig) *bool { return &p.AutoMemory }},
	{"posture", func(p *PromptConfig) *bool { return &p.Posture }},
	{"communication", func(p *PromptConfig) *bool { return &p.Communication }},
	{"task-management", func(p *PromptConfig) *bool { return &p.TaskManagement }},
	{"doing-tasks", func(p *PromptConfig) *bool { return &p.DoingTasks }},
	{"tool-policy", func(p *PromptConfig) *bool { return &p.ToolPolicy }},
	{"git-safety", func(p *PromptConfig) *bool { return &p.GitSafety }},
	{"context-management", func(p *PromptConfig) *bool { return &p.ContextManagement }},
}

func normalizePromptSection(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	name = strings.ReplaceAll(name, "-", "")
	return strings.ReplaceAll(name, "_", "")
}

// promptSectionField resolves a user-supplied section name to its
// PromptConfig field accessor.
func promptSectionField(name string) (func(*PromptConfig) *bool, bool) {
	normalized := normalizePromptSection(name)
	for _, s := range promptSections {
		if normalizePromptSection(s.canonical) == normalized {
			return s.field, true
		}
	}
	return nil, false
}

// PromptSectionNames returns the canonical section names accepted by
// ApplyPromptSectionOverrides (for help text and error messages).
func PromptSectionNames() []string {
	names := make([]string, len(promptSections))
	for i, s := range promptSections {
		names[i] = s.canonical
	}
	return names
}

// ApplyPromptSectionOverrides applies CLI/frontmatter-style section overrides
// onto cfg.Prompt. A non-empty `only` list enables exclusively the listed
// sections (implies minimal); `minimal` alone disables everything; `disable`
// turns the listed sections off, applied last. Section names are matched
// case-insensitively with "-"/"_" ignored. Returns an error naming the first
// unknown section.
func ApplyPromptSectionOverrides(cfg *Config, minimal bool, only, disable []string) error {
	base := DefaultPromptConfig()
	if cfg.Prompt != nil {
		base = *cfg.Prompt
	}
	if minimal || len(only) > 0 {
		keep := base.MemoryMaxBytes
		base = MinimalPromptConfig()
		base.MemoryMaxBytes = keep
	}
	set := func(names []string, val bool) error {
		for _, name := range names {
			field, ok := promptSectionField(name)
			if !ok {
				return fmt.Errorf("unknown prompt section %q (valid: %s)",
					name, strings.Join(PromptSectionNames(), ", "))
			}
			*field(&base) = val
		}
		return nil
	}
	if err := set(only, true); err != nil {
		return err
	}
	if err := set(disable, false); err != nil {
		return err
	}
	cfg.Prompt = &base
	return nil
}

// PromptOrDefault returns the resolved prompt config, defaulting to all-on
// when unset.
func (c *Config) PromptOrDefault() PromptConfig {
	if c != nil && c.Prompt != nil {
		return *c.Prompt
	}
	return DefaultPromptConfig()
}

// CustomSystemPrompt returns the replacement base prompt, if any (nil-safe).
func (c *Config) CustomSystemPrompt() string {
	if c == nil {
		return ""
	}
	return strings.TrimSpace(c.SystemPrompt)
}

// AppendedSystemPrompt returns the appended prompt suffix, if any (nil-safe).
func (c *Config) AppendedSystemPrompt() string {
	if c == nil {
		return ""
	}
	return strings.TrimSpace(c.AppendSystemPrompt)
}

// LoadConfig reads configuration from layered settings files and environment
// variables and applies defaults. Load order (later overrides earlier):
//  1. Defaults
//  2. Layered settings files (user global → project → local)
//  3. CLAUDE.md frontmatter overrides (cwd)
//  4. Environment variables
//  5. CLI flags (applied by the caller after this function returns)
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
	if s.OracleModel != "" {
		cfg.OracleModel = s.OracleModel
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
	if s.AllowImmediateStructuredOutput != nil {
		cfg.AllowImmediateStructuredOutput = *s.AllowImmediateStructuredOutput
	}

	// Plugin configuration
	if s.EnabledPlugins != nil {
		cfg.EnabledPlugins = s.EnabledPlugins
	}

	// Prompt section toggles (tri-state settings → resolved bools).
	promptCfg := ResolvePromptConfig(s.Prompt)
	cfg.Prompt = &promptCfg

	// CLAUDE.md frontmatter overrides (cwd; --work-dir was applied by the
	// caller via os.Chdir before LoadConfig). Sits between settings files
	// and environment variables.
	if cwd, err := os.Getwd(); err == nil {
		applyFrontmatter(cfg, config.LoadFrontmatterForDir(cwd))
	}

	// Environment variables override settings files.
	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		cfg.APIKey = key
	}
	if model := os.Getenv("ANTHROPIC_MODEL"); model != "" {
		cfg.Model = model
	}
	if model := os.Getenv("CLAW_ORACLE_MODEL"); model != "" {
		cfg.OracleModel = model
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

// applyFrontmatter overlays CLAUDE.md frontmatter overrides onto cfg.
func applyFrontmatter(cfg *Config, fm *config.FrontmatterConfig) {
	if fm == nil {
		return
	}
	if fm.Model != nil {
		cfg.Model = *fm.Model
	}
	if fm.PermissionMode != nil {
		cfg.PermissionMode = *fm.PermissionMode
	}
	if len(fm.AllowedTools) > 0 {
		cfg.AllowedTools = fm.AllowedTools
	}
	minimal := fm.MinimalPrompt != nil && *fm.MinimalPrompt
	if minimal || len(fm.PromptSections) > 0 || len(fm.DisablePromptSections) > 0 {
		if err := ApplyPromptSectionOverrides(cfg, minimal, fm.PromptSections, fm.DisablePromptSections); err != nil {
			fmt.Fprintf(os.Stderr, "config warning: CLAUDE.md frontmatter: %v\n", err)
		}
	}
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

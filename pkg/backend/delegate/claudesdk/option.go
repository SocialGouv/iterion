package claudesdk

import "encoding/json"

// Option configures a Session or Prompt call.
type Option func(*config)

// CanUseToolFunc is called when the CLI requests permission to use a tool.
// Return "allow", "deny", or "ask".
type CanUseToolFunc func(toolName string, input map[string]any) (string, error)

// MessageCallbackFunc is called for every NDJSON line received from the CLI,
// before the SDK processes it. msgType is the "type" field from the JSON line,
// and data is the full raw JSON of that line.
type MessageCallbackFunc func(msgType string, data json.RawMessage)

// ThinkingConfig configures extended thinking behavior.
type ThinkingConfig struct {
	Type            string `json:"type"`             // "enabled" or "disabled"
	BudgetTokens    int    `json:"budget_tokens"`    // max tokens for thinking
	TemperatureMode string `json:"temperature_mode"` // "default" or "custom"
}

// SettingSource specifies which settings to load.
type SettingSource string

const (
	SettingSourceUser    SettingSource = "user"
	SettingSourceProject SettingSource = "project"
	SettingSourceLocal   SettingSource = "local"
)

// PluginConfig defines a plugin directory or configuration.
type PluginConfig struct {
	Dir string `json:"dir"`
}

// config holds all configuration options applied via functional options.
type config struct {
	model              string
	systemPrompt       string
	appendSystemPrompt string
	cwd                string
	cliPath            string
	env                map[string]string
	verbose            bool

	// Tool & permission control
	allowedTools    []string
	disallowedTools []string
	permissionMode  string
	canUseTool      CanUseToolFunc

	// Limits
	maxTurns          int
	maxBudgetUSD      float64
	maxThinkingTokens int

	// Streaming
	includePartialMessages bool

	// Extensions
	mcpServers map[string]MCPServerConfig
	hooks      map[HookEvent][]HookMatcher
	agents     map[string]AgentDefinition
	plugins    []PluginConfig

	// Session
	resume               string
	forkSession          bool
	continueConversation bool
	noSessionPersistence bool

	// Advanced
	outputFormat    map[string]any
	thinking        *ThinkingConfig
	settingSources  []SettingSource
	stderrCallback  func(string)
	messageCallback MessageCallbackFunc
	addDirs         []string
}

// WithModel sets the Claude model (e.g. "claude-sonnet-4-6", "claude-opus-4-6").
func WithModel(model string) Option {
	return func(c *config) { c.model = model }
}

// WithSystemPrompt replaces the system prompt entirely.
func WithSystemPrompt(prompt string) Option {
	return func(c *config) { c.systemPrompt = prompt }
}

// WithAppendSystemPrompt appends to the default system prompt.
func WithAppendSystemPrompt(prompt string) Option {
	return func(c *config) { c.appendSystemPrompt = prompt }
}

// WithCwd sets the working directory for the CLI subprocess.
func WithCwd(dir string) Option {
	return func(c *config) { c.cwd = dir }
}

// WithCLIPath sets an explicit path to the claude binary.
func WithCLIPath(path string) Option {
	return func(c *config) { c.cliPath = path }
}

// WithEnv adds an environment variable to the CLI subprocess.
func WithEnv(key, value string) Option {
	return func(c *config) {
		if c.env == nil {
			c.env = make(map[string]string)
		}
		c.env[key] = value
	}
}

// WithVerbose enables verbose CLI output (turn-by-turn).
func WithVerbose(v bool) Option {
	return func(c *config) { c.verbose = v }
}

// WithAllowedTools sets tools that auto-approve (supports glob patterns).
func WithAllowedTools(tools ...string) Option {
	return func(c *config) { c.allowedTools = append(c.allowedTools, tools...) }
}

// WithDisallowedTools removes tools from the model context entirely.
func WithDisallowedTools(tools ...string) Option {
	return func(c *config) { c.disallowedTools = append(c.disallowedTools, tools...) }
}

// WithPermissionMode sets the permission mode ("default", "acceptEdits", "bypassPermissions", "plan").
func WithPermissionMode(mode string) Option {
	return func(c *config) { c.permissionMode = mode }
}

// WithCanUseTool sets a callback for tool permission requests.
func WithCanUseTool(fn CanUseToolFunc) Option {
	return func(c *config) { c.canUseTool = fn }
}

// WithMaxTurns limits the number of agentic turns.
func WithMaxTurns(n int) Option {
	return func(c *config) { c.maxTurns = n }
}

// WithMaxBudgetUSD sets the maximum dollar spend before stopping.
func WithMaxBudgetUSD(budget float64) Option {
	return func(c *config) { c.maxBudgetUSD = budget }
}

// WithMaxThinkingTokens sets the maximum thinking token budget.
func WithMaxThinkingTokens(n int) Option {
	return func(c *config) { c.maxThinkingTokens = n }
}

// WithIncludePartialMessages enables token-level stream events.
func WithIncludePartialMessages(enabled bool) Option {
	return func(c *config) { c.includePartialMessages = enabled }
}

// WithMCPServer registers an MCP server with the given name.
func WithMCPServer(name string, srv MCPServerConfig) Option {
	return func(c *config) {
		if c.mcpServers == nil {
			c.mcpServers = make(map[string]MCPServerConfig)
		}
		c.mcpServers[name] = srv
	}
}

// WithHook registers a lifecycle hook for the given event.
func WithHook(event HookEvent, matcher HookMatcher) Option {
	return func(c *config) {
		if c.hooks == nil {
			c.hooks = make(map[HookEvent][]HookMatcher)
		}
		c.hooks[event] = append(c.hooks[event], matcher)
	}
}

// WithAgent registers a subagent definition.
func WithAgent(name string, def AgentDefinition) Option {
	return func(c *config) {
		if c.agents == nil {
			c.agents = make(map[string]AgentDefinition)
		}
		c.agents[name] = def
	}
}

// WithPlugins adds plugin directories.
func WithPlugins(plugins ...PluginConfig) Option {
	return func(c *config) { c.plugins = append(c.plugins, plugins...) }
}

// WithResume sets a session ID to resume.
func WithResume(sessionID string) Option {
	return func(c *config) { c.resume = sessionID }
}

// WithForkSession forks from the resume point instead of continuing.
func WithForkSession(fork bool) Option {
	return func(c *config) { c.forkSession = fork }
}

// WithContinueConversation continues the most recent conversation.
func WithContinueConversation(cont bool) Option {
	return func(c *config) { c.continueConversation = cont }
}

// WithNoSessionPersistence prevents saving session to disk.
func WithNoSessionPersistence(v bool) Option {
	return func(c *config) { c.noSessionPersistence = v }
}

// WithOutputFormat requires structured output matching the given JSON schema.
func WithOutputFormat(schema map[string]any) Option {
	return func(c *config) { c.outputFormat = schema }
}

// WithThinking configures extended thinking.
func WithThinking(cfg ThinkingConfig) Option {
	return func(c *config) { c.thinking = &cfg }
}

// WithSettingSources controls which settings sources to load.
func WithSettingSources(sources ...SettingSource) Option {
	return func(c *config) { c.settingSources = sources }
}

// WithStderrCallback sets a function to receive stderr output from the CLI.
func WithStderrCallback(fn func(string)) Option {
	return func(c *config) { c.stderrCallback = fn }
}

// WithMessageCallback sets a function that is called for every NDJSON line
// received from the CLI subprocess, before the SDK processes it. This enables
// callers to observe raw protocol messages for logging or diagnostics.
func WithMessageCallback(fn MessageCallbackFunc) Option {
	return func(c *config) { c.messageCallback = fn }
}

// WithAddDirs adds additional working directories.
func WithAddDirs(dirs ...string) Option {
	return func(c *config) { c.addDirs = append(c.addDirs, dirs...) }
}

// applyOptions creates a config with the given options applied.
func applyOptions(opts []Option) *config {
	c := &config{}
	for _, opt := range opts {
		opt(c)
	}
	return c
}
